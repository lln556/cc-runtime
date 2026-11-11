"""Focused mailbox lifecycle and local WebSocket protocol tests; no live agent calls."""
import base64
import contextlib
from collections import deque
import hashlib
import itertools
import io
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "host-agent/src"))
from bridge import HerdrError, Worker, atomic, control, git_head, main, read, submit, task_id
from rpc import CodexRPC, RpcError


class FakeRPC:
    def __init__(self):
        self.calls = []
        self.sent = []
        self.events = deque()
        self.on_receive = None
        self.fail_read = False
        self.fail_start = False
        self.running = False
        self.client_id = None

    def __call__(self, path):
        return self

    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def send(self, value):
        self.sent.append(value)

    def completed(self, state="completed", text="verified"):
        self.events.extend([
            {"method": "item/completed", "params": {"threadId": "thread", "item": {"type": "agentMessage", "text": text}}},
            {"method": "turn/completed", "params": {"threadId": "thread", "turn": {"id": "turn", "status": state}}},
        ])

    def call(self, method, params, **kwargs):
        self.calls.append((method, params))
        if method == "mcpServerStatus/list":
            return {"data": [{"name": "cua_repl", "tools": {"js": {}}}]}
        if method in {"thread/start", "thread/resume"}:
            return {"thread": {"id": "thread", "status": {"type": "idle"}}}
        if method == "turn/start":
            self.running = True
            self.client_id = params.get("clientUserMessageId")
            if self.fail_start:
                raise RpcError("Lost turn/start response")
            return {"turn": {"id": "turn", "status": "inProgress"}}
        if method == "turn/interrupt":
            self.running = False
            self.events.clear()
            self.completed("interrupted")
            return {}
        if method == "thread/read":
            if self.fail_read:
                raise RpcError("Disconnected")
            return {"thread": {"turns": [{"id": "turn", "status": "inProgress" if self.running else "interrupted",
                                           "items": [{"type": "userMessage", "clientId": self.client_id}]}]}}
        if method == "turn/steer":
            return {}
        raise AssertionError(method)

    def receive(self, timeout=1):
        if self.on_receive:
            self.on_receive()
        if self.events:
            event = self.events.popleft()
            if event.get("method") == "turn/completed":
                self.running = False
            return event
        raise AssertionError("Test ran out of events")


class MailboxTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        subprocess.run(["git", "-C", str(self.repo), "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
                        "commit", "--allow-empty", "-qm", "fixture"], check=True)
        self.instructions = self.root / "execution.md"
        self.instructions.write_text("Test instructions")
        self.rpc = FakeRPC()
        self.worker = Worker({"mailbox": self.root / "mailbox", "host_state": self.root / "host",
                              "instructions": self.instructions, "socket": self.root / "rpc.sock",
                              "projects": {"test": {"path": self.repo}}, "task_timeout_seconds": 10}, self.rpc)
        self.worker.prepare()

    def request(self, **overrides):
        result = submit(self.worker.mailbox, "test", "Run tests", git_head(self.repo), "Local tests only", title="测试验证")
        request = read(self.worker.mailbox / "inbox" / (result["task_id"] + ".json"))
        request.update(overrides)
        return request

    def status(self, request):
        return read(self.worker.mailbox / "jobs" / request["id"] / "status.json")

    def result(self, request):
        return read(self.worker.mailbox / "jobs" / request["id"] / "result.json")

    def test_short_title_is_preserved_and_invalid_titles_are_rejected(self):
        result = submit(self.worker.mailbox, "test", "Run tests", git_head(self.repo),
                        "Local tests only", title="订阅验收")
        request = read(self.worker.mailbox / "inbox" / (result["task_id"] + ".json"))
        self.assertEqual(request["title"], "订阅验收")
        for title in [None, "", "一二三四五六", "a\nb", "\x1b标题"]:
            with self.subTest(title=title), self.assertRaises(ValueError):
                submit(self.worker.mailbox, "test", "Run tests", git_head(self.repo),
                       "Local tests only", title=title)

    def test_completed_task_has_evidence_and_is_not_replayed(self):
        request = self.request()
        self.rpc.completed(text="test command passed")
        self.worker.run_task(request)
        self.worker.run_task(request)
        self.assertEqual(self.result(request)["state"], "completed")
        self.assertEqual(self.result(request)["text"], "test command passed")
        self.assertEqual(sum(m == "turn/start" for m, _ in self.rpc.calls), 1)
        public = self.status(request)
        self.assertNotIn("request", public)
        self.assertEqual(public["thread_id"], "thread")

    def test_wrong_commit_never_starts_codex(self):
        request = self.request(expected_commit="0" * 40)
        self.worker.run_task(request)
        self.assertIn("Commit mismatch", self.result(request)["error"])
        self.assertFalse(self.rpc.calls)

    def test_cancel_queued_task_never_starts_codex(self):
        request = self.request()
        control(self.worker.mailbox, request["id"], "cancel")
        self.worker.run_task(request)
        self.assertEqual(self.status(request)["state"], "cancelled")
        self.assertFalse(self.rpc.calls)

    def test_approval_stays_pending_until_host_decides(self):
        request = self.request()
        self.rpc.events.append({"id": "approval-7", "method": "item/commandExecution/requestApproval",
                                "params": {"threadId": "thread", "command": "build"}})
        observed = []

        def human():
            status = self.status(request)
            if status.get("pending") and not observed:
                observed.append(status["state"])
                self.assertFalse(self.rpc.sent)
                token = status["pending"]["token"]
                atomic(self.worker.private / "decisions" / (token + ".json"),
                       {"task_id": request["id"], "result": {"decision": "accept"}})
                self.rpc.events.append({"method": "noop", "params": {}})
                self.rpc.completed()

        self.rpc.on_receive = human
        self.worker.run_task(request)
        self.assertEqual(observed, ["waiting_for_approval"])
        self.assertEqual(self.rpc.sent, [{"id": "approval-7", "result": {"decision": "accept"}}])
        self.assertEqual(self.status(request)["state"], "completed")

    def test_message_is_delivered_and_cancel_waits_for_completion(self):
        request = self.request()
        sent = []

        def sender():
            if not sent:
                sent.append(control(self.worker.mailbox, request["id"], "message", "also check layout"))
                sent.append(control(self.worker.mailbox, request["id"], "cancel"))
                self.rpc.events.append({"method": "noop", "params": {}})

        self.rpc.on_receive = sender
        self.worker.run_task(request)
        self.assertEqual(self.status(request)["state"], "cancelled")
        methods = [m for m, _ in self.rpc.calls]
        self.assertIn("turn/steer", methods)
        self.assertIn("turn/interrupt", methods)
        receipts = self.worker.mailbox / "jobs" / request["id"] / "controls"
        self.assertEqual(read(receipts / (sent[0]["control_id"] + ".json"))["state"], "delivered")

    def test_followup_reuses_only_ended_same_project_thread(self):
        previous = self.request()
        self.rpc.completed()
        self.worker.run_task(previous)
        followup = self.request(after=previous["id"])
        self.rpc.completed()
        self.worker.run_task(followup)
        self.assertEqual(self.result(followup)["state"], "completed")
        self.assertEqual(sum(m == "thread/start" for m, _ in self.rpc.calls), 1)
        self.assertEqual(sum(m == "thread/resume" for m, _ in self.rpc.calls), 1)

    def test_configured_model_and_effort_apply_to_new_and_continued_tasks(self):
        self.worker.config.update(model="model-a", effort="high")
        previous = self.request()
        self.rpc.completed()
        self.worker.run_task(previous)
        self.worker.config.update(model="model-b", effort="medium")
        followup = self.request(after=previous["id"])
        self.rpc.completed()
        self.worker.run_task(followup)
        starts = [p for m, p in self.rpc.calls if m == "thread/start"]
        resumes = [p for m, p in self.rpc.calls if m == "thread/resume"]
        turns = [p for m, p in self.rpc.calls if m == "turn/start"]
        self.assertEqual(starts[0]["model"], "model-a")
        self.assertEqual(resumes[0]["model"], "model-b")
        self.assertEqual([p["effort"] for p in turns], ["high", "medium"])
        self.assertNotIn("execution_config", self.status(followup))

    def test_omitted_model_and_effort_preserve_service_defaults(self):
        request = self.request()
        self.rpc.completed()
        self.worker.run_task(request)
        for method, params in self.rpc.calls:
            if method in {"thread/start", "turn/start"}:
                self.assertNotIn("model", params)
                self.assertNotIn("effort", params)

    def test_local_instructions_supplement_base_instructions_when_present(self):
        first = self.request()
        self.rpc.completed()
        self.worker.run_task(first)
        local = self.root / "config/execution.local.md"
        local.parent.mkdir()
        local.write_text("Host-only private server rules\n")
        second = self.request(after=first["id"])
        self.rpc.completed()
        self.worker.run_task(second)
        starts = [p for m, p in self.rpc.calls if m == "thread/start"]
        resumes = [p for m, p in self.rpc.calls if m == "thread/resume"]
        self.assertEqual(starts[0]["developerInstructions"], "Test instructions")
        self.assertEqual(resumes[0]["developerInstructions"],
                         "Test instructions\n\nHost-only private server rules\n")
        local.unlink()
        local.symlink_to(self.instructions)
        rejected = self.request()
        self.worker.run_task(rejected)
        self.assertIn("Refusing symlink", self.result(rejected)["error"])

    def test_view_is_opened_after_first_turn_materializes_session(self):
        request = self.request()
        self.rpc.completed()
        view = {"workspace_id": "w1", "tab_id": "w1:t2", "pane_id": "w1:p2"}

        def attach(record, project):
            self.assertEqual(record["thread_id"], "thread")
            self.assertIn("turn/start", [method for method, _ in self.rpc.calls])
            self.assertEqual(record["turn_id"], "turn")
            self.assertEqual(project["path"], self.repo)
            return view

        with patch("bridge.HerdrViews") as viewer:
            viewer.return_value.attach.side_effect = attach
            self.worker.run_task(request)
        self.assertEqual(self.status(request)["view"], view)
        self.assertEqual(self.status(request)["state"], "completed")

    def test_view_failure_is_reported_without_replaying_task(self):
        request = self.request()
        self.rpc.completed()
        with patch("bridge.HerdrViews") as viewer:
            viewer.return_value.attach.side_effect = HerdrError("Herdr is offline")
            self.worker.run_task(request)
        self.assertEqual(self.status(request)["view_error"], "Herdr is offline")
        self.assertEqual(self.status(request)["state"], "completed")
        self.assertEqual(sum(method == "turn/start" for method, _ in self.rpc.calls), 1)

    def test_full_access_applies_to_new_and_continued_tasks(self):
        self.worker.config["approval_policy"] = "never"
        self.worker.config["projects"]["test"]["sandbox"] = "danger-full-access"
        previous = self.request()
        self.rpc.completed()
        self.worker.run_task(previous)
        followup = self.request(after=previous["id"])
        self.rpc.completed()
        self.worker.run_task(followup)
        self.assertEqual(self.result(followup)["state"], "completed")
        for method, params in self.rpc.calls:
            if method in {"thread/start", "thread/resume"}:
                self.assertEqual(params["approvalPolicy"], "never")
                self.assertEqual(params["sandbox"], "danger-full-access")
            if method == "turn/start":
                self.assertEqual(params["approvalPolicy"], "never")
                self.assertEqual(params["sandboxPolicy"], {"type": "dangerFullAccess"})

    def test_lost_start_response_is_interrupted_not_retried(self):
        request = self.request()
        self.rpc.fail_start = True
        self.worker.run_task(request)
        self.assertEqual(self.status(request)["state"], "interrupted")
        self.assertIn("turn/interrupt", [m for m, _ in self.rpc.calls])
        self.worker.run_task(request)
        self.assertEqual(sum(m == "turn/start" for m, _ in self.rpc.calls), 1)

    def test_unreachable_old_task_blocks_new_work(self):
        request = self.request()
        self.worker.save({"id": request["id"], "thread_id": "old", "state": "running", "request": request})
        self.rpc.fail_read = True
        self.assertFalse(self.worker.recover())
        self.assertEqual(self.status(request)["state"], "unknown")
        self.assertEqual(read(self.worker.mailbox / "capabilities.json")["worker_state"], "blocked")
        self.assertNotIn("turn/start", [m for m, _ in self.rpc.calls])

    def test_unknown_tool_request_is_reported_and_turn_stopped(self):
        request = self.request()
        self.rpc.events.append({"id": "tool-9", "method": "item/tool/call", "params": {"threadId": "thread"}})
        self.worker.run_task(request)
        self.assertEqual(self.rpc.sent[0]["error"]["code"], -32601)
        self.assertEqual(self.status(request)["state"], "interrupted")

    def test_recovery_never_interrupts_another_clients_turn(self):
        request = self.request()
        self.worker.save({"id": request["id"], "thread_id": "thread", "state": "starting_turn", "request": request})
        self.rpc.running = True
        self.rpc.client_id = "another-client"
        self.assertFalse(self.worker.recover())
        self.assertEqual(self.status(request)["state"], "unknown")
        self.assertNotIn("turn/interrupt", [m for m, _ in self.rpc.calls])

    def test_rejects_path_escape_and_unknown_project(self):
        with self.assertRaises(ValueError):
            task_id("../../host")
        with self.assertRaises(ValueError):
            submit(self.worker.mailbox, "not-configured", "test", git_head(self.repo), "tests")
        self.assertEqual(list((self.worker.mailbox / "inbox").glob("*.json")), [])

    def test_probe_does_not_claim_worker_or_app_access_is_online(self):
        self.worker.probe()
        capabilities = read(self.worker.mailbox / "capabilities.json")
        self.assertEqual(capabilities["computer_use"], "advertised")
        self.assertEqual(capabilities["worker_state"], "offline")

    def test_bad_queue_file_does_not_prevent_next_task(self):
        atomic(self.worker.mailbox / "inbox" / ("0" * 32 + ".json"), [])
        request = self.request()
        self.rpc.completed()
        self.worker.run(once=True)
        self.assertEqual(self.result(request)["state"], "completed")
        self.assertTrue((self.worker.mailbox / "inbox" / ("0" * 32 + ".invalid")).exists())

    def test_worker_recovers_when_codex_starts_later(self):
        attempts = []

        def connect(path):
            attempts.append(path)
            if len(attempts) == 1:
                raise RpcError("Codex has not started yet")
            return self.rpc

        def idle(seconds):
            self.assertEqual(read(self.worker.mailbox / "capabilities.json")["worker_state"], "ready")
            self.worker.stop = True

        self.worker.rpc_factory = connect
        with patch("bridge.time.monotonic", side_effect=itertools.count(0, 60)), patch("bridge.time.sleep", side_effect=idle):
            self.worker.run()
        self.assertEqual(len(attempts), 2)


class ClientPathTests(unittest.TestCase):
    def mailbox_for(self, root, environment, arguments=()):
        snapshot = {"updated_at": "2026-11-11T00:00:00+00:00", "worker_state": "ready"}
        with patch("bridge.ROOT", Path(root)), patch.dict(os.environ, environment, clear=True), \
                patch("bridge.read", return_value=snapshot) as reader, \
                contextlib.redirect_stdout(io.StringIO()):
            main([*arguments, "capabilities"])
        return reader.call_args.args[0].parent

    def test_installed_client_works_without_ssh_environment(self):
        self.assertEqual(self.mailbox_for("/opt/host-agent", {}), Path("/run/host-agent"))

    def test_checkout_preserves_local_mailbox_default(self):
        self.assertEqual(self.mailbox_for(str(ROOT / "host-agent"), {}),
                         ROOT / "host-agent/state/mailbox")

    def test_environment_overrides_installed_default(self):
        self.assertEqual(self.mailbox_for("/opt/host-agent", {"HOST_AGENT_MAILBOX": "/tmp/custom"}),
                         Path("/tmp/custom").resolve())

    def test_flag_overrides_environment(self):
        self.assertEqual(self.mailbox_for("/opt/host-agent", {"HOST_AGENT_MAILBOX": "/tmp/custom"},
                                         ["--mailbox", "/tmp/explicit"]), Path("/tmp/explicit").resolve())


class TransportTests(unittest.TestCase):
    def pair(self):
        client, server = socket.socketpair()
        self.addCleanup(client.close)
        self.addCleanup(server.close)
        rpc = CodexRPC.__new__(CodexRPC)
        rpc.socket = client
        rpc.buffer, rpc.fragments = bytearray(), bytearray()
        rpc.events, rpc.next_id = deque(), 0
        return rpc, server

    @staticmethod
    def frame(payload, opcode=1, final=True):
        if not isinstance(payload, bytes):
            payload = json.dumps(payload).encode()
        first = (128 if final else 0) | opcode
        if len(payload) < 126:
            return bytes([first, len(payload)]) + payload
        return bytes([first, 126]) + struct.pack("!H", len(payload)) + payload

    @staticmethod
    def client_message(server):
        def exact(length):
            data = b""
            while len(data) < length:
                data += server.recv(length - len(data))
            return data
        first, length = exact(2)
        if not length & 128:
            raise AssertionError("Client frames must be masked")
        length &= 127
        if length == 126:
            length = struct.unpack("!H", exact(2))[0]
        mask = exact(4)
        data = exact(length)
        return first & 15, bytes(v ^ mask[i % 4] for i, v in enumerate(data))

    def test_partial_frame_survives_timeout_and_ping(self):
        rpc, server = self.pair()
        message = json.dumps({"text": "中文" * 100}).encode()
        first = self.frame(message[:40], final=False)
        server.sendall(first[:3])
        self.assertIsNone(rpc.receive(.01))
        server.sendall(first[3:] + self.frame(b"hello", opcode=9) + self.frame(message[40:], opcode=0))
        self.assertEqual(rpc.receive(1), json.loads(message))
        self.assertEqual(self.client_message(server), (10, b"hello"))

    def test_rpc_response_preserves_interleaved_events(self):
        rpc, server = self.pair()
        server.sendall(self.frame({"method": "turn/started", "params": {}}) + self.frame({"id": 1, "result": {"ok": True}}))
        self.assertEqual(rpc.call("test", {}), {"ok": True})
        self.assertEqual(rpc.receive()["method"], "turn/started")
        opcode, payload = self.client_message(server)
        self.assertEqual(opcode, 1)
        self.assertEqual(json.loads(payload)["method"], "test")

    def test_disconnect_is_not_a_successful_empty_result(self):
        rpc, server = self.pair()
        server.close()
        with self.assertRaises(RpcError):
            rpc.receive(1)

    def test_real_unix_handshake_and_initialization(self):
        with tempfile.TemporaryDirectory(prefix="ha-") as directory:
            path = Path(directory) / "s"
            listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.addCleanup(listener.close)
            listener.bind(str(path))
            listener.listen(1)
            failures = []

            def serve():
                try:
                    conn, _ = listener.accept()
                    with conn:
                        conn.settimeout(3)
                        header = b""
                        while b"\r\n\r\n" not in header:
                            header += conn.recv(4096)
                        key = next(line.split(b": ", 1)[1] for line in header.split(b"\r\n") if line.startswith(b"Sec-WebSocket-Key:"))
                        accept = base64.b64encode(hashlib.sha1(key + b"258EAFA5-E914-47DA-95CA-C5AB0DC85B11").digest())
                        conn.sendall(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + b"\r\n\r\n")
                        _, payload = self.client_message(conn)
                        request = json.loads(payload)
                        self.assertEqual(request["method"], "initialize")
                        conn.sendall(self.frame({"id": request["id"], "result": {}}))
                        _, payload = self.client_message(conn)
                        self.assertEqual(json.loads(payload)["method"], "initialized")
                except BaseException as error:
                    failures.append(error)

            thread = threading.Thread(target=serve, daemon=True)
            thread.start()
            with CodexRPC(path):
                pass
            thread.join(4)
            self.assertFalse(thread.is_alive())
            if failures:
                raise failures[0]


if __name__ == "__main__":
    unittest.main()
