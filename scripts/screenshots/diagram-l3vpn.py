"""The L3VPN example's topology diagram, in a light and a dark version:
    python3 diagram-l3vpn.py OUT_DIR
writes l3vpn-topology.svg and l3vpn-topology-dark.svg."""

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
    # Drawn for the docs' content width (about 700 px): large type.
    W, H = 1240, 400
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="pe agent, SR-SIM sr1, SR-SIM sr2 with VPRN 100, ce agent">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .port {{ font: 15px {MONO}; fill: {t['muted']}; }}
  .note {{ font: 18px {SANS}; fill: {t['muted']}; }}
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

    by, bh, w = 110, 170, 200
    px, ax, bx, cx = 20, 330, 710, 1020
    wy = by + bh / 2
    links = [(px + w, ax, "eth1", "c1/1", "IS-IS, LDP"), (ax + w, bx, "c2/1", "c1/1", "IS-IS, LDP"), (bx + w, cx, "c2/1", "eth1", "eBGP")]
    for a, b, pa, pb, note in links:
        out += [f'<line x1="{a}" y1="{wy}" x2="{b}" y2="{wy}" class="wire"/>',
                text(a + 6, wy - 10, pa, "port"), text(b - 6, wy - 10, pb, "port", "end"),
                text((a + b) / 2, wy + 28, note, "note", "middle")]
    out += node(px, by, w, bh, "agent", "pe", "clabgen agent", ["the far PE,", "flows in the VPN"])
    out += node(ax, by, w, bh, "router", "sr1", "Nokia SR-SIM", ["P: switches", "the LDP label"])
    out += node(bx, by, w, bh, "router", "sr2", "Nokia SR-SIM", ["PE: VPRN 100", "takes the VPN label"])
    out += node(cx, by, w, bh, "agent", "ce", "clabgen agent", ["a customer site,", "plain IP"])
    fy = by - 34
    out += [f'<path d="M{px + w / 2},{by - 8} V{fy} H{bx + w / 2} V{by - 10}" class="flow" marker-end="url(#arrow)"/>',
            text((px + bx + w) / 2, fy - 12, "iBGP VPNv4 and VPNv6 between loopbacks; packets take labels from it", "note", "middle")]
    out.append(text(W / 2, by + bh + 60, "Route target 65000:100 · pe 192.0.2.1 · sr2 192.0.2.12", "note", "middle"))
    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"l3vpn-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
