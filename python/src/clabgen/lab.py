"""A lab: several agents, and traffic flows judged across all of them."""

from __future__ import annotations

import json
import subprocess
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import Any, Iterable, Iterator, Mapping

from ._connect import ClabgenError
from .agent import Agent, wait_until


@dataclass
class FlowResult:
    """One traffic flow's measurement.

    verdict is "pass", "fail" or "pending" (nothing measured yet), or None
    when the flow has no checks. failed names the failed checks, e.g.
    "loss_pct 2.6 > 1, outage_ms 2030 > 50". For one-way flows (no replies)
    the agent can't judge loss, so rx, lost and loss_pct are the receivers'
    totals and the verdict stays "pending".
    """

    agent: str
    name: str
    type: str
    device: str
    state: str
    flow_id: str
    one_way: bool
    verdict: str | None
    failed: str
    tx: int
    rx: int
    lost: int
    loss_pct: float
    outage_max_ms: float
    rtt_avg_ms: float | None
    status: dict = field(repr=False)
    received: list[dict] = field(default_factory=list, repr=False)

    @property
    def id(self) -> str:
        return f"{self.agent}/{self.name}"

    @property
    def passed(self) -> bool:
        return self.verdict == "pass"

    def summary(self) -> str:
        rtt = f" rtt {self.rtt_avg_ms:.2f} ms" if self.rtt_avg_ms is not None else ""
        v = self.verdict or "no checks"
        if self.failed:
            v += f" ({self.failed})"
        way = " one way" if self.one_way else ""
        return (f"{self.id}: {v}; tx {self.tx} rx {self.rx} lost {self.lost} ({self.loss_pct:.2f}%)"
                f" outage {self.outage_max_ms:g} ms{rtt}{way}")


def _result(agent: str, p: dict, received: list[dict]) -> FlowResult:
    s = p.get("status") or {}
    v = s.get("verdict") or {}
    one_way = s.get("tracks_replies") is False
    rx, lost, loss_pct = s.get("rx", 0), s.get("lost", 0), s.get("loss_pct", 0.0)
    outage = s.get("outage_max_ms", 0.0)
    if one_way:
        rx = sum(r.get("rx", 0) for r in received)
        lost = sum(r.get("lost", 0) for r in received)
        loss_pct = 100.0 * lost / (rx + lost) if rx + lost else 0.0
        outage = max([r.get("outage_max_ms", 0.0) for r in received], default=0.0)
    rtt = s.get("rtt") or {}
    return FlowResult(
        agent=agent, name=p["name"], type=p["type"], device=p.get("device", ""), state=s.get("state", p.get("state", "")),
        flow_id=s.get("flow_id", ""), one_way=one_way, verdict=v.get("result"), failed=v.get("failed", ""),
        tx=s.get("tx", 0), rx=rx, lost=lost, loss_pct=loss_pct, outage_max_ms=outage,
        rtt_avg_ms=rtt["avg_us"] / 1000 if "avg_us" in rtt else None, status=s, received=received,
    )


class FlowsFailed(AssertionError):
    def __init__(self, failed: list[FlowResult], results: list[FlowResult]):
        lines = [f"{len(failed)} of {len(results)} flows failed:"] + ["  " + r.summary() for r in failed]
        super().__init__("\n".join(lines))
        self.failed = failed
        self.results = results


class Measurement:
    """What Lab.measure() measured; results is set when the with block ends."""

    def __init__(self, lab: "Lab", flows: list[tuple[str, str]]):
        self.lab = lab
        self.flows = flows
        self.started = time.time()
        self.results: list[FlowResult] = []

    def __getitem__(self, flow: str) -> FlowResult:
        matches = [r for r in self.results if flow in (r.id, r.name)]
        if len(matches) != 1:
            raise KeyError(f"{flow!r}: {'no flow' if not matches else 'ambiguous, use agent/name'}")
        return matches[0]

    @property
    def failed(self) -> list[FlowResult]:
        return _failures(self.results, False)

    def assert_pass(self, require_checks: bool = False) -> None:
        """Raise FlowsFailed (an AssertionError) if a flow failed its checks,
        or an echo flow with checks measured nothing; require_checks=True
        also fails flows without checks."""
        failed = _failures(self.results, require_checks)
        if failed:
            raise FlowsFailed(failed, self.results)


def _failures(results: list[FlowResult], require_checks: bool) -> list[FlowResult]:
    out = []
    for r in results:
        if r.verdict == "fail" or (r.verdict == "pending" and not r.one_way) or (r.verdict is None and require_checks):
            out.append(r)
    return out


def _docker_agents(lab: str) -> dict[str, str]:
    ids = subprocess.run(["docker", "ps", "-q", "--filter", f"label=containerlab={lab}"],
                         check=True, capture_output=True, text=True).stdout.split()
    if not ids:
        return {}
    out = {}
    for c in json.loads(subprocess.run(["docker", "inspect", *ids], check=True, capture_output=True, text=True).stdout):
        name = c["Config"]["Labels"].get("clab-node-name") or c["Name"].lstrip("/")
        ips = [n.get("IPAddress") for n in c["NetworkSettings"]["Networks"].values() if n.get("IPAddress")]
        if ips:
            out[name] = ips[0]
    return out


class Lab(Mapping[str, Agent]):
    """Agents by name. Build one from a containerlab lab on this host
    (Lab.from_docker("b2b")), from a clabgen-ui (Lab.from_ui(url, "b2b")),
    or from agents: Lab({"gen1": Agent("clab-b2b-gen1"), …})."""

    def __init__(self, agents: Mapping[str, Agent] | Iterable[Agent]):
        if isinstance(agents, Mapping):
            self.agents = dict(agents)
        else:
            self.agents = {a.name: a for a in agents}
        for name, a in self.agents.items():
            a.name = name

    @classmethod
    def from_docker(cls, lab: str, timeout: float = 10.0) -> "Lab":
        """The clabgen agents of a running containerlab lab, by node name
        (nodes that don't answer the agent API are left out)."""
        nodes = _docker_agents(lab)

        def probe(item: tuple[str, str]) -> Agent | None:
            a = Agent(item[1], timeout=timeout, name=item[0])
            try:
                a.call("GetInfo", timeout=2)
                return a
            except ClabgenError:
                return None

        with ThreadPoolExecutor(max_workers=16) as ex:
            found = [a for a in ex.map(probe, sorted(nodes.items())) if a]
        if not found:
            raise ClabgenError("not_found", f"no clabgen agents in lab {lab!r}")
        return cls(found)

    @classmethod
    def from_ui(cls, url: str, lab: str | None = None, timeout: float = 10.0) -> "Lab":
        """Agents known to a clabgen-ui (e.g. http://host:8090), reached
        through its proxy; lab picks one lab's. Names are node names, or
        agent IDs when that would be ambiguous."""
        url = url.rstrip("/")
        with urllib.request.urlopen(f"{url}/api/agents", timeout=timeout) as r:
            agents = [a for a in json.load(r).get("agents", []) if lab is None or a.get("lab") == lab]
        if not agents:
            raise ClabgenError("not_found", f"no agents{f' in lab {lab!r}' if lab else ''} at {url}")
        names = [a.get("name") or a["id"] for a in agents]
        unique = len(set(names)) == len(names)
        return cls({(n if unique else a["id"]): Agent(f"{url}/agents/{a['id']}", timeout=timeout)
                    for n, a in zip(names, agents)})

    def __getitem__(self, name: str) -> Agent:
        return self.agents[name]

    def __iter__(self) -> Iterator[str]:
        return iter(self.agents)

    def __len__(self) -> int:
        return len(self.agents)

    def __repr__(self) -> str:
        return f"Lab({list(self.agents)})"

    def _each(self, fn: Any) -> dict[str, Any]:
        with ThreadPoolExecutor(max_workers=max(1, len(self.agents))) as ex:
            futs = {n: ex.submit(fn, a) for n, a in self.agents.items()}
            return {n: f.result() for n, f in futs.items()}

    # Flows.

    def _select(self, flows: Iterable[str] | None) -> list[tuple[str, str]]:
        all_flows = [(n, p["name"]) for n, ps in self._each(lambda a: a.flows()).items() for p in ps]
        if flows is None:
            return all_flows
        out = []
        for f in [flows] if isinstance(flows, str) else flows:
            m = [x for x in all_flows if f in (f"{x[0]}/{x[1]}", x[1])]
            if len(m) != 1:
                raise KeyError(f"{f!r}: {'no such flow' if not m else 'ambiguous, use agent/name'}")
            out.append(m[0])
        return out

    def flow_results(self, flows: Iterable[str] | None = None) -> list[FlowResult]:
        """Every flow (or the ones named "agent/name" or "name"), with what
        it measured since its start or last clear."""
        sel = set(self._select(flows))
        protos = self._each(lambda a: a.flows())
        received: dict[str, list[dict]] = {}
        for n, rs in self._each(lambda a: a.received()).items():
            for r in rs:
                received.setdefault(r.get("flow_id", ""), []).append({"agent": n, **r})
        out = []
        for n, ps in protos.items():
            for p in ps:
                if (n, p["name"]) in sel:
                    fid = (p.get("status") or {}).get("flow_id", "")
                    out.append(_result(n, p, received.get(fid, []) if fid else []))
        return out

    def clear_flows(self, flows: Iterable[str] | None = None) -> None:
        """Reset flows' statistics at the senders and at every receiver."""
        sel = set(self._select(flows))
        ids = set()
        for n, ps in self._each(lambda a: a.flows()).items():
            for p in ps:
                if (n, p["name"]) in sel:
                    self.agents[n].run(p["name"], "clear")
                    ids.add((p.get("status") or {}).get("flow_id", ""))
        for fid in ids - {""}:
            self._each(lambda a, fid=fid: a.clear_received(flow_id=fid))

    def start_flows(self, flows: Iterable[str] | None = None) -> None:
        for n, name in self._select(flows):
            try:
                self.agents[n].run(name, "start")
            except ClabgenError as e:
                if "already running" not in e.message:
                    raise

    def stop_flows(self, flows: Iterable[str] | None = None) -> None:
        for n, name in self._select(flows):
            try:
                self.agents[n].run(name, "stop")
            except ClabgenError as e:
                if "not running" not in e.message:
                    raise

    @contextmanager
    def measure(self, flows: Iterable[str] | None = None, settle: float = 2.0) -> Iterator[Measurement]:
        """Clear flows, run the with block (the test: flap a link, fail a
        node, …), wait settle seconds for packets in flight to be answered or
        time out, then collect the results:

            with lab.measure() as m:
                lab["gen2"].port_down("eth1"); time.sleep(2); lab["gen2"].port_up("eth1")
            m.assert_pass()

        settle should be at least the flows' reply timeout (1 s by default).
        """
        sel = self._select(flows)
        names = [f"{n}/{f}" for n, f in sel]
        if sel:
            self.clear_flows(names)
        m = Measurement(self, sel)
        yield m
        time.sleep(settle)
        m.results = self.flow_results(names) if sel else []

    def wait_flows_pass(self, flows: Iterable[str] | None = None, timeout: float = 60, interval: float = 1) -> list[FlowResult]:
        """Wait until every flow with checks passes, e.g. while a lab
        converges after deploying. A failed verdict stays until the flow is
        cleared, so failed flows are cleared each round: this returns once
        all of them have passed since their last failure."""
        last: list[FlowResult] = []

        def ok() -> bool:
            nonlocal last
            last = self.flow_results(flows)
            if failed := [r.id for r in last if r.verdict == "fail"]:
                self.clear_flows(failed)
            return not [r for r in last if r.verdict is not None and r.verdict != "pass" and not r.one_way]
        try:
            wait_until(ok, timeout, interval, "flows to pass")
        except TimeoutError as e:
            bad = [r for r in last if r.verdict is not None and r.verdict != "pass" and not r.one_way]
            raise TimeoutError(f"{e}:\n" + "\n".join("  " + r.summary() for r in bad)) from None
        return last
