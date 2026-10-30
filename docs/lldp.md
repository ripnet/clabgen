# LLDP

LLDP runs on an untagged device (LLDP frames carry no VLAN tag):

```yaml
devices:
  - {name: lldp1, port: eth1}
protocols:
  - name: lldp-eth1
    type: lldp
    device: lldp1
    config: {tx_interval: 30, system_name: gen1, capabilities: [bridge, router],
             management_addresses: [192.0.2.50], port_vlan_id: 1, max_frame_size: 9212}
```

- It lists the neighbors it hears (`clabgenctl proto show lldp-eth1`).
- `shutdown`, `resume` and `pause seconds=120` make neighbors drop it or
  age it out, and `send` sends an LLDPDU now.
- `custom_tlvs` add arbitrary TLVs.
