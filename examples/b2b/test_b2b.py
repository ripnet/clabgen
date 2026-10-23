"""Tests for the b2b lab, with the clabgen Python package (python/):

    pip install "git+https://github.com/ripnet/clabgen#subdirectory=python"
    pytest examples/b2b --clabgen-lab b2b
"""

import time

import pytest

from clabgen import ClabgenError

A_MAC, B_MAC = "02:00:00:00:91:01", "02:00:00:00:91:02"


def test_flows_clean(clabgen_lab):
    """Nothing lost on any flow for 3 s."""
    with clabgen_lab.measure() as m:
        time.sleep(3)
    m.assert_pass()
    for r in m.results:
        assert r.rx > 0 and r.lost == 0, r.summary()


def test_ping_measures_a_cut(clabgen_lab):
    """A 2 s cut of the untagged link shows as a ~2 s outage on the ping."""
    gen2 = clabgen_lab["gen2"]
    with clabgen_lab.measure(["gen1/ping-gen2"]) as m:
        gen2.port_down("eth1")
        time.sleep(2)
        gen2.port_up("eth1")
    r = m["ping-gen2"]
    assert 1800 <= r.outage_max_ms <= 2600, r.summary()


@pytest.fixture
def l2_circuit(clabgen_lab):
    """A raw L2 echo stream over eth2 VLAN 901 (no IP), from 16 source MACs."""
    gen1, gen2 = clabgen_lab["gen1"], clabgen_lab["gen2"]
    gen1.add_device("t-l2a", "eth2", vlans=[901], mac=A_MAC)
    gen2.add_device("t-l2b", "eth2", vlans=[901], mac=B_MAC)
    gen2.add_protocol("t-echo", "l2-echo", "t-l2b")
    gen1.add_protocol("t-l2", "l2", "t-l2a", dst_mac=B_MAC, src_mac="02:00:00:01:00:00", src_mac_count=16,
                      reply="echo", rate=200, timeout="300ms", auto_start=True,
                      checks={"max_loss_pct": 0.5, "max_outage_ms": 50})
    clabgen_lab.wait_flows_pass(["gen1/t-l2"], timeout=15)
    yield "gen1/t-l2"
    for agent, kind, name in [(gen1, "protocol", "t-l2"), (gen2, "protocol", "t-echo"),
                              (gen1, "device", "t-l2a"), (gen2, "device", "t-l2b")]:
        try:
            getattr(agent, f"delete_{kind}")(name)
        except ClabgenError:
            pass


def test_l2_circuit_clean(clabgen_lab, l2_circuit):
    with clabgen_lab.measure([l2_circuit]) as m:
        time.sleep(3)
    m.assert_pass()
    assert m[l2_circuit].rtt_avg_ms < 5


def test_l2_circuit_cut_fails_its_checks(clabgen_lab, l2_circuit):
    with clabgen_lab.measure([l2_circuit]) as m:
        clabgen_lab["gen2"].port_down("eth2")
        time.sleep(1)
        clabgen_lab["gen2"].port_up("eth2")
    r = m[l2_circuit]
    assert r.verdict == "fail" and "outage_ms" in r.failed, r.summary()
    assert 900 <= r.outage_max_ms <= 1600, r.summary()
