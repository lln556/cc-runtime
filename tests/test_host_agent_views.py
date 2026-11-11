"""Herdr placement and reuse tests against its CLI boundary."""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "host-agent/src"))
import bridge


class HerdrCLI:
    def __init__(self):
        self.calls = []
        self.workspaces = [{"workspace_id": "other", "label": "other", "focused": True},
                           {"workspace_id": "project", "label": "ios-app", "focused": False}]
        self.panes = [{"workspace_id": "other", "cwd": "/tmp/other"},
                      {"workspace_id": "project", "cwd": "/tmp/ios-app"}]
        self.processes = {}
        self.next_pane = 1
        self.fail_start = False
        self.working_during_start = False
        self.fail_process = False
        self.busy_starts = 0
        self.delayed_process_reads = 0
        self.start_error_after_spawn = None
        self.pending_processes = {}

    def __call__(self, argv, **kwargs):
        self.calls.append(argv)
        command = argv[3:]  # binary --session default
        if command[:2] == ["workspace", "list"]:
            result = {"workspaces": self.workspaces}
        elif command[:2] == ["pane", "list"]:
            result = {"panes": self.panes}
        elif command[:2] in (["workspace", "create"], ["tab", "create"]):
            workspace = "created" if command[0] == "workspace" else command[command.index("--workspace") + 1]
            pane = f"{workspace}:p{self.next_pane}"
            self.next_pane += 1
            result = {"workspace": {"workspace_id": workspace},
                      "tab": {"tab_id": f"{workspace}:t{self.next_pane}"},
                      "root_pane": {"pane_id": pane}}
            self.panes.append({"workspace_id": workspace, "pane_id": pane,
                               "cwd": command[command.index("--cwd") + 1]})
            self.processes[pane] = [{"argv": ["fish"]}]
        elif command[:2] == ["pane", "process-info"]:
            pane = command[command.index("--pane") + 1]
            if self.fail_process:
                return subprocess.CompletedProcess(argv, 1, "", "connection lost")
            if pane not in self.processes:
                return subprocess.CompletedProcess(argv, 1, "", "pane not found")
            if pane in self.pending_processes:
                if self.delayed_process_reads:
                    self.delayed_process_reads -= 1
                else:
                    self.processes[pane] = self.pending_processes.pop(pane)
            result = {"process_info": {"foreground_processes": self.processes[pane]}}
        elif command[:2] == ["agent", "start"]:
            if self.busy_starts:
                self.busy_starts -= 1
                error = {"error": {"code": "agent_pane_busy", "message": "not an available shell"}}
                return subprocess.CompletedProcess(argv, 1, "", json.dumps(error))
            if self.fail_start:
                return subprocess.CompletedProcess(argv, 1, "", "agent_not_ready")
            pane = command[command.index("--pane") + 1]
            native = command[command.index("--") + 1:]
            self.processes[pane] = [{"argv": ["codex", *native]}]
            if self.start_error_after_spawn:
                self.pending_processes[pane] = self.processes[pane]
                self.processes[pane] = [{"argv": ["fish"]}]
                error = {"error": {"code": self.start_error_after_spawn,
                                   "message": "startup detection is not settled"}}
                return subprocess.CompletedProcess(argv, 1, "", json.dumps(error))
            if self.working_during_start:
                return subprocess.CompletedProcess(argv, 1, "", "timed out waiting for agent startup")
            result = {"agent": {"pane_id": pane}}
        elif command[:2] == ["tab", "rename"]:
            result = {}
        else:
            raise AssertionError(command)
        return subprocess.CompletedProcess(argv, 0, json.dumps({"result": result}), "")


class HerdrViewTests(unittest.TestCase):
    def setUp(self):
        self.assertIsNotNone(getattr(bridge, "HerdrViews", None), "Herdr views are not implemented")
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.cli = HerdrCLI()
        self.settings = {"herdr": {"enabled": True, "binary": "herdr", "session": "default"},
                         "socket": Path("/tmp/codex.sock"), "host_state": Path(self.temp.name)}
        self.view = bridge.HerdrViews(self.settings, runner=self.cli)
        self.elapsed = 0
        self.view.clock = lambda: self.elapsed
        self.view.sleep = self.advance_clock
        self.record = {"id": "a" * 32, "project": "ios-app", "thread_id": "thread-id",
                       "execution_config": {"model": "model-a", "effort": "high",
                                            "approval_policy": "never", "sandbox": "danger-full-access"}}
        self.project = {"path": Path("/tmp/ios-app")}

    def advance_clock(self, seconds):
        self.elapsed += seconds

    def test_opens_in_matching_workspace_without_changing_focus(self):
        result = self.view.attach(self.record, self.project)
        self.assertEqual(result["workspace_id"], "project")
        create = next(c for c in self.cli.calls if c[3:5] == ["tab", "create"])
        self.assertIn("--no-focus", create)
        self.assertEqual(create[create.index("--cwd") + 1], "/tmp/ios-app")
        start = next(c for c in self.cli.calls if c[3:5] == ["agent", "start"])
        native = start[start.index("--") + 1:]
        self.assertIn("unix:///tmp/codex.sock", native)
        self.assertEqual(native[native.index("resume") + 1], "thread-id")
        self.assertEqual(native, ["--remote", "unix:///tmp/codex.sock", "resume", "thread-id", "--cd", "/tmp/ios-app",
                                  "-c", 'tui.terminal_title=["thread-name","project-name"]'])
        self.assertNotIn("--last", native)

    def test_followup_reuses_same_live_pane(self):
        first = self.view.attach(self.record, self.project)
        self.record["id"] = "b" * 32
        self.assertEqual(self.view.attach(self.record, self.project)["pane_id"], first["pane_id"])
        self.assertEqual(sum(c[3:5] == ["agent", "start"] for c in self.cli.calls), 1)

    def test_task_name_is_used_on_creation_and_followup(self):
        self.record["request"] = {"title": "订阅验收", "task": "验证会员订阅布局"}
        first = self.view.attach(self.record, self.project)
        create = next(c for c in self.cli.calls if c[3:5] == ["tab", "create"])
        self.assertEqual(create[create.index("--label") + 1], "订阅验收")
        self.record["request"]["title"] = "上传测试包"
        self.view.attach(self.record, self.project)
        self.assertEqual(self.cli.calls[-1][3:], ["tab", "rename", first["tab_id"], "上传测试包"])

    def test_legacy_task_name_never_truncates_prompt(self):
        from views import task_title
        self.record["request"] = {"task": "本任务为新会话。目标：验证订阅布局，检查按钮状态。"}
        self.assertEqual(task_title(self.record), "未命名任务")
        self.record["request"] = {"task": "部署验证：只回复 OK"}
        self.assertEqual(task_title(self.record), "未命名任务")
        self.record["request"] = {}
        self.assertEqual(task_title(self.record), "未命名任务")
        self.record["request"] = {"task": "验证会员订阅布局", "title": "订阅验收"}
        self.assertEqual(task_title(self.record), "订阅验收")

    def test_independent_threads_created_together_have_unique_agent_names(self):
        self.record["thread_id"] = "01a1c65a-1111-7000-8000-000000000001"
        self.view.attach(self.record, self.project)
        self.record.update(id="b" * 32, thread_id="01a1c65a-1111-7000-8000-000000000002")
        self.view.attach(self.record, self.project)
        names = [c[5] for c in self.cli.calls if c[3:5] == ["agent", "start"]]
        self.assertEqual(len(set(names)), 2)

    def test_missing_workspace_is_created_with_project_directory(self):
        self.cli.workspaces = self.cli.workspaces[:1]
        self.cli.panes = self.cli.panes[:1]
        result = self.view.attach(self.record, self.project)
        self.assertEqual(result["workspace_id"], "created")
        create = next(c for c in self.cli.calls if c[3:5] == ["workspace", "create"])
        self.assertIn("--no-focus", create)
        self.assertEqual(create[create.index("--label") + 1], "ios-app")

    def test_replaced_pane_is_not_reused_or_closed(self):
        first = self.view.attach(self.record, self.project)
        self.cli.processes[first["pane_id"]] = [{"argv": ["vim"]}]
        second = self.view.attach(self.record, self.project)
        self.assertNotEqual(second["pane_id"], first["pane_id"])
        self.assertFalse(any("close" in c or "send-keys" in c for c in self.cli.calls))

    def test_closed_pane_is_recreated(self):
        first = self.view.attach(self.record, self.project)
        self.cli.panes = [p for p in self.cli.panes if p.get("pane_id") != first["pane_id"]]
        self.assertNotEqual(self.view.attach(self.record, self.project)["pane_id"], first["pane_id"])

    def test_connection_error_does_not_create_another_view(self):
        self.view.attach(self.record, self.project)
        self.cli.fail_process = True
        with self.assertRaises(RuntimeError):
            self.view.attach(self.record, self.project)
        self.assertEqual(sum(c[3:5] == ["tab", "create"] for c in self.cli.calls), 1)

    def test_same_socket_through_symlink_reuses_live_view(self):
        first = self.view.attach(self.record, self.project)
        alias = Path(self.temp.name) / "socket-alias"
        alias.symlink_to(self.settings["socket"])
        self.cli.processes[first["pane_id"]][0]["argv"][2] = "unix://" + str(alias)
        self.assertEqual(self.view.attach(self.record, self.project)["pane_id"], first["pane_id"])

    def test_view_inherits_remote_task_settings_without_overrides(self):
        self.record["execution_config"].update(approval_policy="on-request", sandbox="workspace-write")
        self.view.attach(self.record, self.project)
        start = next(c for c in self.cli.calls if c[3:5] == ["agent", "start"])
        self.assertNotIn("--dangerously-bypass-approvals-and-sandbox", start)
        self.assertNotIn("--ask-for-approval", start)
        self.assertNotIn("--sandbox", start)
        self.assertNotIn("--model", start)

    def test_start_failure_does_not_duplicate_an_uncertain_launch(self):
        self.cli.fail_start = True
        with self.assertRaises(RuntimeError):
            self.view.attach(self.record, self.project)
        with self.assertRaises(RuntimeError):
            self.view.attach(self.record, self.project)
        self.assertEqual(sum(c[3:5] == ["tab", "create"] for c in self.cli.calls), 1)
        self.assertEqual(sum(c[3:5] == ["agent", "start"] for c in self.cli.calls), 1)

    def test_working_session_is_attached_even_when_readiness_wait_times_out(self):
        self.cli.working_during_start = True
        view = self.view.attach(self.record, self.project)
        self.assertEqual(view["state"], "attached")
        self.assertEqual(self.view.attach(self.record, self.project)["pane_id"], view["pane_id"])
        self.assertEqual(sum(c[3:5] == ["agent", "start"] for c in self.cli.calls), 1)

    def test_shell_startup_rejection_is_retried_in_the_same_pane(self):
        self.cli.busy_starts = 2
        view = self.view.attach(self.record, self.project)
        self.assertEqual(view["state"], "attached")
        starts = [c for c in self.cli.calls if c[3:5] == ["agent", "start"]]
        self.assertEqual(len(starts), 3)
        self.assertEqual({c[c.index("--pane") + 1] for c in starts}, {view["pane_id"]})
        self.assertEqual(sum(c[3:5] == ["tab", "create"] for c in self.cli.calls), 1)
        self.assertGreater(self.elapsed, 0)

    def test_busy_shell_wait_is_bounded_and_keeps_the_existing_pane(self):
        self.cli.busy_starts = 1000
        with self.assertRaisesRegex(RuntimeError, "agent_pane_busy"):
            self.view.attach(self.record, self.project)
        starts = sum(c[3:5] == ["agent", "start"] for c in self.cli.calls)
        self.assertGreater(starts, 1)
        self.assertLess(starts, 1000)
        self.assertGreater(self.elapsed, 0)
        self.assertLessEqual(self.elapsed, 60)
        with self.assertRaises(RuntimeError):
            self.view.attach(self.record, self.project)
        self.assertEqual(sum(c[3:5] == ["tab", "create"] for c in self.cli.calls), 1)
        self.assertEqual(sum(c[3:5] == ["agent", "start"] for c in self.cli.calls), starts)

    def test_delayed_codex_process_is_confirmed_without_relaunching(self):
        self.cli.start_error_after_spawn = "agent_kind_mismatch"
        self.cli.delayed_process_reads = 2
        view = self.view.attach(self.record, self.project)
        self.assertEqual(view["state"], "attached")
        self.assertGreater(self.elapsed, 0)
        self.assertEqual(sum(c[3:5] == ["agent", "start"] for c in self.cli.calls), 1)
        self.assertEqual(self.view.attach(self.record, self.project)["pane_id"], view["pane_id"])

    def test_slow_shell_initialization_does_not_resubmit_an_accepted_command(self):
        self.cli.start_error_after_spawn = "timeout"
        self.cli.delayed_process_reads = 60  # Conda startup can exceed ten seconds.
        view = self.view.attach(self.record, self.project)
        self.assertEqual(view["state"], "attached")
        self.assertGreater(self.elapsed, 10)
        self.assertEqual(sum(c[3:5] == ["agent", "start"] for c in self.cli.calls), 1)

    def test_disabled_view_does_not_invoke_herdr(self):
        self.settings["herdr"]["enabled"] = False
        self.assertIsNone(bridge.HerdrViews(self.settings, runner=self.cli).attach(self.record, self.project))
        self.assertFalse(self.cli.calls)


if __name__ == "__main__":
    unittest.main()
