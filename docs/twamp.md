# TWAMP / STAMP reflector

A `twamp-reflector` answers the delay and loss measurements a router makes:
TWAMP Light (RFC 5357, appendix I) and STAMP (RFC 8762, with the TLVs of
RFC 8972). The router is the session-sender, and clabgen is the far end it
measures to, in place of a second router. Its faults (added delay, lost
replies, a wrong clock) show what the router does with them: its statistics,
its threshold alarms, the link delay it uses for traffic engineering.

A worked example: [TWAMP Light and STAMP with an SR-SIM](examples/twamp.md).

```yaml
devices:
  - {name: gen, port: eth1, addresses: [10.0.0.2/24, "2001:db8::2/64"], gateway4: 10.0.0.1}
protocols:
  - {name: reflector, type: twamp-reflector, device: gen}
```

It answers test packets to the device's addresses (and loopbacks) on its
ports, 862 by default. Routers usually make you set the port; it's
`destination-udp-port 862` in SR OS's OAM-PM sessions.

## What it answers

- **TWAMP Light**: the sender's 14-byte packet (sequence number, timestamp,
  error estimate) and any padding. The reply is the RFC 5357 reflector packet:
  the reflector's sequence number, its send timestamp (T3), its receive
  timestamp (T2), the sender's sequence number, timestamp and error estimate,
  and the TTL the packet arrived with. It's at least 41 bytes, else as long as
  the sender's packet.
- **STAMP**: the same, with the session ID (SSID) at byte 14, the reply as
  long as the sender's packet, and the TLVs after byte 44. In `mode: stamp`
  (the default) it processes the TLVs it knows and clears their U flag:

  | TLV | What the reply carries |
  |---|---|
  | Extra Padding (1) | returned as sent |
  | Timestamp Information (3) | clock source NTP if `synchronized`, else free-running; software timestamps |
  | Class of Service (4) | the DSCP and ECN received; the reply goes out with the DSCP the sender asked for |
  | Direct Measurement (5) | the reflector's received and sent counts for the session |
  | Follow-up Telemetry (7) | the previous reply's sequence number and its actual send time |

  Others come back as sent, U flag still set (unrecognized). A packet whose
  bytes after 44 don't parse as TLVs (zero padding from a TWAMP Light sender)
  is answered as TWAMP Light. SR OS 26.10 counts a TLV's 4-byte header in
  its length; the last TLV of a packet is accepted that way, and the session
  says so.
- `mode: twamp-light` reflects every packet without looking at TLVs.

Each session (sender address and port, local address and port, SSID) has its
own reflector sequence number, counting from 0 (`stateless: true` copies the
sender's). T2 is the kernel's receive time; T3 is taken just before the reply
goes out. Timestamps are NTP format, or PTP when the sender's error estimate
has the Z bit set. Packets arriving with TTL 1 (a router's link delay
measurement sends them) are answered; replies go out with `ttl` (255).

## Settings

| Setting | Default | |
|---|---|---|
| `mode` | `stamp` | `stamp` or `twamp-light` |
| `ports` | `[862]` | UDP ports to answer on |
| `senders` | any | only answer senders in these prefixes; others count as refused |
| `ttl` | 255 | TTL / hop limit of replies |
| `dscp` | the received one | DSCP of replies (a Class of Service TLV wins) |
| `stateless` | false | reply with the sender's sequence number |
| `synchronized` | false | the S bit of the error estimate: the clock is synchronized to an external source |
| `error_scale`, `error_multiplier` | 0, 1 | the rest of the error estimate |

## Faults

Settings, applied in place: sessions and counters stay, and the next replies
have them.

| Setting | What the sender sees |
|---|---|
| `delay_us` | Replies are held this long, and T2 is taken as if the packet arrived then: the round trip and forward delay grow by it. |
| `jitter_us` | A random 0 to `jitter_us` more on each reply; replies can overtake each other. |
| `dwell_us` | Replies are held this long with true timestamps: a sender that subtracts T3 − T2 (as TWAMP's round trip does) sees no change. |
| `drop_pct` | That percentage of replies lost at random. |
| `loss_direction` | Where lost replies are lost: `backward` (default), on the way back, so the reflector's sequence number moves on; `forward`, on the way in, as if never received. A sender that compares sequence numbers (SR OS does) puts the loss in that direction. |
| `clock_offset_ms` | Both timestamps shifted: one-way delays come out wrong, or negative, while the round trip stays right. |
| `bad_timestamps` | `reversed`: T3 before T2; `zero`: both zero. |

Actions:

- `mute` / `unmute`: stop answering, and start again.
- `burst-loss count=N`: each active session loses its next N replies in a
  row, for a router's high-loss-interval and unavailability counts.
- `clear`: forget the sessions and reset the counters.

## Status

`clabgenctl proto show reflector`, or the reflector's page in the UI:

- totals: received, replied, dropped (faults), refused (`senders`), errors
  (bad checksum, too short), replies held (`delay_us`, `dwell_us`);
- each session: sender and local address and port, STAMP with its SSID or
  TWAMP Light, timestamp format, received, replied, dropped, the sender's
  sequence numbers missing (forward loss before the reflector), reordered or
  repeated, the TTL and DSCP it arrived with, the last dwell (T3 − T2), the
  TLVs it carries, and when it was last heard. A session not heard for 10 s
  is idle (greyed).

Events: `session-new`, `session-idle`, `muted`, `unmuted`, `burst-loss`.
Health, for group views: `no sessions`, `N sessions` or `muted`.

## Router configuration

SR OS (26.10), the sessions of the [example](examples/twamp.md):

```
/configure oam-pm session "twamp-v4" ip router-instance "Base" source 10.0.0.1 destination 10.0.0.2
/configure oam-pm session "twamp-v4" ip destination-udp-port 862
/configure oam-pm session "twamp-v4" ip twamp-light test-id 1 admin-state enable

/configure oam-pm session "stamp-v6" ip source 2001:db8::1 destination 2001:db8::2 destination-udp-port 862
/configure oam-pm session "stamp-v6" ip twamp-light session-sender-type stamp pad-tlv-size 20
```

and an interface's link delay (STAMP packets with TTL 1, DSCP 48):

```
/configure test-oam link-measurement measurement-template "link-delay" twamp-light session-sender-type stamp
/configure router interface "to-gen" if-attribute delay dynamic link-measurement link-measurement-template "link-delay"
/configure router interface "to-gen" if-attribute delay dynamic twamp-light ipv4 source 10.0.0.1 destination 10.0.0.2 admin-state enable
```

OAM-PM records loss only with `record-stats delay-and-loss` on the session.
