import pytest

from clabgen import ClabgenError, Convergence, ConvergenceFailed, ConvergenceFlow, Lab
from clabgen.lab import _merge_convergence


def row(**over):
    return {"side": "sender", "device": "d", "one_way": False, "running": True, "status": "no_impact",
            "loss_ms": 0.0, "lost": 0, "clean_ms": 60000.0, "resolution_ms": 1.0, "outages": [], **over}


def test_merge_picks_each_flows_measurement():
    rows = [
        row(name="echo", flow_id="a", status="restored"),
        row(side="receiver", flow_id="a", one_way=True),  # the echo's outbound leg
        row(name="oneway", flow_id="b", one_way=True),
        row(side="receiver", device="far", flow_id="b", one_way=True, status="restored", lost=5),
        row(name="nobody", flow_id="c", one_way=True),
        row(name="halted", flow_id="d", one_way=True, running=False, status="stopped"),
        row(side="receiver", device="far", flow_id="d", one_way=True, status="not_restored"),
        row(side="receiver", device="far", flow_id="e", one_way=True),  # sender elsewhere
    ]
    got = {f["flow_id"]: f for f in _merge_convergence(rows)}
    assert len(got) == 5
    assert got["a"]["side"] == "sender"
    assert (got["b"]["side"], got["b"]["name"], got["b"]["lost"]) == ("receiver", "oneway", 5)
    assert got["c"]["status"] == "no_receiver"
    assert (got["d"]["side"], got["d"]["status"]) == ("receiver", "stopped")


def _result(**over):
    return ConvergenceFlow._of(row(name="x", agent="a", flow_id="1", **over))


def test_summary_and_assertions():
    c = Convergence()
    c.flows = [_result(status="restored", loss_ms=100.0, lost=100), _result(), _result(status="restored", loss_ms=300.0, lost=300),
               _result(status="stopped", loss_ms=9999.0)]
    assert (c.worst_ms, c.median_ms, c.p95_ms) == (300.0, 100.0, 300.0)
    c.assert_restored()
    c.assert_restored(max_ms=300)
    with pytest.raises(ConvergenceFailed, match="limit 200 ms"):
        c.assert_restored(max_ms=200)
    c.flows.append(_result(status="not_restored"))
    with pytest.raises(ConvergenceFailed, match="not restored"):
        c.assert_restored()


class FakeAgent:
    """Answers GetConvergence: clean before the failure, then a loss."""

    def __init__(self, name, before, after):
        self.name, self.before, self.after = name, before, after
        self.asked = []

    def flows(self):
        return [{"name": r["name"]} for r in self.before if r.get("side") == "sender"]

    def convergence(self, since=None, flow_ids=()):
        self.asked.append(since)
        return {"now": "T0", "flows": self.after if since else self.before, "events": [{"time": "T1", "message": "m"}] if since else []}


def test_measures_from_the_agents_moment():
    before = [row(name="p", flow_id="1")]
    after = [row(name="p", flow_id="1", status="restored", loss_ms=42.0, lost=42, first_loss_ms=1.0, last_loss_ms=43.0)]
    a = FakeAgent("a1", before, after)
    with Lab({"a1": a}).convergence(settle=1) as c:
        pass
    assert c["p"].loss_ms == 42.0 and c["p"].lost == 42 and c["p"].last_loss_ms == 43.0
    assert c.events[0]["agent"] == "a1"
    assert "T0" in a.asked


def test_refuses_flows_that_arent_clean():
    a = FakeAgent("a1", [row(name="p", flow_id="1", status="restored", lost=7)], [])
    with pytest.raises(ClabgenError, match="lost 7 packets"):
        with Lab({"a1": a}).convergence():
            pass
    b = FakeAgent("a1", [row(name="p", flow_id="1", running=False, status="stopped")], [])
    with pytest.raises(ClabgenError, match="no flows running"):
        with Lab({"a1": b}).convergence():
            pass
