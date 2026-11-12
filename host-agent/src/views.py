"""Show the existing Codex session in its project's Herdr workspace."""
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time


class HerdrError(RuntimeError):
    def __init__(self, message, code=None):
        super().__init__(message)
        self.code = code


def task_title(record):
    # Historical journals may predate titles; never invent one from the prompt.
    return record.get("request", {}).get("title") or "未命名任务"


class HerdrViews:
    def __init__(self, settings, runner=None):
        self.options = settings.get("herdr", {})
        self.socket = settings["socket"]
        self.directory = settings["host_state"] / "herdr"
        self.runner = runner or subprocess.run
        self.clock = time.monotonic
        self.sleep = time.sleep

    def call(self, *arguments):
        command = [self.options.get("binary", "herdr"), "--session",
                   self.options.get("session", "default"), *arguments]
        try:
            result = self.runner(command, capture_output=True, text=True, timeout=15)
        except (OSError, subprocess.SubprocessError) as error:
            raise HerdrError(str(error)) from error
        if result.returncode:
            message = (result.stderr or result.stdout).strip() or "Herdr command failed"
            code = None
            try:
                code = json.loads(message)["error"]["code"]
            except (ValueError, KeyError, TypeError):
                pass
            raise HerdrError(message, code=code)
        try:
            return json.loads(result.stdout)["result"]
        except (ValueError, KeyError) as error:
            raise HerdrError("Invalid Herdr response") from error

    def save(self, path, value):
        self.directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        with tempfile.NamedTemporaryFile(mode="w", dir=self.directory, delete=False) as stream:
            temporary = Path(stream.name)
            json.dump(value, stream, ensure_ascii=False)
        try:
            temporary.replace(path)
        finally:
            temporary.unlink(missing_ok=True)

    def is_attached(self, view, thread_id):
        info = self.call("pane", "process-info", "--pane", view["pane_id"])
        for process in info["process_info"]["foreground_processes"]:
            argv = process.get("argv", [])
            if "--remote" in argv and "resume" in argv:
                remote = argv[argv.index("--remote") + 1:][:1]
                if (remote and remote[0].startswith("unix://") and
                        Path(remote[0][len("unix://"):]).resolve() == self.socket.resolve() and
                        argv[argv.index("resume") + 1:][:1] == [thread_id]):
                    return True
        return False

    def attach(self, record, project):
        if not self.options.get("enabled", False):
            return None
        thread_id = record["thread_id"]
        if not re.fullmatch(r"[A-Za-z0-9-]{1,128}", thread_id):
            raise HerdrError("Invalid thread ID for Herdr view")
        path = self.directory / (thread_id + ".json")
        title = task_title(record)
        if path.exists():
            view = json.loads(path.read_text())
            panes = self.call("pane", "list")["panes"]
            exists = any(pane.get("pane_id") == view["pane_id"] for pane in panes)
            attached = exists and self.is_attached(view, thread_id)
            if attached:
                self.call("tab", "rename", view["tab_id"], title)
                view.update(state="attached", task_id=record["id"])
                self.save(path, view)
                return view
            if exists and view["state"] == "launching":
                raise HerdrError(f"Herdr launch is unconfirmed; inspect pane {view['pane_id']} before retrying")

        workspaces = self.call("workspace", "list")["workspaces"]
        panes = self.call("pane", "list")["panes"]
        matching = {pane["workspace_id"] for pane in panes if pane.get("cwd") and
                    Path(pane["cwd"]).expanduser().resolve() == project["path"].resolve()}
        candidates = [workspace for workspace in workspaces if workspace["workspace_id"] in matching]
        named = [workspace for workspace in candidates if workspace.get("label") == record["project"]]
        candidates = named or candidates
        if len(candidates) > 1:
            raise HerdrError("Multiple Herdr workspaces match the project directory; use a unique project label")
        if candidates:
            workspace_id = candidates[0]["workspace_id"]
            created = self.call("tab", "create", "--workspace", workspace_id,
                                "--cwd", str(project["path"]), "--label", title, "--no-focus")
        else:
            created = self.call("workspace", "create", "--cwd", str(project["path"]),
                                "--label", record["project"], "--no-focus")
            workspace_id = created["workspace"]["workspace_id"]
            self.call("tab", "rename", created["tab"]["tab_id"], title)
        view = {"workspace_id": workspace_id, "tab_id": created["tab"]["tab_id"],
                "pane_id": created["root_pane"]["pane_id"], "thread_id": thread_id,
                "task_id": record["id"], "state": "launching"}
        # Persist before launching: an uncertain CLI reply must not create a
        # second viewer or send the task prompt again.
        self.save(path, view)
        # Remote resume inherits the session's model and permissions. Explicit
        # cwd is required when tui.resume_cwd=current; never submit a prompt.
        native = ["--remote", "unix://" + str(self.socket), "resume", thread_id,
                  "--cd", str(project["path"]),
                  # Remote viewers can retain activity after turn/completed.
                  # Herdr prioritizes that spinner over the ready prompt, so
                  # let its screen rules detect work and use a static idle title.
                  "-c", 'tui.terminal_title=["thread-name","project-name"]']
        name = "mac-" + re.sub(r"[^a-z0-9_-]", "-", record["project"].lower())[:12] + "-" + thread_id[-8:]
        # Interactive shell hooks (for example Conda) can take tens of seconds
        # before the queued viewer command reaches its prompt.
        deadline = self.clock() + 60
        while True:
            try:
                self.call("agent", "start", name, "--kind", "codex", "--pane", view["pane_id"],
                          "--timeout", "4000", "--", *native)
                break
            except HerdrError as error:
                # A new pane can exist before its shell prompt is ready. This
                # specific rejection occurs before Herdr submits the command.
                if error.code == "agent_pane_busy" and self.clock() < deadline:
                    self.sleep(0.25)
                    continue
                # Other failures may follow command submission. Wait for the
                # exact TUI process; never resend an uncertain launch. Agent
                # detection can also briefly misclassify the starting TUI.
                while not self.is_attached(view, thread_id):
                    if self.clock() >= deadline:
                        raise
                    self.sleep(0.25)
                break
        view["state"] = "attached"
        self.save(path, view)
        return view
