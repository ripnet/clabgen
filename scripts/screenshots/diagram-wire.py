"""The wire example's topology diagram, in a light and a dark version:
    python3 diagram-wire.py OUT_DIR
writes wire-topology.svg and wire-topology-dark.svg."""

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
    W, H = 1460, 560
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="ce-a agent, SR-SIM sr1, SR-SIM sr2, ce-b agent with three Epipes; sr1 mirrors the core link to the tap agent">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .port {{ font: 15px {MONO}; fill: {t['muted']}; }}
  .note {{ font: 17px {SANS}; fill: {t['muted']}; }}
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

    by, bh, w = 40, 270, 210
    ax, r1, r2, bx = 20, 380, 870, 1230
    rows = [(by + 105, "eth1", "c1/1", "epl", "null SAP"), (by + 170, "eth2", "c3/1", "evpl", "SAP :3162"),
            (by + 235, "eth3", "c4/1", "enni", "SAP :3162.163")]
    out += node(ax, by, w, bh, "agent", "ce-a", "clabgen agent")
    out += node(r1, by, w, bh, "router", "sr1", "Nokia SR-SIM")
    out += node(r2, by, w, bh, "router", "sr2", "Nokia SR-SIM")
    out += node(bx, by, w, bh, "agent", "ce-b", "clabgen agent")
    # Access links, one per Epipe, each side; the SAP inside each router, the
    # customer's tags inside each CE.
    for y, ce, sr, svc, sap in rows:
        for a, b, pa, pb in [(ax + w, r1, ce, sr), (r2 + w, bx, sr, ce)]:
            out += [f'<line x1="{a}" y1="{y}" x2="{b}" y2="{y}" class="wire"/>', text(a + 8, y - 9, pa, "port"), text(b - 8, y - 9, pb, "port", "end")]
        for x in (r1, r2):
            out.append(text(x + w / 2, y + 6, f"{svc}: {sap}", "line", "middle"))
        for x in (ax, bx):
            out.append(text(x + w / 2, y + 6, f"{svc}: 3162 · 163", "line", "middle"))
    # Core.
    cy = by + 105
    out += [f'<line x1="{r1 + w}" y1="{cy}" x2="{r2}" y2="{cy}" class="wire"/>', text(r1 + w + 8, cy - 9, "c2/1", "port"),
            text(r2 - 8, cy - 9, "c2/1", "port", "end"), text((r1 + w + r2) / 2, cy + 26, "IS-IS, LDP", "note", "middle"),
            text((r1 + w + r2) / 2, cy + 48, "a pseudowire per Epipe", "note", "middle")]
    # The tap, under the core, and sr1's mirror to it.
    tx, ty, tw, th = (r1 + w + r2) / 2 - 100, 420, 200, 120
    out += node(tx, ty, tw, th, "agent", "tap", "clabgen agent", ["monitor"])
    mx = r1 + w - 40
    out += [f'<line x1="{mx}" y1="{by + bh}" x2="{mx}" y2="{ty + 70}" class="wire"/>',
            f'<line x1="{mx}" y1="{ty + 70}" x2="{tx}" y2="{ty + 70}" class="wire"/>',
            text(mx + 8, by + bh + 22, "c5/1", "port"), text(tx - 8, ty + 62, "eth1", "port", "end")]
    out += [f'<path d="M{(r1 + w + r2) / 2},{cy + 64} V{ty - 12}" class="flow" marker-end="url(#arrow)"/>',
            text((r1 + w + r2) / 2 + 12, ty - 36, "sr1 mirrors c2/1, both ways", "flowlbl")]
    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"wire-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
