// A lab drawn: its nodes (clabgen agents link to their pages) and links,
// from containerlab's topology-data.json (via the UI server). Drag nodes
// to arrange them (kept in this browser per lab), wheel to zoom, drag the
// background to pan.

import { useEffect, useMemo, useRef, useState, type PointerEvent as RPE, type WheelEvent as RWE } from "react";
import { client, labTopology, usePoll } from "../api";
import { agentHref } from "../router";
import { ErrorBox } from "../ui";
import { BUNDLE, NODE_H, bounds, clip, layout, pairKey, pairs, subtitle, withEnds, type Box, type Pair, type Placed, type TLink } from "../components/topology/layout";

type Saved = Record<string, { x: number; y: number }>;
const storeKey = (lab: string) => `clabgen.topology.${lab}`;
function loadSaved(lab: string): Saved {
  try {
    return JSON.parse(localStorage.getItem(storeKey(lab)) ?? "{}") as Saved;
  } catch {
    return {};
  }
}
function save(lab: string, p: Placed) {
  try {
    localStorage.setItem(storeKey(lab), JSON.stringify(Object.fromEntries(Object.entries(p).map(([k, b]) => [k, { x: Math.round(b.x), y: Math.round(b.y) }]))));
  } catch {
    /* storage unavailable: the arrangement lasts for this page */
  }
}
function forget(lab: string) {
  try {
    localStorage.removeItem(storeKey(lab));
  } catch {
    /* nothing kept */
  }
}

// Link state, from the clabgen agents' ports (routers report nothing).
// A link is as bad as its worst clabgen end: admin down (shut here), down,
// unknown (the agent didn't answer, or has no such port), up; links with
// no clabgen end have none.
type LinkState = "up" | "down" | "admin-down" | "unknown" | "none";
type PortState = { admin: boolean; oper: boolean };
type AgentPorts = { ok: boolean; ports: Map<string, PortState> };
const rank: Record<LinkState, number> = { "admin-down": 4, down: 3, unknown: 2, up: 1, none: 0 };
const worst = (s: LinkState[]) => s.reduce<LinkState>((a, b) => (rank[b] > rank[a] ? b : a), "none");
const stateText: Record<LinkState, string> = { up: "up", down: "down", "admin-down": "shut (admin down)", unknown: "state unknown", none: "" };

// endState: one end of a link: a clabgen agent's port, else nothing known.
function endState(node: string, port: string, agentOf: Map<string, string>, ports: Record<string, AgentPorts>): LinkState {
  const id = agentOf.get(node);
  if (!id) return "none";
  const ap = ports[id];
  const p = ap?.ok ? ap.ports.get(port) : undefined;
  if (!p) return "unknown";
  if (!p.admin) return "admin-down";
  return p.oper ? "up" : "down";
}

// usePortStates polls the ports of the lab's agents.
function usePortStates(ids: string[], everyMs = 3000): Record<string, AgentPorts> {
  const [out, setOut] = useState<Record<string, AgentPorts>>({});
  const key = ids.join(",");
  useEffect(() => {
    if (!ids.length) return;
    let live = true;
    const run = () =>
      Promise.allSettled(ids.map((id) => client(id).listPorts({}))).then((rs) => {
        if (!live) return;
        const next: Record<string, AgentPorts> = {};
        rs.forEach((r, i) => {
          next[ids[i]] =
            r.status === "fulfilled"
              ? { ok: true, ports: new Map(r.value.ports.map((p) => [p.name, { admin: p.adminUp, oper: p.operUp }])) }
              : { ok: false, ports: new Map() };
        });
        setOut(next);
      });
    run();
    const t = setInterval(run, everyMs);
    return () => {
      live = false;
      clearInterval(t);
    };
    // ids is keyed by its contents.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, everyMs]);
  return out;
}

const SPACING = 14; // between parallel links
// The smallest scale a lab opens at, so names stay readable; Fit shows all.
const MIN_SCALE = 0.75;
// labelWidth: about how long an interface name is drawn (10px monospace).
const labelWidth = (s: string) => s.length * 6.1 + 8;

export function LabTopology({ lab }: { lab: string }) {
  const t = usePoll(() => labTopology(lab), [lab]);
  const [layoutGen, setLayoutGen] = useState(0);
  const nodes = useMemo(() => (t.data ? withEnds(t.data) : []), [t.data]);
  const [pos, setPos] = useState<Placed>({});
  useEffect(() => {
    if (!t.data) return;
    setPos(layout(nodes, t.data.links, loadSaved(lab)));
  }, [t.data, nodes, lab, layoutGen]);
  const ps = useMemo(() => (t.data ? pairs(t.data.links) : []), [t.data]);
  const agentOf = useMemo(() => new Map(nodes.filter((n) => n.agent).map((n) => [n.name, n.agent!])), [nodes]);
  const ports = usePortStates([...new Set(agentOf.values())].sort());
  const ends = (l: TLink): [LinkState, LinkState] => [endState(l.a, l.a_if, agentOf, ports), endState(l.z, l.z_if, agentOf, ports)];
  const linkState = (l: TLink) => worst(ends(l));
  const fit = useMemo(() => bounds(pos), [pos]);
  const [view, setView] = useState<{ x: number; y: number; w: number; h: number }>();
  const [px, setPx] = useState({ w: 0, h: 0 });
  const [hover, setHover] = useState<string>();
  const [sel, setSel] = useState<string>();
  const svg = useRef<SVGSVGElement>(null);
  const drag = useRef<{ node?: string; sx: number; sy: number; moved: boolean; ox: number; oy: number }>();
  // The drawing's size on screen, for the opening view.
  useEffect(() => {
    const el = svg.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setPx({ w: el.clientWidth, h: el.clientHeight }));
    ro.observe(el);
    return () => ro.disconnect();
  }, [t.data]);
  // Opening view: the whole lab, unless that makes it too small to read;
  // then a readable part of it, around its middle.
  const opening = useMemo(() => {
    if (!px.w || !px.h) return fit;
    const scale = Math.min(px.w / fit.w, px.h / fit.h);
    if (scale >= MIN_SCALE) return fit;
    const w = px.w / MIN_SCALE;
    const h = px.h / MIN_SCALE;
    return { x: fit.x + (fit.w - w) / 2, y: fit.y + (fit.h - h) / 2, w, h };
  }, [fit, px]);
  const vb = view ?? opening;
  const whole = view ? view.w >= fit.w - 1 && view.h >= fit.h - 1 : opening === fit;

  // Fit again when the lab (not an arrangement) changes.
  useEffect(() => setView(undefined), [lab, layoutGen]);

  const toSvg = (e: { clientX: number; clientY: number }) => {
    const m = svg.current?.getScreenCTM();
    if (!m) return { x: 0, y: 0 };
    const p = new DOMPoint(e.clientX, e.clientY).matrixTransform(m.inverse());
    return { x: p.x, y: p.y };
  };
  const down = (e: RPE, node?: string) => {
    e.stopPropagation();
    (e.currentTarget as Element).setPointerCapture(e.pointerId);
    const p = toSvg(e);
    drag.current = { node, sx: p.x, sy: p.y, moved: false, ox: node ? pos[node].x : vb.x, oy: node ? pos[node].y : vb.y };
  };
  const move = (e: RPE) => {
    const d = drag.current;
    if (!d) return;
    const p = toSvg(e);
    const dx = p.x - d.sx;
    const dy = p.y - d.sy;
    if (Math.hypot(dx, dy) > 4) d.moved = true;
    if (!d.moved) return;
    if (d.node) setPos((cur) => ({ ...cur, [d.node!]: { ...cur[d.node!], x: d.ox + dx, y: d.oy + dy } }));
    else {
      // Panning moves the view; the pointer's SVG point moves with it, so
      // keep the start in screen terms.
      setView({ ...vb, x: vb.x - dx, y: vb.y - dy });
    }
  };
  const up = () => {
    const d = drag.current;
    drag.current = undefined;
    if (!d) return;
    if (d.node && d.moved) save(lab, pos);
    else if (d.node && !d.moved) {
      const n = nodes.find((x) => x.name === d.node);
      if (n?.agent) window.location.hash = agentHref(n.agent);
    }
  };
  const wheel = (e: RWE) => {
    const p = toSvg(e);
    const f = Math.exp(e.deltaY * 0.0015);
    const w = Math.min(Math.max(vb.w * f, 200), fit.w * 6);
    const s = w / vb.w;
    setView({ x: p.x - (p.x - vb.x) * s, y: p.y - (p.y - vb.y) * s, w, h: vb.h * s });
  };
  // The page shouldn't scroll while the wheel zooms the drawing.
  useEffect(() => {
    const el = svg.current;
    if (!el) return;
    const stop = (e: WheelEvent) => e.preventDefault();
    el.addEventListener("wheel", stop, { passive: false });
    return () => el.removeEventListener("wheel", stop);
  }, [t.data]);

  const d = t.data;
  const linked = (name: string) => ps.filter((p) => p.a === name || p.z === name).map((p) => pairKey(p.a, p.z));
  const hot = new Set(hover ? linked(hover) : []);
  // The selected pair, read from its clabgen end when only the far one is.
  const selPair = useMemo(() => {
    const p = ps.find((x) => pairKey(x.a, x.z) === sel);
    if (!p || agentOf.has(p.a) || !agentOf.has(p.z)) return p;
    return { a: p.z, z: p.a, links: p.links.map((l) => ({ a: l.z, a_if: l.z_if, z: l.a, z_if: l.a_if })).sort((x, y) => x.a_if.localeCompare(y.a_if, undefined, { numeric: true })) };
  }, [ps, sel, agentOf]);

  return (
    <div className="card topo">
      <div className="card-head">
        <h2>
          Lab <span className="mono">{lab}</span>
        </h2>
        <div className="btn-row">
          <button className="btn" onClick={() => setView(fit)} disabled={whole} title="Show the whole lab">
            Fit
          </button>
          <button
            className="btn"
            onClick={() => {
              forget(lab);
              setLayoutGen((g) => g + 1);
            }}
            title="Forget where nodes were dragged to and lay the lab out again"
          >
            Rearrange
          </button>
          <button className="btn" onClick={t.refresh}>
            Reload
          </button>
        </div>
      </div>
      <ErrorBox error={t.error} />
      {d && (
        <>
          <p className="muted">
            {d.nodes.length} nodes · {d.links.length} links. Drag nodes to arrange them, the wheel zooms, drag the background to pan; a clabgen node opens its agent; hover a node for its links&apos; interfaces.
            {d.from && <span className="small-note topo-from"> From {d.from}.</span>}
          </p>
          {d.note && <div className="warn-box">{d.note}</div>}
          <svg
            ref={svg}
            className="topo-svg"
            role="img"
            aria-label={`Lab ${lab}: ${d.nodes.length} nodes, ${d.links.length} links`}
            viewBox={`${vb.x} ${vb.y} ${vb.w} ${vb.h}`}
            onPointerDown={(e) => down(e)}
            onPointerMove={move}
            onPointerUp={up}
            onWheel={wheel}
          >
            <g className="topo-links">
              {ps.map((p) => (
                <PairLines
                  key={pairKey(p.a, p.z)}
                  p={p}
                  a={pos[p.a]}
                  z={pos[p.z]}
                  state={hover ? (hot.has(pairKey(p.a, p.z)) ? "hot" : "dim") : sel === pairKey(p.a, p.z) ? "hot" : ""}
                  ends={ends}
                  onPick={() => setSel(pairKey(p.a, p.z))}
                />
              ))}
            </g>
            <g className="topo-nodes">
              {nodes.map((n) => {
                const b = pos[n.name];
                if (!b) return null;
                const cls = `topo-node ${n.agent ? "agent" : n.kind === "linux" ? "linux" : d.nodes.some((x) => x.name === n.name) ? "router" : "external"} ${hover && hover !== n.name && !linked(hover).some((k) => k.split("\u0000").includes(n.name)) ? "dim" : ""}`;
                return (
                  <g
                    key={n.name}
                    className={cls}
                    transform={`translate(${b.x - b.w / 2} ${b.y - b.h / 2})`}
                    onPointerDown={(e) => down(e, n.name)}
                    onPointerEnter={() => setHover(n.name)}
                    onPointerLeave={() => setHover(undefined)}
                    role={n.agent ? "link" : undefined}
                    aria-label={n.agent ? `${n.name}: open its agent` : n.name}
                  >
                    <title>{[n.name, n.kind, n.image, n.mgmt_ipv4 && `mgmt ${n.mgmt_ipv4}`, n.agent && "click to open its agent"].filter(Boolean).join("\n")}</title>
                    <rect width={b.w} height={NODE_H} rx={9} />
                    <text x={b.w / 2} y={22} className="topo-name">
                      {n.name}
                    </text>
                    <text x={b.w / 2} y={40} className="topo-kind">
                      {subtitle(n)}
                    </text>
                  </g>
                );
              })}
            </g>
          </svg>
          {selPair && (
            <div className="topo-sel card-lite">
              <div className="btn-row" style={{ justifyContent: "space-between" }}>
                <strong>
                  {selPair.a} ↔ {selPair.z}: {selPair.links.length} link{selPair.links.length === 1 ? "" : "s"}
                </strong>
                <button className="link" onClick={() => setSel(undefined)}>
                  close
                </button>
              </div>
              <div className="mono small-note">
                {selPair.links.map((l) => (
                  <div key={l.a_if + l.z_if}>
                    {l.a_if} ↔ {l.z_if} <StateTag s={linkState(l)} />
                  </div>
                ))}
              </div>
            </div>
          )}
          {agentOf.size > 0 && <Legend />}
          <LinkTable links={d.links} state={linkState} />
        </>
      )}
    </div>
  );
}

// PairLines draws the links between two nodes: parallel lines with each
// end's interface along them, or past BUNDLE of them one thick line with a
// count (click it for the list).
function PairLines({ p, a, z, state, ends, onPick }: { p: Pair; a?: Box; z?: Box; state: string; ends: (l: TLink) => [LinkState, LinkState]; onPick: () => void }) {
  if (!a || !z) return null;
  const len = Math.hypot(z.x - a.x, z.y - a.y) || 1;
  const ux = (z.x - a.x) / len;
  const uy = (z.y - a.y) / len;
  const nx = -uy;
  const ny = ux;
  if (p.links.length > BUNDLE) {
    const [x1, y1] = clip(a, z.x, z.y);
    const [x2, y2] = clip(z, a.x, a.y);
    const mx = (x1 + x2) / 2;
    const my = (y1 + y2) / 2;
    const states = p.links.map((l) => worst(ends(l)));
    const down = states.filter((s) => s === "down" || s === "admin-down").length;
    const label = `×${p.links.length}${down ? ` · ${down} down` : ""}`;
    return (
      <g className={`topo-pair bundle ${state} st-${worst(states)}`} onClick={onPick} onPointerDown={(e) => e.stopPropagation()}>
        <title>{`${p.a} ↔ ${p.z}: ${p.links.length} links (click for the list)\n` + p.links.map((l, i) => `${l.a_if} ↔ ${l.z_if}${states[i] !== "none" ? ` · ${stateText[states[i]]}` : ""}`).join("\n")}</title>
        <line x1={x1} y1={y1} x2={x2} y2={y2} />
        <rect x={mx - 6 - label.length * 3.6} y={my - 10} width={12 + label.length * 7.2} height={20} rx={10} className="topo-badge" />
        <text x={mx} y={my + 4} className="topo-badge-text">
          {label}
        </text>
      </g>
    );
  }
  return (
    <g className={`topo-pair ${state}`}>
      {p.links.map((l, i) => {
        const off = (i - (p.links.length - 1) / 2) * SPACING;
        const ox = nx * off;
        const oy = ny * off;
        const [x1, y1] = clip(a, z.x + ox, z.y + oy, ox, oy);
        const [x2, y2] = clip(z, a.x + ox, a.y + oy, ox, oy);
        // Names that would run into each other show only while the link is
        // highlighted (its tooltip and the table always have them).
        const fits = labelWidth(l.a_if) + labelWidth(l.z_if) + 16 <= Math.hypot(x2 - x1, y2 - y1);
        const [sa, sz] = ends(l);
        const st = worst([sa, sz]);
        const bad = (s: LinkState) => s === "down" || s === "admin-down";
        return (
          <g key={l.a_if + "|" + l.z_if} className={`st-${st}`}>
            <title>{`${p.a} ${l.a_if} ↔ ${p.z} ${l.z_if}${st !== "none" ? ` · ${stateText[st]}` : ""}`}</title>
            <line x1={x1} y1={y1} x2={x2} y2={y2} />
            {bad(sa) && <circle className="topo-end" cx={x1 + ux * 3} cy={y1 + uy * 3} r={4} />}
            {bad(sz) && <circle className="topo-end" cx={x2 - ux * 3} cy={y2 - uy * 3} r={4} />}
            {(fits || state === "hot") && (
              <>
                <EndLabel x={x1} y={y1} ux={ux} uy={uy} text={l.a_if} />
                <EndLabel x={x2} y={y2} ux={-ux} uy={-uy} text={l.z_if} />
              </>
            )}
          </g>
        );
      })}
    </g>
  );
}

// EndLabel: an interface name along its line, starting just off the box
// (u points away from it), turned to read left to right.
function EndLabel({ x, y, ux, uy, text }: { x: number; y: number; ux: number; uy: number; text: string }) {
  let deg = (Math.atan2(uy, ux) * 180) / Math.PI;
  let anchor: "start" | "end" = "start";
  if (deg > 90 || deg < -90) {
    deg += 180;
    anchor = "end";
  }
  const px = x + ux * 8;
  const py = y + uy * 8;
  return (
    <text className="topo-if" x={px} y={py} dy={-3} textAnchor={anchor} transform={`rotate(${deg} ${px} ${py})`}>
      {text}
    </text>
  );
}

function StateTag({ s }: { s: LinkState }) {
  if (s === "none") return null;
  return <span className={`topo-state st-${s}`}>{stateText[s]}</span>;
}

function Legend() {
  return (
    <div className="topo-legend small-note">
      {(["up", "down", "admin-down", "unknown", "none"] as LinkState[]).map((s) => (
        <span key={s} className={`st-${s}`}>
          <svg width="22" height="8" aria-hidden="true">
            <line x1="1" y1="4" x2="21" y2="4" />
          </svg>
          {s === "none" ? "no clabgen end (not known)" : stateText[s]}
        </span>
      ))}
      <span className="muted">from the clabgen agents&apos; ports, every 3 s</span>
    </div>
  );
}

function LinkTable({ links, state }: { links: TLink[]; state: (l: TLink) => LinkState }) {
  if (!links.length) return null;
  const rows = [...links].sort((x, y) => x.a.localeCompare(y.a) || x.a_if.localeCompare(y.a_if, undefined, { numeric: true }));
  return (
    <details className="topo-table" open={links.length <= 40}>
      <summary>Links ({links.length})</summary>
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>Node</th>
              <th>Interface</th>
              <th>Node</th>
              <th>Interface</th>
              <th>State</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((l) => (
              <tr key={`${l.a}/${l.a_if}/${l.z}/${l.z_if}`}>
                <td className="mono">{l.a}</td>
                <td className="mono">{l.a_if}</td>
                <td className="mono">{l.z}</td>
                <td className="mono">{l.z_if}</td>
                <td>
                  <StateTag s={state(l)} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </details>
  );
}
