# MACsec

A `macsec` instance is MACsec (IEEE 802.1AE) on a device or on its whole port.
Every frame the devices behind it send is protected, and they take only frames
that validate. Flows, BGP, ARP and everything else on the device run over it
unchanged. Keys come from MKA, the key agreement routers use, with a pre-shared
key, or are static.

Use it to test a circuit that should carry MACsec end to end (an Epipe, a VPLS,
an EVPN service), or to stand in for a router that runs MACsec.
A worked example: [MACsec through Epipes](examples/macsec.md).

```yaml
devices:
  - {name: evpl, port: eth2, vlans: [{vid: 100}], addresses: [10.0.2.1/24]}
protocols:
  - name: evpl-macsec
    type: macsec
    device: evpl
    config:
      mka:
        cak: "ffeeddccbbaa99887766554433221100"
        ckn: "657670"
```

## Where the SecTAG goes

MACsec puts its SecTAG after the MAC addresses and encrypts what follows, so a
frame's VLAN tags are either in front of it (in clear, where a provider's
equipment can still see them) or inside it.

| Config | On the wire | Like |
|---|---|---|
| a device without VLANs | `DA SA │ SecTAG │ {EtherType, data} ICV` | MACsec on a router's port |
| a device with VLAN 100 | `DA SA 100 │ SecTAG │ {…} ICV` | MACsec on a subinterface (VLAN in clear) |
| VLANs 100 and 200 | `DA SA 100 200 │ SecTAG │ {…} ICV` | the same, QinQ |
| VLANs 100 and 200, `clear_tags: 1` | `DA SA 100 │ SecTAG │ {200, …} ICV` | the outer tag in clear, for a provider that adds it |
| VLAN 100, `clear_tags: 0` | `DA SA │ SecTAG │ {100, …} ICV` | port-level MACsec with subinterfaces behind it |
| `scope: port` | `DA SA │ SecTAG │ {each device's tags, …} ICV` | one router port: every device on it shares one secure channel |

`{…}` is encrypted, unless `encrypt` is off. MKPDUs go with the tags that are in
clear, as a router sends them on that subinterface.

- `scope: device` (the default) secures the device. Other devices on the port
  are untouched, so VLAN 100 can run MACsec beside a plain VLAN 200, or beside
  VLAN 300 with another key: each one a separate customer circuit.
- `scope: port` secures every device on the port, now or added later, through
  one SecY with the SCI of the device holding it, and one MKA session. It's what
  a router doing MACsec on a port looks like to its peer. A port is secured as a
  whole or per device, not both.

## Keys

### MKA

`mka` runs the MACsec Key Agreement (IEEE 802.1X) with a pre-shared CAK, as
routers are usually configured:

- `cak` and `ckn`: the connectivity association key (32 or 64 hex digits) and
  its name (up to 64). Both ends need the same.
- `key_server_priority`: the lowest is key server, the one that makes and hands
  out keys (ties go to the lowest SCI); 255 never is. Default 16.
- `hello_time`: between MKPDUs, 2 s by default. A peer that's silent for three
  of them is gone.
- `eapol_dst`: where MKPDUs go. `nearest-non-tpmr` (01:80:c2:00:00:03) is the
  default and what most equipment expects; `nearest-bridge`, `nearest-customer`,
  or a MAC.

The key server sends with a new key once every peer receives with it, and the
others once the key server does. The old key is kept for 3 s more, so a rekey
loses nothing. The key server makes a new key when peers come or go, when its
packet numbers pass 0xC0000000, and on `rekey`.

### Static keys

`static` is one SAK both ends use, as `ip macsec` sets up on Linux: `key`, `an`
(0–3) and `pn`, the first packet number (default 1). XPN suites also need `salt`
(24 hex digits), `ssci` and the far end's `peer_ssci`. Receive channels are made
for any SCI that sends with the key.

## Settings

- `cipher`: `gcm-aes-128` (the default), `gcm-aes-256`, `gcm-aes-xpn-128`,
  `gcm-aes-xpn-256`. The XPN suites (64-bit packet numbers) need static keys for
  now. With MKA, the key server's suite must match.
- `encrypt`: off protects integrity only, the data stays readable.
  `confidentiality_offset` (0, 30 or 50) leaves that many bytes after the
  SecTAG in clear. With MKA, the key server's settings apply to both ends.
- `include_sci`: the SCI in every SecTAG, the default. Off saves 8 bytes; with
  `end_station` the SCI is the source MAC and port 1.
- `port_id`: the SCI's port number (its MAC is the device's).
- `validate`: `strict` (the default) drops frames that fail and frames without
  MACsec; `check` delivers integrity-only frames that fail, and frames without
  MACsec. `replay_protect` with a `replay_window` of packets a frame may be late.
- `policy`: without a key, `must-secure` (the default) sends nothing, and the
  device's flows report `macsec: no transmit SA`; `should-secure` sends in clear.

## Frame sizes

MACsec adds 32 bytes to every frame: a 16-byte SecTAG and a 16-byte ICV, or 24
without the SCI. A flow's `frame_size` and `payload_size` are the frame before
MACsec, and the instance's status shows the overhead.

A circuit carrying MACsec needs that room: a 1500-byte IP packet is a 1514-byte
frame, 1546 with MACsec. On a provider's equipment the circuit's MTU is set
without the tags its access port strips: for SR OS, an Epipe's `service-mtu`,
with the access port 4 bytes bigger per stripped VLAN tag.

The MKA exchange has to cross too. MKPDUs go to 01:80:c2:00:00:03, which some
equipment consumes: SR OS drops untagged EAPOL on an access port until
`ethernet dot1x tunneling true`, and tunnels it on dot1q and QinQ ports already.
Without it the encrypted frames would pass, but the session never comes up.

## Status

The status shows the frame's layout, the SecY's SCI and transmit SA, its receive
channels with what each dropped and why (late, not valid, no SA…), and for MKA
the peers, the key server, and the latest and old keys with whether each end
receives and sends with them. `clabgenctl protocols` shows one line:

```
epl-macsec   macsec  epl   running  untagged gcm-aes-128 +32B mka=secured key-server sak=1 an=0 peers=1
```

## Actions

- `rekey`: the key server hands out a new key now (under traffic, nothing should
  be lost).
- `pause-mka` stops sending and answering MKPDUs: the peer times the session out
  and removes the keys, and `must-secure` traffic stops. `resume-mka` starts
  again with a new member identifier, as a restarted router would.
- `clear` resets the counters.

A change of key, cipher or scope is a config edit, and restarts the instance.

## Checked against

The data plane against Linux kernel MACsec (`ip macsec`): every suite, integrity
only, without the SCI, VLANs in clear and inside, QinQ with the outer tag in
clear, and a port with VLANs behind it; frames from Linux validate and clabgen's
are identical to the kernel's for the same key and packet number. MKA against
wpa_supplicant 2.10: either end as key server, under a VLAN, a whole port, and
rekeys under traffic.

## Not yet

XPN with MKA; CAKs from 802.1X authentication (EAP, RADIUS); MKA's later
parameter sets (announcements, XPN); actions that inject bad frames (a wrong
ICV, a replayed packet number, an unknown AN). The Nokia SR-SIM isn't a MACsec
peer: its simulated ports don't take MACsec (tried as an SR-1 and an IXR-e2), so
the example uses it to carry MACsec, not to terminate it.
