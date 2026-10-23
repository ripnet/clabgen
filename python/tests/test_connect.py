import json
import struct
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from clabgen import Agent, ClabgenError
from clabgen._connect import from_wire
from clabgen.agent import _base_url, _vlan, merge


class Fake(BaseHTTPRequestHandler):
    """A few AgentService methods the way connect-go answers them."""

    calls: list = []

    def log_message(self, *a):
        pass

    def do_POST(self):
        method = self.path.rsplit("/", 1)[-1]
        body = self.rfile.read(int(self.headers["Content-Length"]))
        Fake.calls.append((self.path, self.headers["Content-Type"], body))
        if method == "ListPorts":
            self._json(200, {"ports": [{"name": "eth1", "adminUp": True, "counters": {"rxFrames": "12", "txErrors": "0"}}]})
        elif method == "GetInfo":
            self._json(200, {"hostname": "h", "startTime": "2026-10-23T16:29:23.944123456Z"})
        elif method == "GetProtocol":
            self._json(200, {"protocol": {"name": "p", "type": "x", "status": {"someKey": 1, "peers": [{"peerAs": 2}]}}})
        elif method == "DeleteDevice":
            self._json(404, {"code": "not_found", "message": "device nope: not found"})
        elif method == "WatchEvents":
            frames = b""
            for msg, flags in [({"source": "port/eth1", "kind": "down", "time": "2026-10-23T16:00:00Z"}, 0),
                               ({"source": "port/eth1", "kind": "up"}, 0),
                               ({"error": {"code": "canceled", "message": "bye"}}, 2)]:
                b = json.dumps(msg).encode()
                frames += struct.pack(">BI", flags, len(b)) + b
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.send_header("Content-Length", str(len(frames)))
            self.end_headers()
            self.wfile.write(frames)
        else:
            self._json(404, {"code": "unimplemented", "message": method})

    def _json(self, code, v):
        b = json.dumps(v).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)


@pytest.fixture(scope="module")
def fake():
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Fake)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield Agent(f"127.0.0.1:{srv.server_address[1]}")
    srv.shutdown()


def test_unary_converts_names_and_int64(fake):
    p = fake.ports()[0]
    assert p == {"name": "eth1", "admin_up": True, "counters": {"rx_frames": 12, "tx_errors": 0}}
    path, ctype, body = Fake.calls[-1]
    assert path == "/clabgen.v1.AgentService/ListPorts" and ctype == "application/json" and json.loads(body) == {}


def test_timestamps(fake):
    assert fake.info()["start_time"] == datetime(2026, 10, 23, 16, 29, 23, 944123, tzinfo=timezone.utc)


def test_struct_fields_untouched(fake):
    # Protocol statuses keep the protocol's own keys.
    assert fake.status("p") == {"someKey": 1, "peers": [{"peerAs": 2}]}


def test_errors(fake):
    with pytest.raises(ClabgenError) as e:
        fake.delete_device("nope")
    assert e.value.code == "not_found" and "device nope" in str(e.value)


def test_unavailable():
    with pytest.raises(ClabgenError) as e:
        Agent("127.0.0.1:1", timeout=2).info()
    assert e.value.code == "unavailable"


def test_stream(fake):
    it = fake.events(replay=True)
    first = next(it)
    assert first["kind"] == "down" and first["time"].year == 2026
    assert next(it)["kind"] == "up"
    with pytest.raises(ClabgenError) as e:
        next(it)
    assert e.value.code == "canceled"
    _, ctype, body = Fake.calls[-1]
    assert ctype == "application/connect+json"
    flags, n = struct.unpack(">BI", body[:5])
    assert flags == 0 and json.loads(body[5:5 + n]) == {"replay": True}


@pytest.mark.parametrize("addr,url", [
    ("clab-b2b-gen1", "http://clab-b2b-gen1:8080"),
    ("10.0.0.1:9000", "http://10.0.0.1:9000"),
    ("2001:db8::1", "http://[2001:db8::1]:8080"),
    ("[2001:db8::1]:9000", "http://[2001:db8::1]:9000"),
    ("http://host:8090/agents/x", "http://host:8090/agents/x"),
])
def test_base_url(addr, url):
    assert _base_url(addr) == url


def test_vlan():
    assert [_vlan(v) for v in [100, "100", "100.5", "0x88a8:200", {"vid": 7}]] == [
        {"vid": 100}, {"vid": 100}, {"vid": 100, "pcp": 5}, {"tpid": 0x88A8, "vid": 200}, {"vid": 7}]


def test_merge():
    assert merge({"a": 1, "c": {"x": 1, "y": 2}}, {"b": 2, "c": {"y": 3}}) == {"a": 1, "b": 2, "c": {"x": 1, "y": 3}}


def test_from_wire_counters_map():
    # GetDeviceIP counters: a map of uint64s.
    assert from_wire({"counters": {"arp_rx": "3"}, "neighbors": [{"ageMs": "1500"}]}) == {
        "counters": {"arp_rx": 3}, "neighbors": [{"age_ms": 1500}]}
