# clabgen (Python)

Python client and pytest helpers for
[clabgen](https://github.com/ripnet/clabgen), the protocol emulator and
traffic generator for containerlab. Standard library only; Python 3.9+.

```sh
pip install clabgen
```

Installing from a checkout or from git builds the package: Ubuntu 22.04's
own pip 22.0 builds it as `UNKNOWN`, so use a newer pip there (e.g. in a
virtualenv after `pip install -U pip`).

```python
def test_failover(clabgen_lab):              # pytest --clabgen-lab b2b
    with clabgen_lab.measure() as m:
        clabgen_lab["gen2"].port_down("eth1")
        time.sleep(2)
        clabgen_lab["gen2"].port_up("eth1")
    m.assert_pass()
```

See [docs/python.md](https://github.com/ripnet/clabgen/blob/main/docs/python.md).

Tests: `pytest` here. The unit tests need nothing; the `agent` tests run
the agent binary (`make build` at the top, or `CLABGEN_AGENT=path`) in a
container, so they need docker, and are skipped without it.
