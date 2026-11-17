// Laying out a lab: containerlab's own positions (graph-posX/Y labels)
// when every node has them, else a force layout (linked nodes pull
// together, all push apart) from a fixed start, so a lab draws the same
// way each time. Then boxes are pushed apart until none overlap.

export type TNode = { name: string; kind?: string; image?: string; group?: string; mgmt_ipv4?: string; agent?: string; x?: number; y?: number };
export type TLink = { a: string; a_if: string; z: string; z_if: string };
export type Topology = { lab: string; nodes: TNode[]; links: TLink[]; note?: string; from?: string };

export type Box = { x: number; y: number; w: number; h: number };
export type Placed = Record<string, Box>;

export const NODE_H = 52;
export const nodeWidth = (n: TNode) => Math.max(116, Math.max(n.name.length * 8.4, (subtitle(n).length || 0) * 6.6) + 28);

// subtitle: the line under a node's name: its kind, or image for linux.
export function subtitle(n: TNode): string {
  if (n.agent) return "clabgen";
  if (n.kind === "linux" && n.image) return n.image.replace(/^.*\//, "").replace(/:latest$/, "");
  return n.kind ?? "";
}

// withEnds: the nodes plus any a link names that the lab doesn't list
// (host, mgmt-net, a bridge), so every link has two ends to draw.
export function withEnds(t: Topology): TNode[] {
  const names = new Set(t.nodes.map((n) => n.name));
  const extra: TNode[] = [];
  for (const l of t.links)
    for (const e of [l.a, l.z])
      if (!names.has(e)) {
        names.add(e);
        extra.push({ name: e, kind: e.split(":")[0] });
      }
  return [...t.nodes, ...extra];
}

// pairKey: one key per pair of nodes, whichever way round.
export const pairKey = (a: string, z: string) => (a < z ? `${a}\u0000${z}` : `${z}\u0000${a}`);

export function layout(nodes: TNode[], links: TLink[], saved: Record<string, { x: number; y: number }> = {}): Placed {
  const out: Placed = {};
  const size = (n: TNode) => ({ w: nodeWidth(n), h: NODE_H });
  // Positions from containerlab when all have them (and nothing is saved).
  const given = nodes.every((n) => n.x !== undefined && n.y !== undefined);
  if (given && !Object.keys(saved).length) {
    for (const n of nodes) out[n.name] = { x: n.x!, y: n.y!, ...size(n) };
    return separate(out, new Set());
  }

  const names = nodes.map((n) => n.name);
  const idx = new Map(names.map((n, i) => [n, i]));
  const edges = new Map<string, [number, number]>();
  for (const l of links) {
    const a = idx.get(l.a);
    const z = idx.get(l.z);
    if (a === undefined || z === undefined || a === z) continue;
    edges.set(pairKey(l.a, l.z), [a, z]);
  }
  const deg = names.map(() => 0);
  for (const [a, z] of edges.values()) {
    deg[a]++;
    deg[z]++;
  }
  // Start on a circle in breadth-first order from the best-connected node,
  // so neighbours start near each other.
  const adj = names.map(() => [] as number[]);
  for (const [a, z] of edges.values()) {
    adj[a].push(z);
    adj[z].push(a);
  }
  const order: number[] = [];
  const seen = new Set<number>();
  const byDeg = names.map((_, i) => i).sort((p, q) => deg[q] - deg[p] || names[p].localeCompare(names[q]));
  for (const s of byDeg) {
    if (seen.has(s)) continue;
    const queue = [s];
    seen.add(s);
    while (queue.length) {
      const v = queue.shift()!;
      order.push(v);
      for (const w of adj[v].sort((p, q) => names[p].localeCompare(names[q])))
        if (!seen.has(w)) {
          seen.add(w);
          queue.push(w);
        }
    }
  }
  const n = names.length;
  const k = 200;
  const r = Math.max(k, (k * n) / (2 * Math.PI));
  const px = names.map(() => 0);
  const py = names.map(() => 0);
  order.forEach((v, i) => {
    px[v] = r * Math.cos((2 * Math.PI * i) / n);
    py[v] = r * Math.sin((2 * Math.PI * i) / n);
  });
  const fixed = names.map((nm) => !!saved[nm]);
  names.forEach((nm, i) => {
    if (saved[nm]) [px[i], py[i]] = [saved[nm].x, saved[nm].y];
  });

  // Fruchterman–Reingold, with a little pull to the middle so separate
  // parts of a lab stay near each other.
  const iters = 400;
  let temp = r / 2;
  for (let it = 0; it < iters; it++) {
    const dx = names.map(() => 0);
    const dy = names.map(() => 0);
    for (let i = 0; i < n; i++)
      for (let j = i + 1; j < n; j++) {
        let ex = px[i] - px[j];
        let ey = py[i] - py[j];
        let d = Math.hypot(ex, ey);
        if (d < 0.01) [ex, ey, d] = [0.01 * (i - j), 0.01, 0.02];
        const f = (k * k) / d;
        dx[i] += (ex / d) * f;
        dy[i] += (ey / d) * f;
        dx[j] -= (ex / d) * f;
        dy[j] -= (ey / d) * f;
      }
    for (const [a, z] of edges.values()) {
      const ex = px[a] - px[z];
      const ey = py[a] - py[z];
      const d = Math.max(Math.hypot(ex, ey), 0.01);
      const f = (d * d) / k;
      dx[a] -= (ex / d) * f;
      dy[a] -= (ey / d) * f;
      dx[z] += (ex / d) * f;
      dy[z] += (ey / d) * f;
    }
    for (let i = 0; i < n; i++) {
      if (fixed[i]) continue;
      dx[i] -= px[i] * 0.05;
      dy[i] -= py[i] * 0.05;
      const d = Math.hypot(dx[i], dy[i]);
      if (d > 0) {
        px[i] += (dx[i] / d) * Math.min(d, temp);
        py[i] += (dy[i] / d) * Math.min(d, temp);
      }
    }
    temp = Math.max(1, temp * 0.985);
  }
  // Lie the lab along the screen: turn it so its longest spread runs
  // across (screens are wider than tall). Not when nodes were placed by hand.
  if (!fixed.some(Boolean) && n > 2) {
    const mx = px.reduce((a, b) => a + b, 0) / n;
    const my = py.reduce((a, b) => a + b, 0) / n;
    let cxx = 0;
    let cyy = 0;
    let cxy = 0;
    for (let i = 0; i < n; i++) {
      cxx += (px[i] - mx) ** 2;
      cyy += (py[i] - my) ** 2;
      cxy += (px[i] - mx) * (py[i] - my);
    }
    const a = -0.5 * Math.atan2(2 * cxy, cxx - cyy);
    const c = Math.cos(a);
    const sn = Math.sin(a);
    for (let i = 0; i < n; i++) {
      const x = px[i] - mx;
      const y = py[i] - my;
      [px[i], py[i]] = [x * c - y * sn, x * sn + y * c];
    }
  }
  nodes.forEach((nd, i) => (out[nd.name] = { x: px[i], y: py[i], ...size(nd) }));
  return separate(out, new Set(names.filter((_, i) => fixed[i])));
}

// separate pushes overlapping boxes apart (with room for link labels
// between them), leaving fixed ones where they are.
export function separate(p: Placed, fixed: Set<string>, gap = 48): Placed {
  const names = Object.keys(p).sort();
  for (let round = 0; round < 200; round++) {
    let moved = false;
    for (let i = 0; i < names.length; i++)
      for (let j = i + 1; j < names.length; j++) {
        const a = p[names[i]];
        const b = p[names[j]];
        const ox = (a.w + b.w) / 2 + gap - Math.abs(a.x - b.x);
        const oy = (a.h + b.h) / 2 + gap - Math.abs(a.y - b.y);
        if (ox <= 0 || oy <= 0) continue;
        moved = true;
        const fa = fixed.has(names[i]) ? 0 : fixed.has(names[j]) ? 1 : 0.5;
        const fb = 1 - fa;
        if (ox < oy) {
          const s = a.x < b.x || (a.x === b.x && i < j) ? -1 : 1;
          a.x += s * ox * fa;
          b.x -= s * ox * fb;
        } else {
          const s = a.y < b.y || (a.y === b.y && i < j) ? -1 : 1;
          a.y += s * oy * fa;
          b.y -= s * oy * fb;
        }
      }
    if (!moved) break;
  }
  return p;
}

// bounds: the box around every node, with a margin.
export function bounds(p: Placed, margin = 70): { x: number; y: number; w: number; h: number } {
  const bs = Object.values(p);
  if (!bs.length) return { x: 0, y: 0, w: 400, h: 200 };
  const x0 = Math.min(...bs.map((b) => b.x - b.w / 2)) - margin;
  const y0 = Math.min(...bs.map((b) => b.y - b.h / 2)) - margin;
  const x1 = Math.max(...bs.map((b) => b.x + b.w / 2)) + margin;
  const y1 = Math.max(...bs.map((b) => b.y + b.h / 2)) + margin;
  return { x: x0, y: y0, w: x1 - x0, h: y1 - y0 };
}

// clip: where the segment from the centre of box b toward (tx, ty)
// leaves b.
export function clip(b: Box, tx: number, ty: number, ox = 0, oy = 0): [number, number] {
  const cx = b.x + ox;
  const cy = b.y + oy;
  const dx = tx - cx;
  const dy = ty - cy;
  if (!dx && !dy) return [cx, cy];
  const sx = dx ? (b.w / 2 + 2) / Math.abs(dx) : Infinity;
  const sy = dy ? (b.h / 2 + 2) / Math.abs(dy) : Infinity;
  const s = Math.min(sx, sy, 1);
  return [cx + dx * s, cy + dy * s];
}

// Groups of links between the same two nodes, drawn as parallel lines up
// to BUNDLE of them, else as one line with a count.
export const BUNDLE = 4;
export type Pair = { a: string; z: string; links: TLink[] };
export function pairs(links: TLink[]): Pair[] {
  const m = new Map<string, Pair>();
  for (const l of links) {
    if (l.a === l.z) continue;
    const key = pairKey(l.a, l.z);
    let p = m.get(key);
    if (!p) m.set(key, (p = { a: l.a < l.z ? l.a : l.z, z: l.a < l.z ? l.z : l.a, links: [] }));
    // Each link as seen from p.a to p.z.
    p.links.push(l.a === p.a ? l : { a: l.z, a_if: l.z_if, z: l.a, z_if: l.a_if });
  }
  for (const p of m.values()) p.links.sort((x, y) => x.a_if.localeCompare(y.a_if, undefined, { numeric: true }));
  return [...m.values()];
}
