"""The TWAMP example's topology diagram, in a light and a dark version:
    python3 diagram-twamp.py OUT_DIR
writes twamp-topology.svg and twamp-topology-dark.svg."""

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
    W, H = 1000, 400
    out = [f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
           'role="img" aria-label="SR-SIM sr1 measuring delay and loss to the clabgen agent gen, which reflects">',
           f"""<style>
  .box {{ fill: {t['fill']}; stroke-width: 2.5; }}
  .agent {{ stroke: {t['agent']}; }}
  .router {{ stroke: {t['router']}; }}
  .title {{ font: 600 26px {SANS}; fill: {t['text']}; }}
  .kind {{ font: 17px {SANS}; fill: {t['muted']}; }}
  .line {{ font: 19px {SANS}; fill: {t['text']}; }}
  .port {{ font: 15px {MONO}; fill: {t['muted']}; }}
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

    by, bh, w = 130, 230, 240
    rx, gx = 30, W - 30 - w
    cy = by + bh - 40
    out += [f'<line x1="{rx + w}" y1="{cy}" x2="{gx}" y2="{cy}" class="wire"/>',
            text(rx + w + 6, cy - 10, "1/1/c1/1", "port"), text(gx - 6, cy - 10, "eth1", "port", "end"),
            text(rx + w + 6, cy + 26, "10.0.0.1, 2001:db8::1", "port"), text(gx - 6, cy + 26, "10.0.0.2, 2001:db8::2", "port", "end")]
    out += node(rx, by, w, bh, "router", "sr1", "Nokia SR-SIM", ["session-sender:", "OAM-PM, link delay"])
    out += node(gx, by, w, bh, "agent", "gen", "clabgen agent", ["twamp-reflector", "port 862"])
    labels = ["twamp-v4: TWAMP Light, IPv4", "stamp-v6: STAMP, IPv6", "link delay: STAMP, TTL 1"]
    for i, lbl in enumerate(labels):
        y = by + 30 + i * 46
        out += [f'<path d="M{rx + w + 8},{y} H{gx - 10}" class="flow" marker-end="url(#arrow)" marker-start="url(#arrow)"/>',
                text(W / 2, y - 10, lbl, "flowlbl", "middle")]
    out.append(text(W / 2, by - 40, "test packets to port 862, replies with the reflector's timestamps", "kind", "middle"))
    out.append("</svg>\n")
    return "\n".join(out)


def main(dst):
    for suffix, theme in THEMES.items():
        path = Path(dst) / f"twamp-topology{suffix}.svg"
        path.write_text(svg(theme))
        print(path)


if __name__ == "__main__":
    main(*sys.argv[1:])
