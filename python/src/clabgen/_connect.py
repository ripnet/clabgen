"""Connect protocol (JSON encoding) over plain HTTP, standard library only.

Unary calls are a POST of the request as JSON. Server-streaming calls send
and receive enveloped messages: a flags byte, a 4-byte big-endian length,
then the JSON; the end-of-stream message (flag 0x02) carries any error.
"""

from __future__ import annotations

import json
import re
import struct
import urllib.error
import urllib.request
from datetime import datetime
from typing import Any, Iterator

SERVICE = "clabgen.v1.AgentService"

# Fields holding free-form JSON (google.protobuf.Struct): their keys are the
# protocols' own and are left alone.
_STRUCT_FIELDS = {"config", "status", "stats", "args", "result", "data", "spec"}
# 64-bit integers, which protobuf's JSON mapping sends as strings.
_INT64_FIELDS = {"rx_frames", "rx_bytes", "tx_frames", "tx_bytes", "tx_errors", "rx_unmatched", "age_ms", "rtt_us"}
_TIME_FIELDS = {"start_time", "time"}


class ClabgenError(Exception):
    """An error from an agent: code is Connect's ("not_found", "invalid_argument", …)."""

    def __init__(self, code: str, message: str, method: str = ""):
        super().__init__(f"{method}: {code}: {message}" if method else f"{code}: {message}")
        self.code = code
        self.message = message
        self.method = method


def _snake(name: str) -> str:
    return re.sub(r"(?<=[a-z0-9])([A-Z])", r"_\1", name).lower()


def _time(s: str) -> datetime:
    # Python before 3.11 can't parse "Z" or more than 6 fraction digits.
    s = s.replace("Z", "+00:00")
    s = re.sub(r"(\.\d{6})\d+", r"\1", s)
    return datetime.fromisoformat(s)


def from_wire(v: Any) -> Any:
    """Convert a decoded response: snake_case field names, ints and datetimes."""
    if isinstance(v, list):
        return [from_wire(x) for x in v]
    if not isinstance(v, dict):
        return v
    out = {}
    for k, x in v.items():
        k = _snake(k)
        if k in _STRUCT_FIELDS:
            out[k] = x
        elif k == "counters" and isinstance(x, dict):
            # Port counters (a message) and device IP counters (a map): all uint64.
            out[k] = {_snake(ck): int(cv) if isinstance(cv, str) and cv.isdigit() else cv for ck, cv in x.items()}
        elif k in _INT64_FIELDS and isinstance(x, str):
            out[k] = int(x)
        elif k in _TIME_FIELDS and isinstance(x, str):
            out[k] = _time(x)
        else:
            out[k] = from_wire(x)
    return out


def _error(method: str, body: bytes, status: int) -> ClabgenError:
    try:
        e = json.loads(body)
        return ClabgenError(e.get("code", "unknown"), e.get("message", ""), method)
    except ValueError:
        return ClabgenError("unknown", f"HTTP {status}: {body[:200].decode(errors='replace')}", method)


class Transport:
    """Calls one agent's AgentService at base_url (http://host:8080, or a
    clabgen-ui proxy path like http://host:8090/agents/clab-b2b-gen1)."""

    def __init__(self, base_url: str, timeout: float = 10.0):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def _url(self, method: str) -> str:
        return f"{self.base_url}/{SERVICE}/{method}"

    def call(self, method: str, request: dict | None = None, timeout: float | None = None) -> dict:
        data = json.dumps(request or {}).encode()
        req = urllib.request.Request(
            self._url(method), data=data, method="POST",
            headers={"Content-Type": "application/json", "Connect-Protocol-Version": "1"},
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as r:
                return from_wire(json.loads(r.read() or b"{}"))
        except urllib.error.HTTPError as e:
            raise _error(method, e.read(), e.code) from None
        except urllib.error.URLError as e:
            raise ClabgenError("unavailable", str(e.reason), method) from None

    def stream(self, method: str, request: dict | None = None, timeout: float | None = None) -> Iterator[dict]:
        """Server-streaming call; timeout is per read (None: wait forever).
        The call is made before this returns, so nothing sent after it is
        missed; the messages come from the returned iterator."""
        payload = json.dumps(request or {}).encode()
        req = urllib.request.Request(
            self._url(method), data=struct.pack(">BI", 0, len(payload)) + payload, method="POST",
            headers={"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"},
        )
        try:
            r = urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as e:
            raise _error(method, e.read(), e.code) from None
        except urllib.error.URLError as e:
            raise ClabgenError("unavailable", str(e.reason), method) from None
        return self._messages(method, r)

    @staticmethod
    def _messages(method: str, r: Any) -> Iterator[dict]:
        with r:
            while True:
                head = r.read(5)
                if len(head) < 5:
                    return
                flags, n = struct.unpack(">BI", head)
                msg = json.loads(r.read(n) or b"{}")
                if flags & 0x02:
                    if "error" in msg:
                        e = msg["error"]
                        raise ClabgenError(e.get("code", "unknown"), e.get("message", ""), method)
                    return
                yield from_wire(msg)
