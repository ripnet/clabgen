"""One clabgen agent: everything clabgenctl and the web UI can do."""

from __future__ import annotations

import copy
import time
from pathlib import Path
from typing import Any, Callable, Iterable, Iterator

from ._connect import ClabgenError, Transport


def _base_url(address: str) -> str:
    if "://" in address:
        return address
    host, sep, port = address.rpartition(":")
    if not sep or not port.isdigit() or host.endswith(":"):  # no port, or a bare IPv6 address
        host, port = address, "8080"
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    return f"http://{host}:{port}"


def _vlan(v: Any) -> dict:
    """A VLAN tag from 100, "100", "100.5" (PCP), "0x88a8:200" or a dict."""
    if isinstance(v, dict):
        return v
    if isinstance(v, int):
        return {"vid": v}
    s, tag = str(v), {}
    if ":" in s:
        tpid, s = s.split(":", 1)
        tag["tpid"] = int(tpid, 0)
    if "." in s:
        s, pcp = s.split(".", 1)
        tag["pcp"] = int(pcp)
    tag["vid"] = int(s)
    return tag


def merge(dst: dict, src: dict) -> dict:
    """Deep-merge src onto dst (nested dicts merged, everything else replaced)."""
    for k, v in src.items():
        if isinstance(v, dict) and isinstance(dst.get(k), dict):
            merge(dst[k], v)
        else:
            dst[k] = v
    return dst


def wait_until(check: Callable[[], Any], timeout: float = 30, interval: float = 0.5, what: str = "condition") -> Any:
    """Call check until it returns something truthy (returned), or raise
    TimeoutError after timeout seconds. Errors from check propagate."""
    end = time.monotonic() + timeout
    while True:
        v = check()
        if v:
            return v
        if time.monotonic() >= end:
            raise TimeoutError(f"timed out after {timeout:g}s waiting for {what}")
        time.sleep(interval)


class Agent:
    """A clabgen agent's API.

    address is host[:port] (port 8080 by default), e.g. "clab-b2b-gen1", or a
    URL: "http://172.20.20.2:8080", or an agent behind clabgen-ui,
    "http://host:8090/agents/clab-b2b-gen1".

    Methods return the API's objects as dicts with snake_case keys. Protocol
    configs and statuses are as each protocol type defines them (see
    `types()` or `clabgenctl types TYPE`).
    """

    def __init__(self, address: str, timeout: float = 10.0, name: str | None = None):
        self.url = _base_url(address)
        self.name = name or address
        self._t = Transport(self.url, timeout)
        self._traffic: set[str] | None = None

    def __repr__(self) -> str:
        return f"Agent({self.name!r}, url={self.url!r})"

    def call(self, method: str, request: dict | None = None, **kw: Any) -> dict:
        """Any AgentService method by name, e.g. call("ListPorts")."""
        return self._t.call(method, request, **kw)

    # Agent and ports.

    def info(self) -> dict:
        """hostname, version and start_time."""
        return self.call("GetInfo")

    def ports(self) -> list[dict]:
        return self.call("ListPorts").get("ports", [])

    def port(self, name: str) -> dict:
        for p in self.ports():
            if p["name"] == name:
                return p
        raise ClabgenError("not_found", f"port {name}", "ListPorts")

    def set_port(self, name: str, up: bool) -> dict:
        return self.call("SetPortAdmin", {"name": name, "up": up})["port"]

    def port_down(self, name: str) -> dict:
        return self.set_port(name, False)

    def port_up(self, name: str) -> dict:
        return self.set_port(name, True)

    # Devices.

    def devices(self) -> list[dict]:
        return self.call("ListDevices").get("devices", [])

    def device(self, name: str) -> dict:
        for d in self.devices():
            if d["name"] == name:
                return d
        raise ClabgenError("not_found", f"device {name}", "ListDevices")

    def add_device(self, name: str, port: str = "eth1", *, vlans: Iterable[Any] = (), mac: str = "",
                   addresses: Iterable[str] = (), gateway4: str = "", gateway6: str = "") -> dict:
        """Create a device. vlans are outermost first: 100, "100.5" (with PCP),
        "0x88a8:200" or {"tpid":…, "vid":…, "pcp":…}. An empty mac is derived."""
        d = {"name": name, "port": port, "mac": mac, "vlans": [_vlan(v) for v in vlans],
             "addresses": list(addresses), "gateway4": gateway4, "gateway6": gateway6}
        return self.call("CreateDevice", {"device": d})["device"]

    def update_device(self, name: str, **changes: Any) -> dict:
        """Change some of a device's fields (port, mac, vlans, addresses,
        gateway4, gateway6); the rest stay as they are."""
        d = self.device(name)
        if "vlans" in changes:
            changes["vlans"] = [_vlan(v) for v in changes["vlans"]]
        d.update(changes)
        for k in ("mac_derived", "group"):
            d.pop(k, None)
        return self.call("UpdateDevice", {"device": d})["device"]

    def delete_device(self, name: str) -> None:
        self.call("DeleteDevice", {"name": name})

    def device_ip(self, name: str) -> dict:
        """Effective addresses, gateways, neighbors and IP counters."""
        return self.call("GetDeviceIP", {"name": name})

    def resolve(self, device: str, ip: str) -> dict:
        """ARP/ND for ip (or its gateway): next_hop, mac, cached, rtt_us."""
        return self.call("ResolveNeighbor", {"device": device, "ip": ip})

    def clear_neighbors(self, device: str) -> None:
        self.call("ClearNeighbors", {"device": device})

    # Protocols (including traffic flows).

    def types(self) -> list[dict]:
        """Protocol types with their config and action schemas (JSON strings)."""
        return self.call("ListProtocolTypes").get("types", [])

    def protocols(self, type: str | None = None, *, status: bool = True, brief: bool = False,
                  group_members: bool = True) -> list[dict]:
        r = self.call("ListProtocols", {"with_status": status, "brief": brief, "no_group_members": not group_members})
        ps = r.get("protocols", [])
        return [p for p in ps if p["type"] == type] if type else ps

    def protocol(self, name: str) -> dict:
        return self.call("GetProtocol", {"name": name})["protocol"]

    def status(self, name: str) -> dict:
        """A protocol's live status."""
        return self.protocol(name).get("status", {})

    def add_protocol(self, name: str, type: str, device: str = "", config: dict | None = None, **fields: Any) -> dict:
        """Create a protocol instance; keyword arguments are config fields
        (merged onto config), e.g. add_protocol("p", "icmp", "d1", destination="10.0.0.2", rate=50)."""
        cfg = merge(copy.deepcopy(config or {}), fields)
        return self.call("CreateProtocol", {"protocol": {"name": name, "type": type, "device": device, "config": cfg}})["protocol"]

    def update_protocol(self, name: str, config: dict | None = None, *, replace: bool = False, **fields: Any) -> dict:
        """Change a protocol's config and restart it. By default config and the
        keyword fields are merged onto the current config (like clabgenctl
        proto set); replace=True sends them as the whole config."""
        changes = merge(copy.deepcopy(config or {}), fields)
        cfg = changes if replace else merge(copy.deepcopy(self.protocol(name).get("config", {})), changes)
        return self.call("UpdateProtocol", {"name": name, "config": cfg})["protocol"]

    def delete_protocol(self, name: str) -> None:
        self.call("DeleteProtocol", {"name": name})

    def disable(self, name: str) -> dict:
        """Stop a protocol but keep its config (state "disabled")."""
        return self.call("SetProtocolDisabled", {"name": name, "disabled": True})["protocol"]

    def enable(self, name: str) -> dict:
        return self.call("SetProtocolDisabled", {"name": name, "disabled": False})["protocol"]

    def run(self, name: str, action: str, args: dict | None = None, **fields: Any) -> dict:
        """Run a protocol action, e.g. run("mep1", "loopback", target_mep_id=2, count=5)."""
        a = merge(copy.deepcopy(args or {}), fields)
        return self.call("RunProtocolAction", {"name": name, "action": action, "args": a}).get("result", {})

    # Traffic flows.

    def traffic_types(self) -> set[str]:
        """Names of the protocol types that are traffic flows (icmp, udp, l2, …)."""
        if self._traffic is None:
            self._traffic = {t["name"] for t in self.types() if t.get("category") == "traffic"}
        return self._traffic

    def flows(self) -> list[dict]:
        """Traffic flow instances with their statuses."""
        traffic = self.traffic_types()
        return [p for p in self.protocols(group_members=False) if p["type"] in traffic]

    def start(self, *flows: str) -> None:
        for f in flows:
            self.run(f, "start")

    def stop(self, *flows: str) -> None:
        for f in flows:
            self.run(f, "stop")

    def received(self, device: str = "") -> list[dict]:
        """Flows received by this agent's devices: one dict per flow and
        device, with "device" and the one-way statistics (rx, lost, loss_pct,
        reordered, duplicate, latency_*_us, outages, outage_max_ms, …)."""
        flows = self.call("ListReceivedFlows", {"device": device}).get("flows", [])
        return [{"device": f["device"], **f.get("stats", {})} for f in flows]

    def clear_received(self, device: str = "", flow_id: str = "") -> None:
        self.call("ClearReceivedFlows", {"device": device, "flow_id": flow_id})

    # Device groups.

    def groups(self) -> list[dict]:
        return self.call("ListDeviceGroups").get("groups", [])

    def group(self, name: str, members: bool = True) -> dict:
        return self.call("GetDeviceGroup", {"name": name, "summary_only": not members})["group"]

    def add_group(self, spec: dict | str) -> dict:
        """Create a device group from a spec dict or its YAML."""
        g = {"yaml": spec} if isinstance(spec, str) else {"name": spec["name"], "spec": spec}
        return self.call("CreateDeviceGroup", {"group": g})["group"]

    def update_group(self, spec: dict | str) -> dict:
        g = {"yaml": spec} if isinstance(spec, str) else {"name": spec["name"], "spec": spec}
        return self.call("UpdateDeviceGroup", {"group": g})["group"]

    def delete_group(self, name: str) -> None:
        self.call("DeleteDeviceGroup", {"name": name})

    def run_group(self, group: str, action: str, args: dict | None = None, *, protocol: str = "",
                  members: str = "", random: int = 0, **fields: Any) -> list[dict]:
        """Run an action on a group protocol's members: members is "" (all) or
        1-based indices and ranges like "1-10,15"; random picks that many of them."""
        a = merge(copy.deepcopy(args or {}), fields)
        r = self.call("RunGroupAction", {"group": group, "protocol": protocol, "action": action, "args": a,
                                         "members": members, "random": random})
        return r.get("results", [])

    def set_group_disabled(self, group: str, disabled: bool, *, protocol: str = "", members: str = "",
                           random: int = 0) -> list[dict]:
        r = self.call("SetGroupDisabled", {"group": group, "protocol": protocol, "members": members,
                                           "random": random, "disabled": disabled})
        return r.get("results", [])

    # Scenario and events.

    def scenario(self) -> str:
        """The whole config as YAML."""
        return self.call("GetScenario").get("yaml", "")

    def apply_scenario(self, scenario: str | Path, replace: bool = False) -> None:
        """Apply a scenario: YAML text, or a Path to a YAML file. replace=True
        removes existing devices and protocols first. Without it, items that
        already exist fail: everything else is applied, then ClabgenError
        lists what failed."""
        if isinstance(scenario, Path):
            scenario = scenario.read_text()
        self.call("ApplyScenario", {"yaml": scenario, "replace": replace}, timeout=60)

    def events(self, replay: bool = False, timeout: float | None = None) -> Iterator[dict]:
        """Stream events (time, source, kind, message, data) as they happen;
        replay=True starts with the buffered recent ones. timeout bounds each
        wait for the next event (TimeoutError / socket.timeout when it passes)."""
        return self._t.stream("WatchEvents", {"replay": replay}, timeout=timeout)

    # Waiting.

    def wait_protocol(self, name: str, check: Callable[[dict], Any], timeout: float = 30,
                      interval: float = 0.5, what: str = "") -> dict:
        """Poll a protocol until check(protocol) is truthy; returns the protocol.
        E.g. wait_protocol("bgp1", lambda p: p["status"]["peers"][0]["state"] == "established")."""
        def once() -> dict | None:
            p = self.protocol(name)
            return p if check(p) else None
        return wait_until(once, timeout, interval, what or f"{self.name}/{name}")
