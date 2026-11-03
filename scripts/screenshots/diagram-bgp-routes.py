"""The BGP routes example's topology diagram, in a light and a dark version:
    python3 diagram-bgp-routes.py OUT_DIR
writes bgp-routes-topology.svg and bgp-routes-topology-dark.svg."""

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
    W, H = 1210, 380
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="sender agent, SR-SIM sr1, routes agent advertising 100 BGP routes">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .port {{ font: 16px {MONO}; fill: {t['muted']}; }}
  .note {{ font: 18px {SANS}; fill: {t['muted']}; }}
  .wire {{ stroke: {t['link']}; stroke-width: 3; }}
  .flow {{ stroke: {t['flow']}; stroke-width: 2.5; fill: none; stroke-dasharray: 8 5; }}
</style>""",
           f'<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" '
           f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{t["flow"]}"/></marker></defs>']
    by, bh = 30, 220

    def node(x, w, kind, title, sub, lines):
        o = [f'<rect x="{x}" y="{by}" width="{w}" height="{bh}" rx="10" class="box {kind}"/>',
             text(x + w / 2, by + 44, title, "title", "middle"), text(x + w / 2, by + 70, sub, "kind", "middle")]
        for i, ln in enumerate(lines):
            o.append(text(x + w / 2, by + 124 + i * 30, ln, "line", "middle"))
        return o

    sx, sw, rx, rw, tx, tw = 20, 260, 475, 260, 930, 260
    out += node(sx, sw, "agent", "sender", "clabgen agent", ["one flow to all", "100 prefixes"])
    out += node(rx, rw, "router", "sr1", "Nokia SR-SIM", ["AS 65001", "routes the 100 prefixes"])
    out += node(tx, tw, "agent", "routes", "clabgen agent", ["AS 65002", "advertises 100 routes,", "and answers for them"])
    wy = by + bh / 2
    for a, b, pa, pb, note in [(sx + sw, rx, "eth1", "1/1/c1/1", ""), (rx + rw, tx, "1/1/c2/1", "eth1", "eBGP")]:
        out += [f'<line x1="{a}" y1="{wy}" x2="{b}" y2="{wy}" class="wire"/>',
                text(a + 8, wy - 12, pa, "port"), text(b - 8, wy - 12, pb, "port", "end")]
        if note:
            out.append(text((a + b) / 2, wy + 30, note, "note", "middle"))
    fy = by + bh + 44
    out += [f'<path d="M{sx + sw / 2},{by + bh + 8} V{fy} H{tx + tw / 2} V{by + bh + 10}" class="flow" marker-end="url(#arrow)"/>',
            text((sx + tx + tw) / 2, fy + 32, "a packet to each prefix in turn: which ones get no reply?", "note", "middle")]
    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"bgp-routes-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
