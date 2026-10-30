# Python and pytest

The `clabgen` Python package ([`python/`](../python)) drives agents from
scripts and turns a lab into a test suite: clear the flows, do the thing
under test, and assert on what the flows measured. It talks to the agents'
[API](api.md) over plain HTTP and needs nothing beyond the standard
library (Python 3.9 or newer); pytest is only needed for the fixtures.

## Installing

```sh
pip install clabgen                     # the latest release
pip install "git+https://github.com/ripnet/clabgen@dev#subdirectory=python"   # the development branch
```

Its version follows the agent's releases; it works with agents of the
same release (an agent's API only grows, so a newer agent works too).

## A test

```python
import time

def test_failover(clabgen_lab):
    with clabgen_lab.measure() as m:      # clears every flow (senders and receivers)
        clabgen_lab["gen2"].port_down("eth1")
        time.sleep(2)
        clabgen_lab["gen2"].port_up("eth1")
    m.assert_pass()                       # every flow's checks passed
    assert m["ping-gen2"].outage_max_ms < 2600
```

```sh
pytest --clabgen-lab b2b                                  # lab on this host
pytest --clabgen-lab b2b --clabgen-ui http://host:8090    # through a clabgen-ui
```

- `clabgen_lab` is a `Lab` of the lab's agents by node name.
- The options can also come from `CLABGEN_LAB` and `CLABGEN_UI`. Tests
  that use the fixture are skipped when neither is set.
- [`examples/b2b/test_b2b.py`](../examples/b2b/test_b2b.py) has a complete
  suite, including a raw L2 circuit that the test builds and removes
  itself.

## Measuring flows

`measure()` clears the flows, runs the `with` block, waits `settle`
seconds (2 by default, at least the flows' reply timeout) and collects a
`FlowResult` per flow:

| Field | |
|---|---|
| `agent`, `name`, `id` | `id` is `agent/name` |
| `verdict` | `pass`, `fail`, `pending` (nothing measured yet), or `None` without checks |
| `failed` | the failed checks, e.g. `loss_pct 2.6 > 1, outage_ms 2030 > 50` |
| `tx`, `rx`, `lost`, `loss_pct`, `outage_max_ms`, `rtt_avg_ms` | what the flow measured |
| `one_way` | no replies: `rx`, `lost` and `loss_pct` are the receivers' totals |
| `received` | each receiver's one-way statistics (agent, device, latency, …) |
| `status` | the flow's whole status |

- `m.assert_pass()` raises `FlowsFailed` (an `AssertionError` listing the
  failed flows) when a flow failed its checks, or when an echo flow with
  checks measured nothing. `require_checks=True` also fails flows without
  checks.
- One-way flows have no verdict (an agent can't judge loss it doesn't
  see), so assert on their `lost` or `loss_pct`.
- Give `measure`, `flow_results`, `clear_flows`, `start_flows` and
  `stop_flows` a list of flows (`"agent/name"`, or `"name"` when it's
  unique) to work on some of them only.
- `wait_flows_pass()` waits until every flow with checks passes, for a lab
  that is still converging. It clears failed verdicts as it goes.

## Labs

```python
from clabgen import Agent, Lab

lab = Lab.from_docker("b2b")                    # containerlab lab on this host
lab = Lab.from_ui("http://host:8090", "b2b")    # agents behind a clabgen-ui
lab = Lab({"gen1": Agent("clab-b2b-gen1"), "gen2": Agent("10.0.0.2:8080")})
gen1 = lab["gen1"]
```

## Agents

An `Agent` has a method for everything `clabgenctl` does. Results are the
API's objects as dicts with snake_case keys; protocol configs and statuses
are as each protocol defines them (`clabgenctl types TYPE` shows them).

```python
gen1.add_device("v300", "eth1", vlans=[300], mac="02:00:00:00:30:01")
gen1.add_device("qinq", "eth2", vlans=["0x88a8:200", "10.5"])     # tag 10, PCP 5
gen1.add_protocol("ping", "icmp", "v100", destination="10.0.100.2", rate=100, auto_start=True,
                  checks={"max_loss_pct": 1})
gen1.update_protocol("ping", rate=50)          # merged onto the current config, in place where it can be
gen1.run("mep-v100", "loopback", target_mep_id=102, count=5)
gen1.status("bgp1")["peers"]
gen1.wait_protocol("bgp1", lambda p: p["status"]["peers"][0]["state"] == "established")
gen1.port_down("eth1"); gen1.port_up("eth1")
gen1.received()                                 # flows arriving at gen1's devices
gen1.run_group("ce", "flap", members="1-20", seconds=10)
gen1.preview_group(spec, members=[1, 57])       # a group spec rendered, not applied
gen1.apply_scenario(Path("gen1.yml"))
for e in gen1.events(replay=True):              # a stream of events
    print(e["time"], e["source"], e["message"])
```

### Packet flows

- `gen1.preview("v100", layers=[...], modifiers=[...], index=3)` returns
  the packet a config would send (hex, summary, layers with their fields)
  without creating anything. `pcap=` takes a capture's path or bytes.
- `pcap_data(path)` encodes a capture for a flow's config:
  `add_protocol("replay", "packet", "v100", pcap={"data": pcap_data("x.pcapng")})`.

### Packet capture

```python
for f in gen1.capture(["eth1"], count=10):      # running once capture() returns
    print(f["time"], f["direction"], f["summary"])   # f["data"]: the frame's bytes
gen1.capture_to_file("v100.pcapng", device="v100", seconds=10)
gen1.decode(frame_bytes)["layers"]
```

### Saved config and errors

- `gen1.info()["persistence"]` says where the agent saves its config (see
  [saved config](configuration.md#saved-config)), and
  `gen1.reset_config()` goes back to the topology's scenario.
- Errors from an agent raise `ClabgenError` with Connect's `code`
  (`not_found`, `invalid_argument`, `already_exists`, `unavailable`, …).
- `agent.call("Method", {...})` reaches any API method directly.
