"""The convergence example's topology diagram, in a light and a dark version:
    python3 diagram-convergence.py OUT_DIR
writes convergence-topology.svg and convergence-topology-dark.svg."""

import sys
from pathlib import Path

THEMES = {
    "": dict(fill="#f6f8fa", stroke="#d0d7de", text="#1f2328", muted="#59636e", agent="#2563eb",
             router="#0f766e", link="#8c959f", flow="#d97706", bad="#cf222e"),
    "-dark": dict(fill="#22272e", stroke="#444c56", text="#e6edf3", muted="#9198a1", agent="#4c8dff",
                  router="#2dd4bf", link="#768390", flow="#f59e0b", bad="#f85149"),
}

MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
SANS = "system-ui, -apple-system, Segoe UI, Roboto, sans-serif"


def text(x, y, s, cls, anchor="start"):
    return f'<text x="{x}" y="{y}" class="{cls}" text-anchor="{anchor}">{s}</text>'


def box(x, y, w, h, kind):
    return f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="10" class="box {kind}"/>'


def svg(t):
    # Drawn for the docs' content width (about 700 px): few words per box,
    # large type.
    W, H = 1210, 420
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="sender agent, SR-SIM sr1, router agent with a primary and a backup link">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .port {{ font: 16px {MONO}; fill: {t['muted']}; }}
  .linkname {{ font: 600 18px {SANS}; fill: {t['text']}; }}
  .linknote {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .failtxt {{ font: 600 18px {SANS}; fill: {t['bad']}; }}
  .wire {{ stroke: {t['link']}; stroke-width: 3; }}
  .cross {{ stroke: {t['bad']}; stroke-width: 5; stroke-linecap: round; }}
  .flow {{ stroke: {t['flow']}; stroke-width: 2.5; fill: none; stroke-dasharray: 8 5; }}
  .flowtxt {{ font: 18px {SANS}; fill: {t['muted']}; }}
</style>""",
           f'<defs><marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="6" markerHeight="6" '
           f'orient="auto-start-reverse"><path d="M0,0 L10,5 L0,10 z" fill="{t["flow"]}"/></marker></defs>']

    by, bh = 30, 280

    def node(x, w, kind, title, sub, lines):
        o = [box(x, by, w, bh, kind), text(x + w / 2, by + 44, title, "title", "middle"),
             text(x + w / 2, by + 70, sub, "kind", "middle")]
        for i, ln in enumerate(lines):
            o.append(text(x + w / 2, by + 124 + i * 30, ln, "line", "middle"))
        return o

    sx, sw = 20, 230
    rx, rw = 390, 220
    tx, tw = 950, 240
    out += node(sx, sw, "agent", "sender", "clabgen agent", ["ping-bfd", "ping-plain", "stream-plain"])
    out += node(rx, rw, "router", "sr1", "Nokia SR-SIM", ["IS-IS level 2", "BFD on VLAN 10"])
    out += node(tx, tw, "agent", "router", "clabgen agent", ["router-bfd", "router-plain", "two IS-IS routers"])

    # sender to sr1
    wy = by + bh / 2
    a, b = sx + sw, rx
    out += [f'<line x1="{a}" y1="{wy}" x2="{b}" y2="{wy}" class="wire"/>',
            text(a + 8, wy - 12, "eth1", "port"), text(b - 8, wy - 12, "1/1/c1", "port", "end")]

    # sr1 to router: primary and backup
    a, b = rx + rw, tx
    m = (a + b) / 2
    py, ky = by + 70, by + 215
    out += [f'<line x1="{a}" y1="{py}" x2="{b}" y2="{py}" class="wire"/>',
            text(a + 8, py - 12, "1/1/c2", "port"), text(b - 8, py - 12, "eth1", "port", "end"),
            text(m, py + 30, "primary, metric 10", "linkname", "middle"),
            f'<line x1="{m - 14}" y1="{py - 14}" x2="{m + 14}" y2="{py + 14}" class="cross"/>',
            f'<line x1="{m - 14}" y1="{py + 14}" x2="{m + 14}" y2="{py - 14}" class="cross"/>',
            text(m, py + 56, "fails: the SR isn't told", "failtxt", "middle"),
            f'<line x1="{a}" y1="{ky}" x2="{b}" y2="{ky}" class="wire"/>',
            text(a + 8, ky - 12, "1/1/c3", "port"), text(b - 8, ky - 12, "eth2", "port", "end"),
            text(m, ky + 30, "backup, metric 20", "linkname", "middle")]

    # how traffic travels, before and after
    fy = by + bh + 44
    out += [f'<path d="M{sx + sw / 2},{by + bh + 8} V{fy} H{tx + tw / 2} V{by + bh + 10}" class="flow" marker-end="url(#arrow)"/>',
            text((sx + tx + tw) / 2, fy + 32, "flows to each router's loopback, primary link first, then backup once the SR reroutes", "flowtxt", "middle")]

    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"convergence-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
