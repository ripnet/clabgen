import os
import shutil
import subprocess
import uuid
from pathlib import Path

import pytest

from clabgen import Agent, ClabgenError, wait_until

AGENT_BIN = Path(os.environ.get("CLABGEN_AGENT", Path(__file__).resolve().parents[2] / "bin" / "clabgen-agent"))
IMAGE = os.environ.get("CLABGEN_TEST_IMAGE", "alpine:3.20")


def _docker(*args: str) -> str:
    return subprocess.run(["docker", *args], check=True, capture_output=True, text=True).stdout.strip()


@pytest.fixture(scope="session")
def agent():
    """A real clabgen-agent (the static binary) in a throwaway container
    whose eth1 is on a docker network of its own."""
    if not AGENT_BIN.exists():
        pytest.skip(f"no agent binary at {AGENT_BIN} (make build, or set CLABGEN_AGENT)")
    if not shutil.which("docker"):
        pytest.skip("no docker")
    name = f"clabgen-pytest-{uuid.uuid4().hex[:8]}"
    _docker("network", "create", name)
    try:
        _docker("create", "--name", name, "--cap-add", "NET_ADMIN",
                "-v", f"{AGENT_BIN}:/clabgen-agent:ro", IMAGE, "/clabgen-agent")
        _docker("network", "connect", name, name)
        _docker("start", name)
        # The API at the container's address on the default bridge (eth0): a
        # published port may be forwarded over eth1, which tests take down.
        ip = _docker("inspect", "-f", "{{.NetworkSettings.Networks.bridge.IPAddress}}", name)
        a = Agent(f"{ip}:8080", name="test")

        def up():
            try:
                return a.ports()
            except ClabgenError:
                return None
        wait_until(up, 20, 0.2, "the agent and its eth1")
        yield a
    finally:
        subprocess.run(["docker", "rm", "-f", name], capture_output=True)
        subprocess.run(["docker", "network", "rm", name], capture_output=True)


@pytest.fixture
def clean(agent):
    """The agent with nothing configured, before and after the test."""
    agent.apply_scenario("devices: []\n", replace=True)
    yield agent
    agent.apply_scenario("devices: []\n", replace=True)
