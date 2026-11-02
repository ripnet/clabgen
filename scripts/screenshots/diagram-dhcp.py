"""The DHCP example's topology diagram, in a light and a dark version:
    python3 diagram-dhcp.py OUT_DIR
writes dhcp-topology.svg and dhcp-topology-dark.svg."""

import sys
from pathlib import Path

THEMES = {
    "": dict(fill="#f6f8fa", stroke="#d0d7de", text="#1f2328", muted="#59636e", agent="#2563eb",
             router="#0f766e", link="#8c959f", flow="#d97706"),
    "-dark": dict(fill="#22272e", stroke="#444c56", text="#e6edf3", muted="#9198a1", agent="#4c8dff",
                  router="#2dd4bf", link="#768390", flow="#f59e0b"),
}

MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
SANS = "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"


def text(x, y, s, cls, anchor="start"):
    return f'<text x="{x}" y="{y}" class="{cls}" text-anchor="{anchor}">{s}</text>'


def box(x, y, w, h, kind):
    return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="10" class="box {kind}"/>'


def svg(t):
    # Drawn for the docs' content width (about 700 px): few words per box,
    # large type, the subnets on the links.
    W, H = 1210, 390
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="client agent, SR-SIM sr1, server agent; DHCP relayed by sr1">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .addr {{ font: 16px {MONO}; fill: {t['text']}; }}
  .port {{ font: 16px {MONO}; fill: {t['muted']}; }}
  .flowtxt {{ font: 18px {SANS}; fill: {t['muted']}; }}
  .wire {{ stroke: {t['link']}; stroke-width: 3; }}
  .flow {{ stroke: {t['flow']}; stroke-width: 2.5; fill: none; stroke-dasharray: 8 5; }}
</style>""",
           f'<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" '
           f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{t["flow"]}"/></marker></defs>']

    by, bh = 40, 190

    def node(x, w, kind, title, sub, lines):
        o = [box(x, by, w, bh, kind), text(x + w / 2, by + 40, title, "title", "middle"),
             text(x + w / 2, by + 66, sub, "kind", "middle")]
        for i, ln in enumerate(lines):
            o.append(text(x + w / 2, by + 108 + i * 28, ln, "line", "middle"))
        return o

    cx, cw = 20, 240
    rx, rw = 450, 300
    sx, sw = 940, 250
    out += node(cx, cw, "agent", "client", "clabgen agent", ["client1", "subscribers ×10", "DHCPv4 + DHCPv6"])
    out += node(rx, rw, "router", "sr1", "Nokia SR-SIM", ["DHCP relay v4 + v6", "router advertisements", "routes to each /56"])
    out += node(sx, sw, "agent", "server", "clabgen agent", ["10.0.200.10", "2001:db8:200::10", "DHCP servers v4 + v6"])

    # links: the ports at each end, the subnet below
    wy = by + bh / 2
    for a, b, pa, pb, nets in [(cx + cw, rx, "eth1", "1/1/c1/1", ["10.0.100.0/24", "2001:db8:100::/64"]),
                               (rx + rw, sx, "1/1/c2/1", "eth1", ["10.0.200.0/24", "2001:db8:200::/64"])]:
        m = (a + b) / 2
        out += [f'<line x1="{a}" y1="{wy}" x2="{b}" y2="{wy}" class="wire"/>',
                text(a + 8, wy - 12, pa, "port"), text(b - 8, wy - 12, pb, "port", "end"),
                text(m, wy + 30, nets[0], "addr", "middle"), text(m, wy + 54, nets[1], "addr", "middle")]

    # how a request travels: from the subscriber link, relayed to the server
    fy = 300
    c_mid, s_mid, mid = cx + cw / 2, sx + sw / 2, rx + rw / 2
    r_in, r_out = mid - 50, mid + 50
    out += [f'<path d="M{c_mid},{by + bh + 8} V{fy} H{r_in} V{by + bh + 10}" class="flow" marker-end="url(#arrow)"/>',
            f'<path d="M{r_out},{by + bh + 8} V{fy} H{s_mid} V{by + bh + 10}" class="flow" marker-end="url(#arrow)"/>',
            text((c_mid + r_in) / 2, fy + 32, "Discover, Solicit", "flowtxt", "middle"),
            text((c_mid + r_in) / 2, fy + 56, "(broadcast, multicast)", "flowtxt", "middle"),
            text((r_out + s_mid) / 2, fy + 32, "relayed by unicast; giaddr or", "flowtxt", "middle"),
            text((r_out + s_mid) / 2, fy + 56, "link-address picks the pool", "flowtxt", "middle")]

    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"dhcp-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
