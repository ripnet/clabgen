"""Against a real agent (see conftest.agent). Two devices on the same port
reach each other inside the agent, so flows run without a second node."""

import time

import pytest

from clabgen import ClabgenError, FlowsFailed, Lab, pcap_data

pytestmark = pytest.mark.agent

A_MAC, B_MAC = "02:00:00:00:0a:01", "02:00:00:00:0b:01"


def two_devices(agent, **kw):
    agent.add_device("a", agent.test_port, vlans=[100], mac=A_MAC, **kw.get("a", {}))
    agent.add_device("b", agent.test_port, vlans=[100], mac=B_MAC, **kw.get("b", {}))


def test_info_and_ports(agent):
    assert agent.info()["version"]
    p = agent.port(agent.test_port)
    assert p["oper_up"] and isinstance(p["counters"].get("rx_frames", 0), int)


def test_devices(clean):
    d = clean.add_device("d", clean.test_port, vlans=["0x88a8:200", "10.3"], addresses=["10.0.0.1/24"])
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


def test_preview_group(clean):
    spec = {"name": "g", "count": 30, "device": {"port": clean.test_port, "vlans": [{"vid": 300}]},
            "protocols": [{"type": "cfm-mep", "config": {"level": 3, "mep_id": {"start": 101}}}]}
    p = clean.preview_group(spec)
    assert p["count"] == 30 and [m["index"] for m in p["members"]] == [1, 2, 30] and "plan" not in p
    assert p["members"][2]["protocols"][0]["config"]["mep_id"] == 130
    assert clean.groups() == []
    clean.add_group(spec)
    p = clean.preview_group({**spec, "count": 32}, members=[31])
    assert [m["index"] for m in p["members"]] == [31] and p["plan"] == {"unchanged": 30, "changed": 0, "added": 2, "removed": 0}
    clean.delete_group("g")


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
    clean.add_device("ev", clean.test_port, vlans=[7])
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
        clean.port_down(clean.test_port)
        time.sleep(1)
        clean.port_up(clean.test_port)
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


def test_packet_builder(clean):
    two_devices(clean)
    p = clean.preview("a", layers=[{"ethernet": {"dst": B_MAC}}, {"vlan": {"vid": 5}}, {"payload": {"size": 30}}],
                      modifiers=[{"field": "vlan.vid", "mode": "increment"}], index=2)
    assert p["index"] == 2 and [l["title"] for l in p["layers"]] == ["Ethernet", "Device VLAN tags (1)", "EtherType", "VLAN", "Payload"]
    vlan = {f["name"]: f["value"] for f in p["layers"][3]["fields"]}
    assert vlan["vid"] == "7"
    # The previewed frame, replayed from a capture: b counts it (it is signed).
    frame = bytes.fromhex(clean.preview("a", layers=[{"ethernet": {"dst": B_MAC}}, {"payload": {"size": 46}}])["hex"])
    import struct
    cap = struct.pack("<IHHiIII", 0xA1B2C3D4, 2, 4, 0, 0, 65535, 1) + struct.pack("<IIII", 0, 0, len(frame), len(frame)) + frame
    assert clean.preview("a", pcap=cap)["frames"][0]["length"] == len(frame)
    clean.add_protocol("replay", "packet", "a", pcap={"data": pcap_data(cap)}, mode="count", count=20, rate=200, auto_start=True)
    clean.wait_protocol("replay", lambda x: x["status"]["state"] == "done", timeout=10)
    assert clean.status("replay")["tx"] == 20
    with pytest.raises(ClabgenError) as e:
        clean.preview("a", layers=[{"udp": {}}])
    assert "needs an IPv4 or IPv6 layer" in e.value.message


def test_capture(clean, tmp_path):
    two_devices(clean)
    frames = clean.capture(device="a", count=10, timeout=10)  # running once it returns
    clean.add_protocol("s", "l2", "a", dst_mac=B_MAC, rate=100, auto_start=True)
    got = list(frames)
    assert len(got) == 10 and all(f["port"] == clean.test_port and f["direction"] == "tx" for f in got)
    f = got[0]
    assert f["data"][:6] == bytes.fromhex(B_MAC.replace(":", "")) and f["length"] == len(f["data"])
    assert "experimental 0x88b5" in f["summary"] and f["time"].year >= 2026
    d = clean.decode(f["data"])
    assert d["summary"] == f["summary"] and [l["title"] for l in d["layers"]][:2] == ["Ethernet", "VLAN"]

    path = tmp_path / "x.pcapng"
    n = clean.capture_to_file(path, [clean.test_port], snaplen=32, count=5)
    raw = path.read_bytes()
    assert n == len(raw) and raw[:4] == bytes.fromhex("0a0d0d0a") and raw.count(b"clabgen") >= 1
    # The agent reads its own capture back as a packet replay.
    p = clean.preview("a", pcap=raw)
    assert len(p["frames"]) == 5 and all(x["length"] == 32 for x in p["frames"])
    with pytest.raises(ClabgenError):
        clean.capture_to_file(path, ["nope"], count=1)
    with pytest.raises(ClabgenError):
        next(clean.capture(["nope"]))


def test_saved_config(clean):
    # The test container is no containerlab node: nothing saved, and why.
    cfg = clean.info()["persistence"]
    assert cfg.get("file", "") == "" and "not a containerlab node" in cfg["not_saved"]
    clean.add_device("r", clean.test_port, vlans=[5])
    # Reset with no scenario: back to empty.
    assert clean.reset_config()["source"] == "empty" and clean.devices() == []
