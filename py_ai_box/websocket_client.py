import base64
import hashlib
import os
import socket
import ssl
import struct
from urllib.parse import urlparse


class WebSocketClient:
    def __init__(self, url: str, headers=None) -> None:
        self.url = url
        self.headers = headers or {}
        self.sock = None

    def connect(self) -> None:
        u = urlparse(self.url)
        host = u.hostname
        port = u.port or (443 if u.scheme == "wss" else 80)
        if host is None:
            raise ValueError("invalid ws url")
        raw_sock = socket.create_connection((host, port), timeout=10)
        if u.scheme == "wss":
            context = ssl._create_unverified_context()
            raw_sock = context.wrap_socket(raw_sock, server_hostname=host)
        self.sock = raw_sock

        key = base64.b64encode(os.urandom(16)).decode("ascii")
        req = [
            f"GET {u.path or '/'}{f'?{u.query}' if u.query else ''} HTTP/1.1",
            f"Host: {host}",
            "Upgrade: websocket",
            "Connection: Upgrade",
            "Sec-WebSocket-Version: 13",
            f"Sec-WebSocket-Key: {key}",
        ]
        for k, v in self.headers.items():
            req.append(f"{k}: {v}")
        req.append("\r\n")
        self.sock.sendall("\r\n".join(req).encode("utf-8"))

        resp = self._recv_until(b"\r\n\r\n")
        if b" 101 " not in resp:
            raise RuntimeError(f"websocket handshake failed: {resp[:200]!r}")
        accept_key = self._get_accept_key(key)
        if accept_key.encode("ascii") not in resp:
            raise RuntimeError("websocket accept key mismatch")

    def send_text(self, text: str) -> None:
        self._send_frame(0x1, text.encode("utf-8"))

    def send_binary(self, data: bytes) -> None:
        self._send_frame(0x2, data)

    def recv(self):
        while True:
            opcode, payload = self._recv_frame()
            if opcode == 0x1:
                return ("text", payload.decode("utf-8", errors="ignore"))
            if opcode == 0x2:
                return ("binary", payload)
            if opcode == 0x8:
                self.close()
                return ("close", b"")
            if opcode == 0x9:
                self._send_frame(0xA, payload)
                continue
            if opcode == 0xA:
                continue

    def close(self) -> None:
        if self.sock is None:
            return
        try:
            self._send_frame(0x8, b"")
        except Exception:
            pass
        try:
            self.sock.close()
        finally:
            self.sock = None

    def _send_frame(self, opcode: int, payload: bytes) -> None:
        if self.sock is None:
            raise RuntimeError("socket not connected")
        fin = 0x80
        mask_bit = 0x80
        length = len(payload)
        header = bytearray()
        header.append(fin | opcode)
        if length < 126:
            header.append(mask_bit | length)
        elif length < (1 << 16):
            header.append(mask_bit | 126)
            header.extend(struct.pack("!H", length))
        else:
            header.append(mask_bit | 127)
            header.extend(struct.pack("!Q", length))

        mask = os.urandom(4)
        header.extend(mask)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(header + masked)

    def _recv_frame(self):
        if self.sock is None:
            raise RuntimeError("socket not connected")
        first_two = self._recv_exact(2)
        b1, b2 = first_two
        opcode = b1 & 0x0F
        masked = (b2 & 0x80) != 0
        length = b2 & 0x7F
        if length == 126:
            length = struct.unpack("!H", self._recv_exact(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", self._recv_exact(8))[0]
        mask_key = self._recv_exact(4) if masked else None
        payload = self._recv_exact(length) if length > 0 else b""
        if masked and mask_key:
            payload = bytes(b ^ mask_key[i % 4] for i, b in enumerate(payload))
        return opcode, payload

    def _recv_exact(self, n: int) -> bytes:
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise RuntimeError("socket closed")
            buf += chunk
        return buf

    def _recv_until(self, marker: bytes) -> bytes:
        buf = b""
        while marker not in buf:
            chunk = self.sock.recv(4096)
            if not chunk:
                break
            buf += chunk
        return buf

    @staticmethod
    def _get_accept_key(key: str) -> str:
        magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
        sha1 = hashlib.sha1((key + magic).encode("utf-8")).digest()
        return base64.b64encode(sha1).decode("ascii")
