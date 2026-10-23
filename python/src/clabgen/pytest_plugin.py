"""pytest fixtures for clabgen labs (loaded automatically once installed).

    pytest --clabgen-lab b2b                      # a containerlab lab on this host
    pytest --clabgen-lab b2b --clabgen-ui http://host:8090   # through a clabgen-ui

    def test_failover(clabgen_lab):
        with clabgen_lab.measure() as m:
            clabgen_lab["gen2"].port_down("eth1")
            ...
        m.assert_pass()

Tests using clabgen_lab are skipped when no lab is given.
"""

from __future__ import annotations

import os

import pytest

from .lab import Lab


def pytest_addoption(parser: pytest.Parser) -> None:
    g = parser.getgroup("clabgen")
    g.addoption("--clabgen-lab", default=None, help="containerlab lab whose clabgen agents tests use (env CLABGEN_LAB)")
    g.addoption("--clabgen-ui", default=None, help="reach the agents through this clabgen-ui, e.g. http://host:8090 (env CLABGEN_UI)")


def _opt(config: pytest.Config, name: str) -> str | None:
    return config.getoption(name) or os.environ.get(name.lstrip("-").replace("-", "_").upper())


@pytest.fixture(scope="session")
def clabgen_lab(pytestconfig: pytest.Config) -> Lab:
    """The lab's agents (clabgen.Lab), by node name."""
    lab, ui = _opt(pytestconfig, "--clabgen-lab"), _opt(pytestconfig, "--clabgen-ui")
    if not lab and not ui:
        pytest.skip("no clabgen lab (--clabgen-lab or CLABGEN_LAB)")
    return Lab.from_ui(ui, lab) if ui else Lab.from_docker(lab)
