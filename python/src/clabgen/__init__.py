"""Python client for clabgen agents, with helpers for pytest.

    from clabgen import Lab

    lab = Lab.from_docker("b2b")
    with lab.measure() as m:
        lab["gen2"].port_down("eth1")
        ...
    m.assert_pass()
"""

from ._connect import ClabgenError
from .agent import Agent, merge, wait_until
from .lab import FlowResult, FlowsFailed, Lab, Measurement

__version__ = "0.2.0"

__all__ = ["Agent", "ClabgenError", "FlowResult", "FlowsFailed", "Lab", "Measurement", "merge", "wait_until", "__version__"]
