"""Against a real agent (see conftest.agent). Two devices on the same port
reach each other inside the agent, so flows run without a second node."""

import time

import pytest

from clabgen import ClabgenError, FlowsFailed, Lab

pytestmark = pytest.mark.agent

A_MAC, B_MAC = "02:00:00:00:0a:01", "02:00:00:00:0b:01"


def two_devices(agent, **kw):
    agent.add_device("a", "eth1", vlans=[100], mac=A_MAC, **kw.get("a", {}))
    agent.add_device("b", "eth1", vlans=[100], mac=B_MAC, **kw.get("b", {}))


def test_info_and_ports(agent):
    assert agent.info()["version"]
    p = agent.port("eth1")
    assert p["oper_up"] and isinstance(p["counters"].get("rx_frames", 0), int)


def test_devices(clean):
    d = clean.add_device("d", "eth1", vlans=["0x88a8:200", "10.3"], addresses=["10.0.0.1/24"])
    assert [v["vid"] for v in d["vlans"]] == [200, 10] and d["vlans"][0]["tpid"] == 0x88A8 and d["mac_derived"]
    d = clean.update_device("d", vlans=[300])
    assert [v["vid"] for v in d["vlans"]] == [300] and d["addresses"] == ["10.0.0.1/24"]
    assert "10.0.0.1/24" in clean.device_ip("d")["addresses"]
    clean.delete_device("d")
    with pytest.raises(ClabgenError) as e:
        clean.device("d")
    assert e.value.code == "not_found"


def test_protocols(clean):
    two_devices(clean)
    p = clean.add_protocol("s", "l2", "a", dst_mac=B_MAC, rate=50, checks={"max_loss_pct": 1})
    assert p["state"] == "running" and p["config"]["rate"] == 50
    # Merged onto the current config.
    p = clean.update_protocol("s", rate=20, checks={"max_outage_ms": 100})
    assert p["config"]["rate"] == 20 and p["config"]["dst_mac"] == B_MAC
    assert p["config"]["checks"] == {"max_loss_pct": 1, "max_outage_ms": 100}
    assert clean.disable("s")["state"] == "disabled"
    assert clean.enable("s")["state"] == "running"
    assert clean.run("s", "preview")
    assert [f["name"] for f in clean.flows()] == ["s"]
    with pytest.raises(ClabgenError) as e:
        clean.add_protocol("s", "l2", "a")
    assert e.value.code == "already_exists"


def test_scenario(clean):
    two_devices(clean)
    y = clean.scenario()
    assert "02:00:00:00:0a:01" in y
    clean.apply_scenario("devices: []\n", replace=True)
    assert clean.devices() == []
    clean.apply_scenario(y)
    assert {d["name"] for d in clean.devices()} == {"a", "b"}


def test_events(clean):
    events = clean.events(timeout=10)
    clean.add_device("ev", "eth1", vlans=[7])
    for e in events:
        if e["source"] == "device/ev":
            assert e["kind"] and e["time"].year >= 2026
            break


def echo_lab(agent):
    two_devices(agent)
    agent.add_protocol("echo", "l2-echo", "b")
    agent.add_protocol("rt", "l2", "a", dst_mac=B_MAC, reply="echo", rate=100, timeout="300ms", auto_start=True,
                       checks={"max_loss_pct": 1, "max_outage_ms": 100})
    agent.add_protocol("oneway", "l2", "a", dst_mac=B_MAC, rate=100, auto_start=True)
    return Lab({"gen": agent})


def test_measure_pass(clean):
    lab = echo_lab(clean)
    lab.wait_flows_pass(timeout=20)
    with lab.measure() as m:
        time.sleep(2)
    m.assert_pass()
    rt, oneway = m["rt"], m["gen/oneway"]
    assert rt.verdict == "pass" and rt.rx > 100 and rt.lost == 0 and rt.rtt_avg_ms is not None
    # One way: the receiver's numbers, no verdict without replies.
    assert oneway.one_way and oneway.rx > 100 and oneway.lost == 0 and oneway.received[0]["device"] == "b"
    assert oneway.verdict is None


def test_measure_fail(clean):
    lab = echo_lab(clean)
    lab.wait_flows_pass(timeout=20)
    with lab.measure() as m:
        clean.port_down("eth1")
        time.sleep(1)
        clean.port_up("eth1")
    with pytest.raises(FlowsFailed) as e:
        m.assert_pass()
    assert [r.id for r in e.value.failed] == ["gen/rt"]
    assert "outage_ms" in m["rt"].failed and m["rt"].lost > 50
    assert "gen/rt: fail" in str(e.value)
    # Failed verdicts stay until cleared; wait_flows_pass clears them.
    assert lab.flow_results(["rt"])[0].verdict == "fail"
    lab.wait_flows_pass(["rt"], timeout=20)


def test_start_stop(clean):
    lab = echo_lab(clean)
    lab.stop_flows()
    lab.stop_flows()  # already stopped: fine
    assert {r.state for r in lab.flow_results()} == {"idle"}
    lab.start_flows(["rt"])
    assert lab.flow_results(["rt"])[0].state == "running"
    with pytest.raises(KeyError):
        lab.flow_results(["nope"])
