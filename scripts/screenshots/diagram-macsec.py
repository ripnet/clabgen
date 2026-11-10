"""The MACsec example's topology diagram, in a light and a dark version:
    python3 diagram-macsec.py OUT_DIR
writes macsec-topology.svg and macsec-topology-dark.svg."""

import sys
from pathlib import Path

THEMES = {
    "": dict(fill="#f6f8fa", text="#1f2328", muted="#59636e", agent="#2563eb", router="#0f766e", link="#8c959f", flow="#d97706"),
    "-dark": dict(fill="#22272e", text="#e6edf3", muted="#9198a1", agent="#4c8dff", router="#2dd4bf", link="#768390", flow="#f59e0b"),
}

MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
SANS = "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"


def text(x, y, s, cls, anchor="start"):
    return f'<text x="{x}" y="{y}" class="{cls}" text-anchor="{anchor}">{s}</text>'


def svg(t):
    W, H = 1240, 470
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="ce-a agent, SR-SIM sr1, SR-SIM sr2, ce-b agent, with two Epipes carrying MACsec">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .port {{ font: 15px {MONO}; fill: {t['muted']}; }}
  .note {{ font: 18px {SANS}; fill: {t['muted']}; }}
  .flowlbl {{ font: 18px {SANS}; fill: {t['flow']}; }}
  .wire {{ stroke: {t['link']}; stroke-width: 3; }}
  .flow {{ stroke: {t['flow']}; stroke-width: 2.5; fill: none; stroke-dasharray: 8 5; }}
</style>""",
           f'<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" '
           f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{t["flow"]}"/></marker></defs>']

    def node(x, y, w, h, kind, title, sub, lines=()):
        o = [f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="10" class="box {kind}"/>',
             text(x + w / 2, y + 40, title, "title", "middle"), text(x + w / 2, y + 64, sub, "kind", "middle")]
        for i, ln in enumerate(lines):
            o.append(text(x + w / 2, y + 100 + i * 26, ln, "line", "middle"))
        return o

    by, bh, w = 120, 230, 200
    ax, r1, r2, bx = 20, 330, 710, 1020
    y1, y2 = by + 70, by + 170  # the two circuits
    # Access links: eth1 (EPL) and eth2 (EVPL), each side.
    for a, b, pa, pb in [(ax + w, r1, "eth1", "c1/1"), (r2 + w, bx, "c1/1", "eth1")]:
        out += [f'<line x1="{a}" y1="{y1}" x2="{b}" y2="{y1}" class="wire"/>', text(a + 6, y1 - 10, pa, "port"), text(b - 6, y1 - 10, pb, "port", "end")]
    for a, b, pa, pb in [(ax + w, r1, "eth2", "c3/1"), (r2 + w, bx, "c3/1", "eth2")]:
        out += [f'<line x1="{a}" y1="{y2}" x2="{b}" y2="{y2}" class="wire"/>', text(a + 6, y2 - 10, pa, "port"), text(b - 6, y2 - 10, pb, "port", "end")]
    out.append(text((ax + w + r1) / 2, y1 + 28, "untagged", "note", "middle"))
    out.append(text((ax + w + r1) / 2, y2 + 28, "VLAN 100", "note", "middle"))
    # Core.
    cy = by + bh / 2
    out += [f'<line x1="{r1 + w}" y1="{cy}" x2="{r2}" y2="{cy}" class="wire"/>', text(r1 + w + 6, cy - 10, "c2/1", "port"),
            text(r2 - 6, cy - 10, "c2/1", "port", "end"), text((r1 + w + r2) / 2, cy + 28, "IS-IS, LDP", "note", "middle")]
    out += node(ax, by, w, bh, "agent", "ce-a", "clabgen agent", ["customer router", "MKA key server", "pings both circuits"])
    out += node(r1, by, w, bh, "router", "sr1", "Nokia SR-SIM", ["Epipes:", "epl, null SAP", "evpl, SAP :100"])
    out += node(r2, by, w, bh, "router", "sr2", "Nokia SR-SIM", ["Epipes:", "epl, null SAP", "evpl, SAP :100"])
    out += node(bx, by, w, bh, "agent", "ce-b", "clabgen agent", ["customer router", "answers"])
    # End to end: MACsec and MKA over each circuit.
    top, bottom = by - 36, by + bh + 36
    out += [f'<path d="M{ax + w / 2},{by - 8} V{top} H{bx + w / 2} V{by - 10}" class="flow" marker-end="url(#arrow)" marker-start="url(#arrow)"/>',
            text(W / 2, top - 12, "epl: MACsec at port level, MKA with CAK 1 · service-mtu 1546", "flowlbl", "middle"),
            f'<path d="M{ax + w / 2},{by + bh + 8} V{bottom} H{bx + w / 2} V{by + bh + 10}" class="flow" marker-end="url(#arrow)" marker-start="url(#arrow)"/>',
            text(W / 2, bottom + 26, "evpl: MACsec under VLAN 100 (in clear), MKA with CAK 2 · service-mtu 1546", "flowlbl", "middle")]
    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"macsec-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
