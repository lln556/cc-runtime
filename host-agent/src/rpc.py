"""Codex JSON-RPC over its local Unix WebSocket; no third-party dependencies."""
import base64
import hashlib
import json
import os
import select
import socket
import struct
import time
from collections import deque


class RpcError(RuntimeError):
    pass


class CodexRPC:
    MAX_FRAME = 32 * 1024 * 1024

    def __init__(self, path, timeout=20):
        self.socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.buffer = bytearray()
        self.fragments = bytearray()
        self.events = deque()
        self.next_id = 0
        try:
            self.socket.settimeout(timeout)
            self.socket.connect(str(path))
            key = base64.b64encode(os.urandom(16)).decode()
            self.socket.sendall((
                "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\n"
                "Connection: Upgrade\r\nSec-WebSocket-Version: 13\r\n"
                f"Sec-WebSocket-Key: {key}\r\n\r\n"
            ).encode())
            while b"\r\n\r\n" not in self.buffer:
                chunk = self.socket.recv(4096)
                if not chunk:
                    raise RpcError("Codex closed the WebSocket handshake")
                self.buffer.extend(chunk)
                if len(self.buffer) > 65536:
                    raise RpcError("Oversized WebSocket handshake")
            header, remaining = bytes(self.buffer).split(b"\r\n\r\n", 1)
            self.buffer = bytearray(remaining)
            lines = header.decode("ascii").split("\r\n")
            headers = {k.lower(): v.strip() for k, v in
                       (line.split(":", 1) for line in lines[1:])}
            expected = base64.b64encode(hashlib.sha1(
                (key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()
            ).digest()).decode()
            if " 101 " not in lines[0] or headers.get("sec-websocket-accept", "").strip() != expected:
                raise RpcError("Invalid Codex WebSocket handshake")
            self.socket.settimeout(None)
            self.call("initialize", {
                "clientInfo": {"name": "cc_runtime_host_agent", "version": "0.1.0"},
                "capabilities": {"experimentalApi": True, "requiredHumanApproval": True},
            }, timeout=timeout)
            self.send({"method": "initialized"})
        except BaseException:
            self.socket.close()
            raise

    def close(self):
        self.socket.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()

    def _send_frame(self, payload, opcode=1):
        length = len(payload)
        if length < 126:
            header = bytes([0x80 | opcode, 0x80 | length])
        elif length < 65536:
            header = bytes([0x80 | opcode, 0xFE]) + struct.pack("!H", length)
        else:
            header = bytes([0x80 | opcode, 0xFF]) + struct.pack("!Q", length)
        mask = os.urandom(4)
        masked = bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload))
        self.socket.sendall(header + mask + masked)

    def send(self, value):
        self._send_frame(json.dumps(value, ensure_ascii=False).encode())

    def _receive(self, timeout):
        """Preserve incomplete frames across timeouts (including interleaved pings)."""
        deadline = time.monotonic() + timeout
        while True:
            if len(self.buffer) >= 2:
                first, second = self.buffer[:2]
                length, offset = second & 127, 2
                if length == 126 and len(self.buffer) >= 4:
                    length, offset = struct.unpack("!H", self.buffer[2:4])[0], 4
                elif length == 127 and len(self.buffer) >= 10:
                    length, offset = struct.unpack("!Q", self.buffer[2:10])[0], 10
                elif length >= 126:
                    length = None
                if length is not None:
                    if length > self.MAX_FRAME or second & 128 or first & 0x70:
                        raise RpcError("Unsupported or oversized WebSocket frame")
                    if len(self.buffer) >= offset + length:
                        payload = bytes(self.buffer[offset:offset + length])
                        del self.buffer[:offset + length]
                        opcode = first & 15
                        if opcode == 8:
                            raise RpcError("Codex closed the connection")
                        if opcode == 9:
                            self._send_frame(payload, 10)
                        elif opcode == 10:
                            pass
                        elif opcode in (0, 1):
                            self.fragments.extend(payload)
                            if len(self.fragments) > self.MAX_FRAME:
                                raise RpcError("Oversized WebSocket message")
                            if first & 128:
                                data = bytes(self.fragments)
                                self.fragments.clear()
                                return json.loads(data)
                        else:
                            raise RpcError(f"Unsupported WebSocket opcode {opcode}")
                        continue
            left = deadline - time.monotonic()
            if left <= 0 or not select.select([self.socket], [], [], left)[0]:
                return None
            chunk = self.socket.recv(65536)
            if not chunk:
                raise RpcError("Codex disconnected")
            self.buffer.extend(chunk)

    def receive(self, timeout=1):
        return self.events.popleft() if self.events else self._receive(timeout)

    def call(self, method, params, timeout=30):
        self.next_id += 1
        request_id = self.next_id
        self.send({"id": request_id, "method": method, "params": params})
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            message = self._receive(max(0, deadline - time.monotonic()))
            if message is None:
                break
            if message.get("id") == request_id and "method" not in message:
                if "error" in message:
                    raise RpcError(f"{method}: {message['error']}")
                return message["result"]
            self.events.append(message)
        raise RpcError(f"Timed out waiting for {method}; execution outcome may be unknown")
