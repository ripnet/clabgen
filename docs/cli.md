# clabgenctl

`clabgenctl` drives an agent from a shell or a script: everything the web
UI does, with tables for lists and YAML or JSON (`-o json`) for details.

## Getting it

Inside every agent container it's already there, and talks to the agent
on localhost: `docker exec clab-<lab>-<node> clabgenctl …`.

On your own machine (to drive agents from a laptop, and for the
[Wireshark plugin](#wireshark)), download it from the
[releases](https://github.com/ripnet/clabgen/releases): one file, no
dependencies. `releases/latest/download/` always points at the newest
release.

**macOS** (`arm64` for Apple silicon, `amd64` for Intel):

```sh
f=clabgenctl-darwin-arm64
curl -fLO https://github.com/ripnet/clabgen/releases/latest/download/$f
curl -fLO https://github.com/ripnet/clabgen/releases/latest/download/SHA256SUMS
grep " $f\$" SHA256SUMS | shasum -a 256 -c        # prints "OK"
chmod +x $f && sudo mv $f /usr/local/bin/clabgenctl
clabgenctl --version
```

Download it with `curl` as above. A browser marks the file as downloaded
from the internet, and macOS then refuses to run it ("cannot be opened
because the developer cannot be verified"); clear that with
`xattr -d com.apple.quarantine /usr/local/bin/clabgenctl`.

**Linux** (`amd64` or `arm64`):

```sh
f=clabgenctl-linux-amd64
curl -fLO https://github.com/ripnet/clabgen/releases/latest/download/$f
curl -fLO https://github.com/ripnet/clabgen/releases/latest/download/SHA256SUMS
grep " $f\$" SHA256SUMS | sha256sum -c             # prints "OK"
chmod +x $f && sudo mv $f /usr/local/bin/clabgenctl   # or ~/.local/bin, without sudo
clabgenctl --version
```

**Windows** (PowerShell; `amd64`, or `arm64` on ARM machines):

```powershell
$f = "clabgenctl-windows-amd64.exe"
Invoke-WebRequest "https://github.com/ripnet/clabgen/releases/latest/download/$f" -OutFile clabgenctl.exe
(Get-FileHash clabgenctl.exe).Hash      # compare with the $f line of SHA256SUMS on the release page
New-Item -ItemType Directory -Force "$env:USERPROFILE\bin" | Out-Null
Move-Item clabgenctl.exe "$env:USERPROFILE\bin\" -Force
[Environment]::SetEnvironmentVariable("Path", "$env:Path;$env:USERPROFILE\bin", "User")   # then open a new terminal
clabgenctl --version
```

**With Go** (1.23 or newer): `go install github.com/ripnet/clabgen/cmd/clabgenctl@latest`
puts it in `$(go env GOPATH)/bin`.

Then point it at an agent's management address or containerlab name with
`-s` (or `CLABGEN_SERVER`):

```sh
clabgenctl -s clab-b2b-gen1 protocols
```

With `--lab` (or `CLABGEN_LAB`) set to a lab's name or its topology file,
`-s` takes node names: the address comes from docker (containerlab's
labels), else from the container name, the topology's `prefix` included.
`nodes` lists the lab's agents:

```sh
export CLABGEN_LAB=examples/b2b/b2b.clab.yml
clabgenctl nodes                  # NODE  ADDRESS  VERSION  UP SINCE
clabgenctl -s gen2 received
```

## Commands

```
info                      agent hostname, version and start time
nodes                     the lab's agents (with --lab)
ports / port              data ports; port down|up <port>
devices / device          emulated devices: add, set, show, del, resolve, clear-neighbors
protocols / proto         protocol instances: add, show, set, disable, enable, del, run <action>
types [TYPE]              protocol types, or one type's config and action schemas
groups / group            device groups: add, set, show, disable, enable, del, run <action>
ping                      ping from a device (a temporary icmp flow)
start / stop              start or stop traffic flows
received                  flows arriving at the agent's devices (one-way statistics)
preview                   the packet a config would send (packet type), decoded and as hex
capture                   frames the ports receive and send: live summaries, or pcapng
events                    stream the agent's events
scenario get / apply      export or apply the whole config
scenario reset            forget the saved config: back to the topology's scenario
```

`clabgenctl <command> --help` lists each command's options. Configs and
action arguments take `key=value` pairs or a whole document with
`--config` / `--args` (JSON, YAML, or `@file`). A value is read as JSON
when it is valid JSON (`remote_meps=[2]` is a list, `rate=50` a number)
and as a string otherwise; dots reach into nested settings:

```sh
clabgenctl proto add ping --type icmp --device d1 destination=10.9.0.2 rate=50 auto_start=true \
  checks.max_loss_pct=1 checks.max_outage_ms=200
```

## Examples

With the [b2b example lab](../examples/b2b):

```sh
clabgenctl -s clab-b2b-gen1 events &                          # live events
clabgenctl -s clab-b2b-gen2 port down eth1                    # gen1 raises LOC
clabgenctl -s clab-b2b-gen1 proto run mep-v100 loopback target_mep_id=102 count=5
clabgenctl -s clab-b2b-gen1 proto run mep-v100 fault ccm_enabled=false
clabgenctl -s clab-b2b-gen1 proto disable mep-v100            # stop it, keep its config
clabgenctl -s clab-b2b-gen1 proto enable mep-v100
clabgenctl -s clab-b2b-gen1 types cfm-mep                     # config + action schemas
clabgenctl -s clab-b2b-gen1 device set v100 --ip 10.0.100.1/24 --gw 10.0.100.254
clabgenctl -s clab-b2b-gen1 device resolve untagged 192.0.2.2 # ARP / ND
clabgenctl -s clab-b2b-gen1 device show untagged              # neighbors + counters
clabgenctl -s clab-b2b-gen1 ping untagged 192.0.2.2 -c 5 -i 100ms
clabgenctl -s clab-b2b-gen1 stop ping-gen2                    # flows: start / stop
clabgenctl -s clab-b2b-gen2 received                          # flows arriving at gen2
clabgenctl -s clab-b2b-gen1 scenario get > saved.yml
clabgenctl -s clab-b2b-gen1 scenario reset                    # back to gen1.yml (asks first; -y doesn't)
```

Packet flows (see [configuration](configuration.md#packet-builder)):

```sh
clabgenctl preview --device untagged --config '{layers: [{ipv4: {dst: 192.0.2.2}}, {tcp: {dst_port: 22, flags: SA}}]}'
clabgenctl preview --device untagged --config @packet.yml --index 5   # packet 5: modifiers applied
clabgenctl proto add replay --type packet --device untagged --pcap capture.pcapng rate=100 auto_start=true
clabgenctl proto run replay preview index=2                            # a running flow's packet
```

Packet capture: every frame the agent's ports receive and send, as on the
wire (device VLAN tags included). `-w` writes pcapng (`-` for stdout);
filters are `--device`, `--direction rx|tx`, `--ethertype` and
`--snaplen`; `-c` and `--seconds` stop it:

```sh
clabgenctl capture eth1                            # In/Out lines, like tcpdump
clabgenctl capture --device v100 --ethertype 0x8902 -c 20
clabgenctl capture eth1 eth2 -w trace.pcapng --seconds 30
clabgenctl capture eth1 -w - | wireshark -k -i -   # live in Wireshark
```

A capture that can't keep up drops frames (never the agent's traffic)
and says how many.

## Wireshark

`clabgenctl` is also a Wireshark plugin (extcap): installed once, every
clabgen agent's ports appear in Wireshark's list of capture interfaces
("clabgen b2b/gen1: eth1", and "all ports" per agent). The interface's
options (the gear next to it) filter by device, direction and ethertype.

```sh
clabgenctl wireshark install --ui http://labhost:8090   # the agents a clabgen-ui knows
clabgenctl wireshark install --lab b2b                  # or a containerlab lab on this machine
clabgenctl wireshark install --agent 172.20.20.2        # or agents by address (repeatable)
clabgenctl wireshark status                             # what Wireshark will list
clabgenctl wireshark uninstall
```

`install` links `clabgenctl` into Wireshark's personal extcap folder
(`~/.local/lib/wireshark/extcap`; `%APPDATA%\Wireshark\extcap` on
Windows; Help → About Wireshark → Folders shows yours, and `--dir` picks
another) and saves where the agents are in
`~/.config/clabgen/wireshark.json`. Restart Wireshark, or Capture →
Refresh Interfaces, to see new agents. Capture filters (BPF) don't apply;
use the options, or a display filter.

Device groups (see [configuration](configuration.md#device-groups)):

```sh
clabgenctl groups                                   # each group protocol's states
clabgenctl group show ce                            # the members
clabgenctl group run ce flap --members 1-20 seconds=10
clabgenctl group run ce reset --random 5
clabgenctl group disable ce --members 2,5           # until group enable
```
