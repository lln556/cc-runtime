"""File-mailbox client and serial macOS Codex worker."""
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time
import uuid

from rpc import CodexRPC, RpcError
from storage import Mailbox
from views import HerdrError, HerdrViews

ROOT = Path(__file__).resolve().parents[1]
TERMINAL = {"completed", "failed", "cancelled", "interrupted"}
APPROVAL_METHODS = {
    "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
    "item/permissions/requestApproval", "execCommandApproval", "applyPatchApproval",
}
INPUT_METHODS = {"item/tool/requestUserInput", "mcpServer/elicitation/request"}


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def atomic(path, value, *, root=None):
    if root is not None:
        return Mailbox(root).atomic(path, value)
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                     prefix=".tmp-", delete=False) as stream:
        temporary = Path(stream.name)
        try:
            json.dump(value, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    try:
        temporary.replace(path)
    finally:
        temporary.unlink(missing_ok=True)


def read(path, *, root=None):
    if root is not None:
        return Mailbox(root).read(path)
    path = Path(path)
    if path.is_symlink():
        raise ValueError(f"Refusing symlink: {path}")
    if path.stat().st_size > 4 * 1024 * 1024:
        raise ValueError(f"JSON file too large: {path}")
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError(f"Expected a JSON object: {path}")
    return value


def task_id(value):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-f]{32}", value):
        raise ValueError("Invalid task ID")
    return value


def git_head(path):
    return subprocess.check_output(
        ["git", "-C", str(path), "rev-parse", "HEAD"], text=True,
        stderr=subprocess.DEVNULL, timeout=10).strip()


def git_root(path):
    return Path(subprocess.check_output(
        ["git", "-C", str(path), "rev-parse", "--show-toplevel"], text=True,
        stderr=subprocess.DEVNULL, timeout=10).strip()).resolve()


def project_paths(projects):
    return {name: [str(Path(project["path"]).resolve()), *project.get("client_paths", [])]
            for name, project in projects.items()}


def select_project(paths, caller_repo, explicit=None):
    # Match the Git root exactly: a nested repository must not inherit its
    # parent's project, and identical commits do not establish project identity.
    matches = [name for name, roots in paths.items() if str(caller_repo) in roots]
    if not matches:
        raise ValueError(f"No project mapping for repository {caller_repo}; configure its host path and client_paths")
    if len(matches) != 1:
        raise ValueError(f"Ambiguous project mapping for {caller_repo}: {', '.join(matches)}")
    if explicit is not None and explicit != matches[0]:
        raise ValueError(f"--project {explicit} does not match repository {caller_repo} (project {matches[0]})")
    return matches[0]


def config(path):
    path = Path(path).expanduser().resolve()
    data = read(path)
    for key in ("model", "effort"):
        value = data.get(key)
        if value is not None and (not isinstance(value, str) or not value.strip()):
            raise ValueError(f"{key} must be a nonempty string or null")
    if data.get("approval_policy", "on-request") not in {"on-request", "never"}:
        raise ValueError("approval_policy must be on-request or never")
    for key in ("socket", "mailbox", "host_state", "instructions"):
        item = Path(data[key]).expanduser()
        item = item if item.is_absolute() else path.parent / item
        data[key] = item.parent.resolve() / item.name if key == "mailbox" else item.resolve()
    local_instructions = data.get("local_instructions", "execution.local.md")
    if local_instructions is not None:
        if not isinstance(local_instructions, str) or not local_instructions.strip():
            raise ValueError("local_instructions must be a nonempty string or null")
        item = Path(local_instructions).expanduser()
        data["local_instructions"] = item if item.is_absolute() else path.parent / item
    else:
        data["local_instructions"] = None
    if data["host_state"].is_relative_to(data["mailbox"]):
        raise ValueError("Host decisions must be outside the shared mailbox")
    for project in data["projects"].values():
        item = Path(project["path"]).expanduser()
        project["path"] = (item if item.is_absolute() else path.parent / item).resolve()
        aliases = project.get("client_paths", [])
        if (not isinstance(aliases, list) or any(
                not isinstance(alias, str) or not Path(alias).is_absolute() for alias in aliases)):
            raise ValueError("Project client_paths must be a list of absolute repository paths")
        project["client_paths"] = [os.path.normpath(alias) for alias in aliases]
        if project.get("sandbox", "workspace-write") not in {"workspace-write", "read-only", "danger-full-access"}:
            raise ValueError("Project sandbox must be workspace-write, read-only or danger-full-access")
    if not data["projects"]:
        raise ValueError("Configure at least one project")
    return data


def check_request(request, projects):
    task_id(request["id"])
    if request.get("version") != 1 or not isinstance(request.get("project"), str) or request["project"] not in projects:
        raise ValueError("Unsupported request version or unknown project")
    if not isinstance(request["task"], str) or not 1 <= len(request["task"]) <= 64000:
        raise ValueError("Task must contain 1–64000 characters")
    if (not isinstance(request.get("title"), str) or
            not 1 <= len(request["title"].strip()) <= 5 or
            any(ord(char) < 32 or ord(char) == 127 for char in request["title"])):
        raise ValueError("Provide --title with a meaningful 1–5 character task name without control characters")
    if not isinstance(request.get("authorization"), str) or len(request["authorization"]) > 8000:
        raise ValueError("Invalid authorization description")
    if not isinstance(request.get("expected_commit"), str) or not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", request["expected_commit"]):
        raise ValueError("Provide a full Git commit hash")
    if request.get("after"):
        task_id(request["after"])


def submit(mailbox, project, text, commit, authorization, after=None, title=None):
    capabilities = read(mailbox / "capabilities.json", root=mailbox)
    caller_repo = git_root(Path.cwd())
    project = select_project(capabilities.get("project_paths", {}), caller_repo, project)
    request = {"version": 1, "id": uuid.uuid4().hex, "project": project,
               "caller_repo": str(caller_repo),
               "task": text, "expected_commit": commit, "authorization": authorization,
               "after": after, "created_at": now()}
    if title is not None:
        request["title"] = title.strip()
    check_request(request, capabilities["projects"])
    atomic(mailbox / "inbox" / (request["id"] + ".json"), request, root=mailbox)
    return {"task_id": request["id"], "state": "queued"}


def control(mailbox, identifier, kind, text=None):
    files = Mailbox(mailbox)
    identifier = task_id(identifier)
    job = mailbox / "jobs" / identifier
    queued = mailbox / "inbox" / (identifier + ".json")
    if not files.exists(queued) and not files.exists(job / "status.json"):
        raise ValueError("Unknown task")
    if files.exists(job / "status.json"):
        state = files.read(job / "status.json")["state"]
        if state in TERMINAL:
            raise ValueError("Task has ended; submit a new task with --after to continue")
    elif kind == "message":
        raise ValueError("Task is still queued; wait until it is running")
    event = {"id": uuid.uuid4().hex, "kind": kind, "text": text, "created_at": now()}
    files.atomic(mailbox / "controls" / identifier / (event["id"] + ".json"), event)
    return {"task_id": identifier, "control_id": event["id"], "state": "queued"}


class Worker:
    def __init__(self, settings, rpc_factory=CodexRPC):
        self.config = settings
        self.mailbox = settings["mailbox"]
        self.files = Mailbox(self.mailbox)
        self.private = settings["host_state"]
        self.rpc_factory = rpc_factory
        self.stop = False
        self.probed = False
        self.tools = {}
        self.probe_error = None

    def prepare(self):
        self.private.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.private.chmod(0o700)
        with self.files.directory(self.mailbox, create=True) as directory:
            os.fchmod(directory, 0o700)
        for name in ("inbox", "jobs", "controls"):
            self.files.mkdir(self.mailbox / name)
        for path in [self.private / "jobs", self.private / "decisions"]:
            path.mkdir(parents=True, exist_ok=True)
        if not self.files.exists(self.mailbox / "capabilities.json"):
            self.heartbeat("offline")

    def heartbeat(self, state="ready", active=None):
        self.files.atomic(self.mailbox / "capabilities.json", {
            "version": 1, "updated_at": now(), "worker_state": state,
            "heartbeat_timeout_seconds": 90,
            "active_task": active,
            "projects": list(self.config["projects"]),
            "project_paths": project_paths(self.config["projects"]),
            "service_connected": self.probed,
            "computer_use": "advertised" if "cua_repl" in self.tools else "unverified",
            "note": "Tool discovery does not prove app permissions or browser availability.",
            "probe_error": self.probe_error,
        })

    def probe(self):
        try:
            with self.rpc_factory(self.config["socket"]) as rpc:
                result = rpc.call("mcpServerStatus/list", {})
                self.tools = {item["name"]: list(item.get("tools", {}))
                              for item in result.get("data", [])}
            self.probed, self.probe_error = True, None
        except (OSError, RpcError) as error:
            self.probed, self.probe_error = False, str(error)
        self.heartbeat("offline")  # A successful probe is not a running worker.

    def save(self, record, state=None, **values):
        record.update(values)
        if state:
            record["state"] = state
        record["updated_at"] = now()
        identifier = record["id"]
        # Authoritative host journal is never mounted into the container.
        atomic(self.private / "jobs" / (identifier + ".json"), record)
        public = {k: v for k, v in record.items() if k not in {"request", "execution_config"}}
        self.files.atomic(self.mailbox / "jobs" / identifier / "status.json", public)

    def finish(self, record, state, text="", error=None):
        self.files.atomic(self.mailbox / "jobs" / record["id"] / "result.json", {
            "task_id": record["id"], "state": state, "text": text, "error": error,
            "thread_id": record.get("thread_id"), "turn_id": record.get("turn_id"),
            "finished_at": now(),
        })
        self.save(record, state, pending=None, error=error)
        # A message racing with completion must get an explicit rejection.
        for path, event in self.controls(record):
            self.ack(record, path, event, "rejected", "Task has ended")

    def controls(self, record):
        directory = self.mailbox / "controls" / record["id"]
        for path in self.files.json_files(directory):
            try:
                event = self.files.read(path)
                task_id(event["id"])
                if path.stem != event["id"] or event["kind"] not in {"cancel", "message"}:
                    raise ValueError("Invalid control")
                yield path, event
            except (ValueError, KeyError, OSError):
                self.files.invalidate(path)

    def ack(self, record, path, event, state, error=None):
        self.files.atomic(self.mailbox / "jobs" / record["id"] / "controls" / (event["id"] + ".json"),
               {"control_id": event["id"], "state": state, "error": error})
        self.files.unlink(path)

    def reconcile(self, record):
        """After a disconnect/crash, stop the old turn; never replay its prompt."""
        if not record.get("thread_id"):
            self.finish(record, "interrupted", error="Worker stopped before starting a turn")
            return True
        try:
            with self.rpc_factory(self.config["socket"]) as rpc:
                thread = rpc.call("thread/read", {"threadId": record["thread_id"],
                                                  "includeTurns": True})["thread"]
                turns = thread.get("turns", [])
                owned = [turn for turn in turns if turn["id"] == record.get("turn_id") or any(
                    item.get("type") == "userMessage" and item.get("clientId") == record["id"]
                    for item in turn.get("items", [])
                )]
                if not owned and (thread.get("status", {}).get("type") == "active" or any(
                    turn.get("status") == "inProgress" for turn in turns
                )):
                    raise RpcError("Cannot identify this task's running turn; refusing to interrupt another client's work")
                for turn in owned:
                    if turn.get("status") in {"completed", "failed", "interrupted"}:
                        self.finish(record, turn["status"], "\n\n".join(
                            item.get("text", "") for item in turn.get("items", [])
                            if item.get("type") == "agentMessage"
                        ), turn.get("error"))
                        return True
                    if turn.get("status") == "inProgress":
                        rpc.call("turn/interrupt", {"threadId": record["thread_id"], "turnId": turn["id"]})
                        deadline = time.monotonic() + 15
                        while time.monotonic() < deadline:
                            event = rpc.receive(1)
                            if event and event.get("method") == "turn/completed" and event.get("params", {}).get("turn", {}).get("id") == turn["id"]:
                                break
                            latest = rpc.call("thread/read", {"threadId": record["thread_id"], "includeTurns": True})["thread"]
                            matching = [t for t in latest.get("turns", []) if t["id"] == turn["id"]]
                            if matching and matching[0].get("status") != "inProgress":
                                break
                        else:
                            raise RpcError("Could not confirm the interrupted turn stopped")
            cause = record.get("error")
            detail = "Execution was interrupted; inspect its thread before retrying"
            self.finish(record, "interrupted", error=f"{cause}; {detail}" if cause else detail)
            return True
        except (OSError, RpcError, KeyError) as error:
            self.save(record, "unknown", error=str(error))
            return False

    def recover(self):
        for path in sorted((self.private / "jobs").glob("*.json")):
            record = read(path)
            if record["state"] not in TERMINAL and not self.reconcile(record):
                self.heartbeat("blocked", record["id"])
                return False
        return True

    def handle_request(self, rpc, record, event):
        method = event["method"]
        if method in APPROVAL_METHODS | INPUT_METHODS:
            if record.get("pending"):
                raise RpcError("Multiple simultaneous user requests; stopping rather than losing one")
            token = uuid.uuid4().hex
            self.save(record, "waiting_for_approval" if method in APPROVAL_METHODS else "waiting_for_input",
                      pending={"token": token, "rpc_id": event["id"], "method": method,
                               "params": event.get("params", {})})
        else:
            rpc.send({"id": event["id"], "error": {"code": -32601,
                      "message": "Host bridge does not implement this client tool"}})
            raise RpcError(f"Unsupported client request: {method}")

    def decisions(self, rpc, record):
        pending = record.get("pending")
        if not pending:
            return
        path = self.private / "decisions" / (pending["token"] + ".json")
        if not path.exists():
            return
        response = read(path)
        if response.get("task_id") != record["id"]:
            raise ValueError("Decision belongs to another task")
        rpc.send({"id": pending["rpc_id"], "result": response["result"]})
        path.unlink()
        self.save(record, "running", pending=None)

    def monitor(self, rpc, record):
        job = self.mailbox / "jobs" / record["id"]
        with self.files.event_log(job / "events.jsonl") as events:
            self._monitor(rpc, record, events)

    def _monitor(self, rpc, record, events):
        deadline = time.monotonic() + self.config.get("task_timeout_seconds", 3600)
        cancel_deadline = None
        cancellation_reason = None
        messages = []
        while True:
            self.heartbeat("busy", record["id"])
            for path, event in self.controls(record):
                if event["kind"] == "cancel":
                    cancellation_reason = "cancelled"
                    self.ack(record, path, event, "accepted")
                elif cancel_deadline:
                    self.ack(record, path, event, "rejected", "Task is cancelling")
                else:
                    try:
                        rpc.call("turn/steer", {"threadId": record["thread_id"],
                            "expectedTurnId": record["turn_id"],
                            "input": [{"type": "text", "text": event["text"]}]})
                        self.ack(record, path, event, "delivered")
                    except RpcError as error:
                        self.ack(record, path, event, "rejected", str(error))
            self.decisions(rpc, record)
            if self.stop or time.monotonic() >= deadline:
                cancellation_reason = cancellation_reason or "interrupted"
            if cancellation_reason and cancel_deadline is None:
                rpc.call("turn/interrupt", {"threadId": record["thread_id"], "turnId": record["turn_id"]})
                self.save(record, "cancelling", pending=None)
                cancel_deadline = time.monotonic() + 20
            if cancel_deadline and time.monotonic() >= cancel_deadline:
                raise RpcError("Cancellation was sent, but completion was not confirmed")
            event = rpc.receive(1)
            if not event:
                continue
            params = event.get("params", {})
            if params.get("threadId") not in (None, record["thread_id"]):
                continue
            if event.get("method", "").startswith("codex/event/"):
                continue  # Legacy duplicate stream; persist the v2 events only.
            self.files.append_event(events, event)
            method = event.get("method")
            if "id" in event and method:
                self.handle_request(rpc, record, event)
            elif method == "serverRequest/resolved" and record.get("pending"):
                if params.get("requestId") == record["pending"]["rpc_id"]:
                    self.save(record, "running", pending=None)
            elif method == "item/completed":
                item = params.get("item", {})
                if item.get("type") == "agentMessage":
                    messages.append(item.get("text", ""))
            elif method == "turn/completed" and params.get("turn", {}).get("id") == record["turn_id"]:
                turn = params["turn"]
                state = {"completed": "completed", "failed": "failed", "interrupted": "interrupted"}.get(turn["status"], "failed")
                if state == "interrupted" and cancellation_reason:
                    state = cancellation_reason
                self.finish(record, state, "\n\n".join(messages), turn.get("error"))
                return

    def load_instructions(self):
        base = Path(self.config["instructions"])
        if base.is_symlink():
            raise ValueError(f"Refusing symlink: {base}")
        text = base.read_text()
        extra_path = self.config.get("local_instructions", base.parent / "config/execution.local.md")
        if extra_path is not None:
            extra_path = Path(extra_path)
            if extra_path.is_symlink():
                raise ValueError(f"Refusing symlink: {extra_path}")
            if extra_path.exists():
                extra = extra_path.read_text().strip()
                if extra:
                    text = text.rstrip() + "\n\n" + extra + "\n"
        return text

    def run_task(self, request):
        identifier = task_id(request["id"])
        existing = self.private / "jobs" / (identifier + ".json")
        if existing.exists():
            return  # A replayed inbox file can never start the task again.
        record = {"id": identifier, "request": request, "project": request.get("project"),
                  "created_at": now(), "thread_id": None, "turn_id": None, "pending": None}
        self.save(record, "starting")
        job = self.mailbox / "jobs" / identifier
        self.files.atomic(job / "request.json", request)
        try:
            check_request(request, self.config["projects"])
            # Old queued requests have no caller_repo; preserve their lifecycle.
            if "caller_repo" in request:
                select_project(project_paths(self.config["projects"]),
                               request["caller_repo"], request["project"])
            for path, event in self.controls(record):
                if event["kind"] == "cancel":
                    self.ack(record, path, event, "accepted")
                    self.finish(record, "cancelled", error="Cancelled before execution")
                    return
                self.ack(record, path, event, "rejected", "Task has not started")
            project = self.config["projects"][request["project"]]
            actual_commit = git_head(project["path"])
            if actual_commit != request["expected_commit"]:
                raise ValueError(f"Commit mismatch: expected {request['expected_commit']}, found {actual_commit}")
            artifacts = job / "artifacts"
            self.files.mkdir(artifacts)
            instructions = self.load_instructions()
            prompt = (f"委派任务：\n{request['task']}\n\n授权范围：\n{request['authorization']}\n\n"
                      f"项目：{request['project']}\n提交：{actual_commit}\n"
                      f"产物目录：{artifacts}\n调用方可读取的产物路径：/run/host-agent/jobs/{identifier}/artifacts\n")
            with self.rpc_factory(self.config["socket"]) as rpc:
                approval_policy = self.config.get("approval_policy", "on-request")
                sandbox = project.get("sandbox", "workspace-write")
                parameters = {"cwd": str(project["path"]), "approvalPolicy": approval_policy,
                              "approvalsReviewer": "user", "sandbox": sandbox,
                              "developerInstructions": instructions}
                if self.config.get("model"):
                    parameters["model"] = self.config["model"]
                if request.get("after"):
                    previous = read(self.private / "jobs" / (request["after"] + ".json"))
                    if previous["project"] != request["project"] or previous["state"] not in TERMINAL or not previous.get("thread_id"):
                        raise ValueError("Follow-up requires an ended task in the same project")
                    parameters.update(threadId=previous["thread_id"], excludeTurns=True)
                    response = rpc.call("thread/resume", parameters)
                else:
                    response = rpc.call("thread/start", parameters)
                if response["thread"].get("status", {}).get("type") == "active":
                    raise ValueError("The Codex thread is already active in another client")
                self.probed, self.probe_error = True, None
                self.save(record, "starting_turn", thread_id=response["thread"]["id"],
                          execution_config={"model": response.get("model"),
                                            "effort": self.config.get("effort") or response.get("reasoningEffort"),
                                            "approval_policy": approval_policy, "sandbox": sandbox})
                turn = {"threadId": record["thread_id"], "clientUserMessageId": identifier,
                        "approvalPolicy": approval_policy,
                        "input": [{"type": "text", "text": prompt}]}
                if self.config.get("effort"):
                    turn["effort"] = self.config["effort"]
                if sandbox == "danger-full-access":
                    turn["sandboxPolicy"] = {"type": "dangerFullAccess"}
                elif sandbox == "workspace-write":
                    turn["sandboxPolicy"] = {"type": "workspaceWrite", "writableRoots": [str(project["path"]), str(artifacts)],
                                             "networkAccess": False, "excludeTmpdirEnvVar": False, "excludeSlashTmp": False}
                response = rpc.call("turn/start", turn)
                self.save(record, "running", turn_id=response["turn"]["id"])
                # A brand-new thread has no resumable rollout until its first
                # turn starts. Attach the display after materializing it.
                try:
                    view = HerdrViews(self.config).attach(record, project)
                    if view:
                        self.save(record, view=view, view_error=None)
                except (HerdrError, OSError, ValueError, KeyError) as error:
                    self.save(record, view_error=str(error))
                self.monitor(rpc, record)
        except (OSError, ValueError, KeyError, RpcError, subprocess.SubprocessError) as error:
            if isinstance(error, (RpcError, OSError)):
                self.probed, self.probe_error = False, str(error)
            if record.get("thread_id"):
                self.save(record, "unknown", error=str(error))
                self.reconcile(record)
            else:
                self.finish(record, "failed", error=str(error))

    def run(self, once=False):
        self.prepare()
        with (self.private / "worker.lock").open("a") as lock:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise ValueError("A host-agent worker is already running") from None
            self.probe()
            if not self.recover():
                raise ValueError("An old task has unknown execution state; restore Codex connectivity and restart")
            next_probe = time.monotonic() + 30
            try:
                while not self.stop:
                    if not self.probed and time.monotonic() >= next_probe:
                        self.probe()
                        next_probe = time.monotonic() + 30
                    self.heartbeat("ready" if self.probed else "offline")
                    for path in self.files.json_files(self.mailbox / "inbox"):
                        try:
                            request = self.files.read(path)
                            if path.stem != task_id(request["id"]):
                                raise ValueError("Request ID does not match its filename")
                        except (OSError, ValueError, KeyError):
                            self.files.invalidate(path)
                            continue
                        self.run_task(request)
                        self.files.unlink(path)
                        if not self.recover():
                            raise ValueError("Task state is unknown; worker stopped to avoid overlapping execution")
                        if once or self.stop:
                            return
                    if once:
                        return
                    time.sleep(self.config.get("poll_seconds", 1))
            finally:
                self.heartbeat("offline")


def print_json(value):
    print(json.dumps(value, ensure_ascii=False, indent=2, default=str))


def main(argv=None):
    parser = argparse.ArgumentParser(description="Submit Mac tasks and retrieve their progress and results.")
    default_mailbox = Path("/run/host-agent") if ROOT == Path("/opt/host-agent") else ROOT / "state/mailbox"
    parser.add_argument("--mailbox", type=Path, default=Path(os.environ.get("HOST_AGENT_MAILBOX", default_mailbox)))
    parser.add_argument("--config", type=Path, default=ROOT / "config/local.json")
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("capabilities")
    submit_parser = sub.add_parser("submit")
    submit_parser.add_argument("--project", help="Optional assertion; must match the current Git repository mapping")
    submit_parser.add_argument("--title", required=True, help="Summarized task name for Herdr (1–5 characters)")
    source = submit_parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--task")
    source.add_argument("--task-file", type=Path)
    submit_parser.add_argument("--commit", help="Full commit hash; defaults to HEAD of the current directory")
    submit_parser.add_argument("--authorization", default="仅本地构建、测试与诊断；其他操作须明确获得用户授权。")
    submit_parser.add_argument("--after", help="Continue the context of an ended task")
    for command in ("status", "result", "cancel", "message"):
        action = sub.add_parser(command)
        action.add_argument("task_id", type=task_id)
        if command == "message":
            action.add_argument("text")
    serve = sub.add_parser("serve")
    serve.add_argument("--once", action="store_true")
    sub.add_parser("prepare")
    sub.add_parser("probe")
    respond = sub.add_parser("respond", help="Host only: answer a pending approval/input request")
    respond.add_argument("task_id", type=task_id)
    respond.add_argument("--token", required=True, type=task_id)
    response = respond.add_mutually_exclusive_group(required=True)
    response.add_argument("--decision", choices=["accept", "decline", "cancel"])
    response.add_argument("--result-file", type=Path, help="Exact JSON-RPC result object for input/permission requests")
    args = parser.parse_args(argv)
    try:
        mailbox = args.mailbox.expanduser()
        mailbox = mailbox.parent.resolve() / mailbox.name
        files = Mailbox(mailbox)
        if args.command == "capabilities":
            result = read(mailbox / "capabilities.json", root=mailbox)
            stamp = datetime.datetime.fromisoformat(result["updated_at"])
            result["heartbeat_age_seconds"] = round((datetime.datetime.now(datetime.timezone.utc) - stamp).total_seconds(), 1)
            result["online"] = result["heartbeat_age_seconds"] < result.get("heartbeat_timeout_seconds", 90) and result["worker_state"] not in {"offline", "blocked"}
            print_json(result)
        elif args.command == "submit":
            print_json(submit(mailbox, args.project, args.task if args.task is not None else args.task_file.read_text(),
                              args.commit or git_head(Path.cwd()), args.authorization, args.after, args.title))
        elif args.command in {"cancel", "message"}:
            print_json(control(mailbox, args.task_id, args.command, getattr(args, "text", None)))
        elif args.command in {"status", "result"}:
            path = mailbox / "jobs" / args.task_id / (args.command + ".json")
            if args.command == "status" and not files.exists(path) and files.exists(mailbox / "inbox" / (args.task_id + ".json")):
                print_json({"id": args.task_id, "state": "queued"})
            else:
                result = files.read(path)
                if args.command == "status":
                    result["controls"] = [files.read(p) for p in sorted(files.json_files(path.parent / "controls"))]
                print_json(result)
        else:
            settings = config(args.config)
            worker = Worker(settings)
            if args.command == "prepare":
                worker.prepare()
                print_json({"mailbox": worker.mailbox, "host_state": worker.private, "enabled": False})
            elif args.command == "probe":
                worker.prepare()
                worker.probe()
                print_json(worker.files.read(worker.mailbox / "capabilities.json"))
                if not worker.probed:
                    raise ValueError("Cannot reach the configured Codex App Server")
            elif args.command == "respond":
                record = read(worker.private / "jobs" / (args.task_id + ".json"))
                pending = record.get("pending")
                if not pending or pending["token"] != args.token:
                    raise ValueError("The pending request has changed; inspect status again")
                if args.decision:
                    if pending["method"] not in {"item/commandExecution/requestApproval", "item/fileChange/requestApproval"}:
                        raise ValueError("This request requires --result-file with its protocol-specific response")
                    result = {"decision": args.decision}
                else:
                    result = read(args.result_file)
                    if not isinstance(result, dict):
                        raise ValueError("Response must be a JSON object")
                atomic(worker.private / "decisions" / (args.token + ".json"), {"task_id": args.task_id, "result": result})
                print_json({"task_id": args.task_id, "state": "response_queued"})
            elif args.command == "serve":
                def stop(signum, frame):
                    worker.stop = True
                signal.signal(signal.SIGTERM, stop)
                signal.signal(signal.SIGINT, stop)
                worker.run(args.once)
    except (OSError, ValueError, KeyError, RpcError, subprocess.SubprocessError) as error:
        print_json({"error": str(error)})
        raise SystemExit(1)


if __name__ == "__main__":
    main()
