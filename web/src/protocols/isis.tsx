import { Fragment, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { registerView, type Health } from "./registry";

type AdjSID = { label: number; neighbor?: string; ipv6?: boolean; backup?: boolean };
type SRv6SID = { sid: string; behavior: string; algorithm?: number; neighbor?: string };
type LinkAttrs = { flex_algo: boolean; te_metric?: number; delay_us?: number; admin_groups?: number };
type Nbr = { id: string; metric: number; mt?: number; adj_sids?: AdjSID[]; link_attributes?: LinkAttrs; end_x_sids?: SRv6SID[] };
type SID = { index?: number; label?: number; flags?: string; algorithm?: number };
type Pfx = { prefix: string; metric: number; down?: boolean; mt?: number; sid?: SID; flex_sids?: SID[] };
type FAD = { algorithm: number; metric_type: string; priority: number; exclude_any?: number; include_any?: number; include_all?: number; participate_only?: boolean };
type Range = { base: number; size: number };
type SRCap = {
  router_id: string;
  srgb?: Range[];
  srlb?: Range[];
  mpls_ipv4: boolean;
  mpls_ipv6: boolean;
  algorithms?: number[];
  flex_algo_definitions?: FAD[];
  srv6?: boolean;
  msd?: Record<string, number>;
};
type Locator = { locator: string; metric: number; algorithm?: number; mt?: number; end_sids?: SRv6SID[] };
type Binding = { prefix: string; range: number; index: number; algorithm?: number; mt?: number };
const ranges = (rs?: Range[]) => (rs ?? []).map((g) => `${g.base}–${g.base + g.size - 1}`).join(", ");
// Admin group bit mask → "3,5".
const groups = (m?: number) =>
  Array.from({ length: 32 }, (_, i) => i)
    .filter((i) => ((m ?? 0) >>> i) & 1)
    .join(",");
const fadText = (f: FAD) =>
  `algo ${f.algorithm}: ${f.metric_type}, prio ${f.priority}` +
  (f.exclude_any ? `, exclude ${groups(f.exclude_any)}` : "") +
  (f.include_any ? `, include-any ${groups(f.include_any)}` : "") +
  (f.include_all ? `, include-all ${groups(f.include_all)}` : "");
const mtTag = (mt?: number) => (mt ? <span className="tag">{({ 2: "MT ipv6", 3: "MT mcast4", 4: "MT mcast6" } as Record<number, string>)[mt] ?? `MT ${mt}`}</span> : null);
// LSDB rows in the status are summaries; the "lsp" action has the rest.
type LSPRow = {
  level: number;
  lsp_id: string;
  seq: number;
  lifetime: number;
  checksum: string;
  own?: boolean;
  hostname?: string;
  overload?: boolean;
  neighbors: number;
  prefixes: number;
};
type LSPDetail = {
  sr?: SRCap;
  topologies?: string[];
  areas?: string[];
  router_id?: string;
  addresses?: string[];
  neighbors?: Nbr[];
  prefixes?: Pfx[];
  srv6_locators?: Locator[];
  sid_bindings?: Binding[];
};

function LSPDetailRow({ agentId, name, row, nameOf }: { agentId?: string; name?: string; row: LSPRow; nameOf: (id: string) => string }) {
  const d = usePoll(
    async () => {
      if (!agentId || !name) return undefined;
      const r = await client(agentId).runProtocolAction({ name, action: "lsp", args: { id: row.lsp_id, level: row.level } });
      return r.result as unknown as LSPDetail;
    },
    [agentId, name, row.lsp_id, row.level],
    3000,
  );
  const l = d.data;
  if (d.error) return <div className="error-text">{d.error}</div>;
  if (!l) return <div className="muted">Loading…</div>;
  return (
    <div className="lsp-grid">
      <div>
        <div className="field-label">neighbors</div>
        {(l.neighbors ?? []).map((n) => (
          <div key={`${n.id}/${n.mt ?? 0}`} className="mono">
            {nameOf(n.id)} <span className="muted">metric {n.metric}</span>
            {mtTag(n.mt)}
            {n.adj_sids?.map((a, i) => (
              <span key={i} className="tag" title={a.neighbor ? `LAN-Adj-SID for ${nameOf(a.neighbor + ".00")}` : "Adj-SID"}>
                {a.ipv6 ? "v6 " : ""}adj {a.label}
                {a.neighbor ? ` → ${nameOf(a.neighbor + ".00").replace(/\.00$/, "")}` : ""}
              </span>
            ))}
            {n.link_attributes && (
              <span className="tag" title="Application-specific link attributes (flex-algo)">
                {[
                  n.link_attributes.te_metric !== undefined && `te ${n.link_attributes.te_metric}`,
                  n.link_attributes.delay_us !== undefined && `${n.link_attributes.delay_us} µs`,
                  n.link_attributes.admin_groups && `groups ${groups(n.link_attributes.admin_groups)}`,
                ]
                  .filter(Boolean)
                  .join(" · ") || "ASLA"}
              </span>
            )}
            {n.end_x_sids?.map((x) => (
              <span key={x.sid + (x.neighbor ?? "")} className="tag" title={`SRv6 ${x.behavior}${x.neighbor ? ` toward ${nameOf(x.neighbor + ".00")}` : ""}`}>
                {x.sid}
              </span>
            ))}
          </div>
        ))}
        {!l.neighbors?.length && <div className="muted">none</div>}
        {l.router_id && <div className="mono muted">router ID {l.router_id}</div>}
        {l.topologies && <div className="mono muted">topologies: {l.topologies.join(", ")}</div>}
        {l.sr && (
          <div className="mono muted">
            SR:{l.sr.srgb?.length ? ` SRGB ${ranges(l.sr.srgb)} · MPLS ${[l.sr.mpls_ipv4 && "v4", l.sr.mpls_ipv6 && "v6"].filter(Boolean).join("/")} ·` : ""}
            {l.sr.srlb?.length ? ` SRLB ${ranges(l.sr.srlb)} ·` : ""}
            {l.sr.srv6 ? " SRv6 ·" : ""} algorithms {(l.sr.algorithms ?? [0]).join(", ")} · router ID {l.sr.router_id}
            {l.sr.flex_algo_definitions?.map((f) => <div key={f.algorithm}>FAD {fadText(f)}</div>)}
          </div>
        )}
        {l.srv6_locators?.map((x) => (
          <div key={`${x.locator}/${x.algorithm ?? 0}`} className="mono muted">
            locator {x.locator}
            {x.algorithm ? ` (algo ${x.algorithm})` : ""}
            {x.end_sids?.map((e) => ` · ${e.sid} ${e.behavior}`).join("")}
          </div>
        ))}
        {l.sid_bindings?.map((b) => (
          <div key={b.prefix} className="mono muted">
            mapping {b.prefix} ×{b.range} → SID {b.index}
            {b.algorithm ? ` (algo ${b.algorithm})` : ""}
          </div>
        ))}
        {l.addresses && <div className="mono muted">{l.addresses.join(", ")}</div>}
      </div>
      <div>
        <div className="field-label">prefixes ({l.prefixes?.length ?? 0})</div>
        <div className="pfx-list mono">
          {(l.prefixes ?? []).slice(0, 500).map((p) => (
            <div key={`${p.prefix}/${p.mt ?? 0}`}>
              {p.prefix} <span className="muted">{p.metric}</span>
              {p.down && <span className="tag">down</span>}
              {mtTag(p.mt)}
              {p.sid && (
                <span className="tag" title={`Prefix-SID flags ${p.sid.flags || "none"}`}>
                  SID {p.sid.index ?? `label ${p.sid.label}`}
                  {p.sid.flags ? ` ${p.sid.flags}` : ""}
                </span>
              )}
              {p.flex_sids?.map((f) => (
                <span key={f.algorithm} className="tag" title={`Prefix-SID for flex-algo ${f.algorithm}`}>
                  algo {f.algorithm} SID {f.index ?? `label ${f.label}`}
                </span>
              ))}
            </div>
          ))}
          {(l.prefixes?.length ?? 0) > 500 && <div className="muted">… {l.prefixes!.length - 500} more</div>}
        </div>
      </div>
    </div>
  );
}
type Adj = ISISStatus["adjacency"];
type LANRow = { interface?: string; level: number; we_are_dis: boolean; dis: string; lan_id: string; priority: number; elections: number };
type IfRow = {
  name: string;
  circuit: string;
  metric: number;
  priority?: number;
  passive?: boolean;
  attached: boolean;
  error?: string;
  mac?: string;
  addresses?: string[];
  hello_auth: string;
  adjacencies_up: number;
  flapping?: boolean;
};
type ISISStatus = {
  system_id: string;
  hostname: string;
  level: string;
  circuit: string;
  areas: string[];
  metric: number;
  overload: boolean;
  overload_on_startup_until?: string;
  graceful_restart?: { helper: boolean; restarting?: { since: string; remaining_s: number; acked: number; synced: number; pending: number } };
  bfd?: boolean;
  inter_level?: { attached: boolean; to_l2: number; leaked_to_l1: number; attached_routers: string[] };
  flapping?: boolean;
  advertised_prefixes: number;
  hello_auth?: string;
  lsp_auth?: string;
  topologies?: string[];
  segment_routing?: {
    srgb_base: number;
    srgb_size: number;
    mpls?: boolean;
    srlb?: Range;
    node_sids: { prefix: string; index: number; label: number; algorithm?: number; flags?: string }[];
    adj_sids: { neighbor: string; label: number; ipv6?: boolean; mt?: number; lan?: boolean }[];
    mappings?: Binding[];
    flex_algos?: FAD[];
    srv6_sids?: (SRv6SID & { locator?: string; lan?: boolean })[];
  };
  loopbacks?: string[];
  interfaces?: IfRow[];
  adjacency: {
    interface?: string;
    state: string;
    neighbor?: string;
    hostname?: string;
    mac?: string;
    levels?: number[];
    hold_remaining_s?: number;
    since?: string;
    areas?: string[];
    addresses?: string[];
    three_way: boolean;
    transitions: number;
    priority?: number;
    dis?: boolean;
    topologies?: string[];
    bfd?: { family: string; address: string; state: string; remote_state?: string; detect_ms?: number }[];
    waiting_for_bfd?: boolean;
    restarting?: boolean;
    suppressed?: boolean;
  };
  adjacencies?: ISISStatus["adjacency"][];
  lan?: LANRow[];
  lsdb: LSPRow[];
  lsdb_total?: number;
  topology?: {
    kind: string;
    nodes: number;
    links: number;
    prefixes: number;
    attached: string[];
    down_nodes?: string[];
    down_links?: string[];
    overloaded?: string[];
  };
  counters: Record<string, number>;
};

const as = (s: JsonObject) => s as unknown as ISISStatus;

// AdjExtras shows an adjacency's BFD sessions and restart state.
function AdjExtras({ a }: { a: Adj }) {
  return (
    <>
      {(a.bfd ?? []).map((b) => (
        <span key={b.family} className={b.state === "up" ? "tag" : "tag tag-warn"} title={`BFD with ${b.address}${b.detect_ms ? `, detection ${b.detect_ms} ms` : ""}`}>
          BFD {b.family} {b.state}
        </span>
      ))}
      {a.waiting_for_bfd && <span className="tag tag-warn">waiting for BFD</span>}
      {a.restarting && <span className="tag tag-warn">restarting (helping)</span>}
      {a.suppressed && <span className="tag">SA: not advertised</span>}
    </>
  );
}

const multi = (s: ISISStatus) => (s.interfaces?.length ?? 1) > 1;

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  if (multi(s)) {
    const ifs = s.interfaces!;
    const up = (s.adjacencies ?? []).filter((x) => x.state === "up").length;
    const active = ifs.filter((i) => !i.passive);
    const live = active.filter((i) => i.adjacencies_up > 0).length;
    const tail = `${s.lsdb.filter((l) => l.lifetime > 0).length} LSPs${s.overload ? " · overload" : ""}${s.graceful_restart?.restarting ? " · restarting" : ""}`;
    if (!up) return { health: "bad", text: `no adjacencies on ${ifs.length} interfaces` };
    return {
      health: live < active.length || s.overload ? "warn" : "ok",
      text: `${up} neighbor${up > 1 ? "s" : ""} on ${live}/${active.length} interfaces · ${tail}`,
    };
  }
  if (s.lan) {
    const up = (s.adjacencies ?? []).filter((x) => x.state === "up").length;
    const disNames = s.lan.map((l) => (l.we_are_dis ? "us" : hostOf(s, l.dis))).join("/");
    const tail = `${s.lsdb.filter((l) => l.lifetime > 0).length} LSPs${s.overload ? " · overload" : ""}`;
    if (up) return { health: s.overload ? "warn" : "ok", text: `${up} neighbor${up > 1 ? "s" : ""} · DIS ${disNames} · ${tail}` };
    return { health: "bad", text: s.flapping ? "down (flap)" : "no LAN neighbors" };
  }
  const a = s.adjacency;
  const who = a.hostname || a.neighbor;
  const tail = `${s.lsdb.filter((l) => l.lifetime > 0).length} LSPs${s.overload ? " · overload" : ""}`;
  if (a.state === "up") return { health: s.overload ? "warn" : "ok", text: `up with ${who} · ${tail}` };
  if (a.state === "initializing") return { health: "warn", text: `initializing with ${who}` };
  return { health: "bad", text: s.flapping ? "down (flap)" : "adjacency down" };
}

function hostOf(s: ISISStatus, sys: string): string {
  const l = s.lsdb.find((x) => x.lsp_id.startsWith(sys + ".00") && x.hostname);
  return l?.hostname ?? sys;
}

function Interfaces({ s }: { s: ISISStatus }) {
  return (
    <div className="table-wrap">
      <table className="table dense">
        <thead>
          <tr>
            <th>Interface</th>
            <th>Circuit</th>
            <th className="num">Metric</th>
            <th className="num">Up</th>
            <th>Addresses</th>
            <th>Hello auth</th>
          </tr>
        </thead>
        <tbody>
          {(s.interfaces ?? []).map((i) => (
            <tr key={i.name}>
              <td className="mono">
                {i.name}
                {!i.attached && <span className="tag tag-warn" title={i.error}>detached</span>}
                {i.passive && <span className="tag">passive</span>}
                {i.flapping && <span className="tag tag-warn">flapping</span>}
              </td>
              <td>
                {i.circuit}
                {i.circuit === "broadcast" && <span className="muted"> · prio {i.priority ?? 0}</span>}
              </td>
              <td className="num mono">{i.metric}</td>
              <td className="num mono">{i.passive ? "—" : i.adjacencies_up}</td>
              <td className="mono">{i.addresses?.join(", ") || (i.attached ? "—" : i.error)}</td>
              <td className="mono">{i.hello_auth}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function LANAdjacencies({ s }: { s: ISISStatus }) {
  const m = multi(s);
  return (
    <>
      {(s.lan ?? []).map((l) => (
        <div key={`${l.interface}/${l.level}`} className="flow-path mono small-note">
          {m && `${l.interface} `}L{l.level} DIS: <b>{l.we_are_dis ? `us (${s.hostname})` : hostOf(s, l.dis)}</b> · LAN ID {l.lan_id} · our priority {l.priority} · {l.elections} election
          {l.elections === 1 ? "" : "s"}
        </div>
      ))}
      {(s.adjacencies ?? []).length === 0 ? (
        <div className="muted">{m ? "No neighbors heard on any interface." : "No neighbors heard on the LAN."}</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                {m && <th>Interface</th>}
                <th>Neighbor</th>
                <th>State</th>
                <th>Level</th>
                <th className="num">Priority</th>
                <th className="num">Hold</th>
                <th>MAC</th>
                <th>Addresses</th>
              </tr>
            </thead>
            <tbody>
              {(s.adjacencies ?? []).map((a: Adj) => (
                <tr key={`${a.interface}/${a.levels?.[0]}/${a.mac}`}>
                  {m && <td className="mono">{a.interface}</td>}
                  <td className="mono">
                    {a.hostname ? `${a.hostname} · ` : ""}
                    {a.neighbor}
                    {a.dis && <span className="tag">DIS</span>}
                    <AdjExtras a={a} />
                  </td>
                  <td>
                    <span className={a.state === "up" ? "pill pill-on" : "pill pill-warn"}>{a.state}</span>
                  </td>
                  <td>L{a.levels?.join("/L")}</td>
                  <td className="num mono">{a.priority}</td>
                  <td className="num mono">{a.hold_remaining_s !== undefined ? `${Math.max(0, a.hold_remaining_s).toFixed(0)} s` : "—"}</td>
                  <td className="mono">{a.mac}</td>
                  <td className="mono">{a.addresses?.join(", ")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

// Names LSP IDs by hostname where the LSDB knows one.
function namer(rows: LSPRow[]) {
  const names = new Map<string, string>();
  for (const r of rows) if (r.hostname) names.set(r.lsp_id.slice(0, 14), r.hostname);
  return (id: string) => {
    const h = names.get(id.slice(0, 14));
    return h ? `${h}${id.slice(14)}` : id;
  };
}

function Status({ status, agentId, name: inst }: { status: JsonObject; agentId?: string; name?: string }) {
  const s = as(status);
  const a = s.adjacency;
  const [open, setOpen] = useState<string>();
  const name = namer(s.lsdb);
  const pill = a.state === "up" ? "pill pill-on" : a.state === "initializing" ? "pill pill-warn" : "pill";
  const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) => x.localeCompare(y));
  return (
    <div className="isis">
      <dl className="kv">
        <div className="kv-row">
          <dt>system ID</dt>
          <dd className="mono">
            {s.system_id} ({s.hostname})
          </dd>
        </div>
        <div className="kv-row">
          <dt>level · circuit</dt>
          <dd className="mono">
            {s.level} · {s.circuit}
          </dd>
        </div>
        <div className="kv-row">
          <dt>areas</dt>
          <dd className="mono">{s.areas.join(", ")}</dd>
        </div>
        <div className="kv-row">
          <dt>metric · overload</dt>
          <dd className="mono">
            {s.metric} · {s.overload ? <span className="warn-text">set</span> : "clear"}
            {s.overload_on_startup_until && <span className="muted"> (on startup, until {new Date(s.overload_on_startup_until).toLocaleTimeString()})</span>}
          </dd>
        </div>
        {s.inter_level && (
          <div className="kv-row">
            <dt>{s.level === "L1L2" ? "between levels" : "attached routers"}</dt>
            <dd className="mono">
              {s.level === "L1L2"
                ? `ATT ${s.inter_level.attached ? "set" : "clear"} · ${s.inter_level.to_l2} into L2 · ${s.inter_level.leaked_to_l1} leaked into L1`
                : s.inter_level.attached_routers.map((r) => hostOf(s, r)).join(", ") || "none (no way out of the area)"}
            </dd>
          </div>
        )}
        <div className="kv-row">
          <dt>graceful restart · BFD</dt>
          <dd className="mono">
            {s.graceful_restart?.restarting ? (
              <span className="warn-text">
                restarting: {s.graceful_restart.restarting.acked} acked, {s.graceful_restart.restarting.synced} synced, {s.graceful_restart.restarting.pending} LSPs to go,{" "}
                {s.graceful_restart.restarting.remaining_s} s left
              </span>
            ) : (
              `helper ${s.graceful_restart?.helper ? "on" : "off"}`
            )}{" "}
            · BFD {s.bfd ? "on" : "off"}
          </dd>
        </div>
        <div className="kv-row">
          <dt>topologies</dt>
          <dd className="mono">{(s.topologies ?? []).join(", ")}</dd>
        </div>
        {s.segment_routing && (
          <div className="kv-row kv-wide">
            <dt>segment routing</dt>
            <dd className="mono">
              {s.segment_routing.mpls !== false && (
                <>
                  SRGB {s.segment_routing.srgb_base}–{s.segment_routing.srgb_base + s.segment_routing.srgb_size - 1}
                  {s.segment_routing.srlb && ` · SRLB ${ranges([s.segment_routing.srlb])}`}
                  {(s.segment_routing.node_sids ?? []).map((n) => (
                    <div key={`${n.prefix}/${n.algorithm ?? 0}`}>
                      {n.prefix} → index {n.index} (label {n.label}){n.algorithm ? ` algo ${n.algorithm}` : ""}
                      {n.flags ? ` ${n.flags}` : ""}
                    </div>
                  ))}
                  <div className="muted">
                    adj SIDs:{" "}
                    {(s.segment_routing.adj_sids ?? []).map((a) => `${a.label}${a.ipv6 ? " v6" : ""} → ${hostOf(s, a.neighbor.slice(0, 14))}`).join(", ") || "none"}
                  </div>
                  {s.segment_routing.mappings?.map((b) => (
                    <div key={b.prefix} className="muted">
                      mapping {b.prefix} ×{b.range} → index {b.index}
                    </div>
                  ))}
                </>
              )}
              {s.segment_routing.flex_algos?.map((f) => (
                <div key={f.algorithm} className="muted">
                  flex-{fadText(f)}
                  {f.participate_only ? " (participating; defined elsewhere)" : ""}
                </div>
              ))}
              {s.segment_routing.srv6_sids?.map((x) => (
                <div key={x.sid} className="muted">
                  SRv6 {x.sid} {x.behavior}
                  {x.locator ? ` (locator ${x.locator}${x.algorithm ? `, algo ${x.algorithm}` : ""})` : ` → ${hostOf(s, (x.neighbor ?? "").slice(0, 14))}`}
                </div>
              ))}
            </dd>
          </div>
        )}
        <div className="kv-row">
          <dt>authentication</dt>
          <dd className="mono">
            hellos {s.hello_auth ?? "none"} · LSPs {s.lsp_auth ?? "none"}
            {Object.entries(s.counters ?? {}).some(([k, v]) => k.startsWith("rx_auth_failures") && v > 0) && <span className="warn-text"> · failures</span>}
          </dd>
        </div>
        <div className="kv-row">
          <dt>advertised prefixes</dt>
          <dd className="mono">
            {s.advertised_prefixes} injected (+ connected{s.loopbacks?.length ? `, loopbacks ${s.loopbacks.join(", ")}` : ""})
          </dd>
        </div>
      </dl>

      {multi(s) && (
        <>
          <h4>Interfaces</h4>
          <Interfaces s={s} />
        </>
      )}
      <h4>{multi(s) ? "Adjacencies" : s.lan ? "LAN adjacencies" : "Adjacency"}</h4>
      {multi(s) || s.lan ? (
        <LANAdjacencies s={s} />
      ) : (
        <>
      <div className="flow-head">
        <div className="flow-path mono">
          <span className={pill}>{a.state}</span> {a.neighbor ? `${a.hostname ? a.hostname + " · " : ""}${a.neighbor}` : "no neighbor"}
          {a.levels && <span className="muted"> · L{a.levels.join("/L")}</span>}
          {a.topologies && <span className="muted"> · {a.topologies.join(", ")}</span>}
          <AdjExtras a={a} />
        </div>
        <div className="muted">
          {a.hold_remaining_s !== undefined && `hold ${Math.max(0, a.hold_remaining_s).toFixed(0)} s · `}
          {a.since && `since ${new Date(a.since).toLocaleTimeString()} · `}
          {a.three_way ? "3-way" : "2-way"} · {a.transitions} transitions
          {s.flapping && <span className="warn-text"> · flapping</span>}
        </div>
      </div>
      {a.addresses && a.addresses.length > 0 && <div className="muted mono small-note">neighbor addresses: {a.addresses.join(", ")}</div>}
        </>
      )}

      {s.topology && (
        <>
          <h4>Emulated topology</h4>
          <div className="flow-path mono small-note">
            {s.topology.kind} · {s.topology.nodes} routers · {s.topology.links} links · {s.topology.prefixes} prefixes · attached to us: {s.topology.attached.join(", ") || "none"}
          </div>
          {(s.topology.down_nodes?.length || s.topology.down_links?.length || s.topology.overloaded?.length) ? (
            <div className="warn-box">
              {s.topology.down_nodes?.length ? <div>routers down: {s.topology.down_nodes.join(", ")}</div> : null}
              {s.topology.down_links?.length ? <div>links down: {s.topology.down_links.join(", ")}</div> : null}
              {s.topology.overloaded?.length ? <div>overloaded: {s.topology.overloaded.join(", ")}</div> : null}
            </div>
          ) : (
            <div className="muted small-note">All simulated routers and links are up. Use the topology-link and topology-node actions to fail them.</div>
          )}
        </>
      )}

      <h4>Link-state database</h4>
      {s.lsdb_total !== undefined && s.lsdb_total > s.lsdb.length && (
        <div className="muted small-note">
          Showing {s.lsdb.length} of {s.lsdb_total} LSPs.
        </div>
      )}
      <div className="table-wrap">
        <table className="table dense hover">
          <thead>
            <tr>
              <th>Level</th>
              <th>LSP</th>
              <th className="num">Seq</th>
              <th className="num">Lifetime</th>
              <th>Checksum</th>
              <th className="num">Neighbors</th>
              <th className="num">Prefixes</th>
              <th>Flags</th>
            </tr>
          </thead>
          <tbody>
            {s.lsdb.map((l) => {
              const key = `${l.level}/${l.lsp_id}`;
              const purged = l.lifetime === 0;
              return (
                <Fragment key={key}>
                  <tr onClick={() => setOpen(open === key ? undefined : key)} className={purged ? "muted" : ""}>
                    <td>L{l.level}</td>
                    <td className="mono">
                      {open === key ? "▾ " : "▸ "}
                      {name(l.lsp_id)}
                      {l.own && <span className="tag">ours</span>}
                    </td>
                    <td className="num mono">{`0x${l.seq.toString(16)}`}</td>
                    <td className="num mono">{purged ? "purged" : `${l.lifetime} s`}</td>
                    <td className="mono">{l.checksum}</td>
                    <td className="num mono">{l.neighbors}</td>
                    <td className="num mono">{l.prefixes}</td>
                    <td>{l.overload && <span className="pill pill-warn">OL</span>}</td>
                  </tr>
                  {open === key && (
                    <tr className="lsp-detail">
                      <td colSpan={8}>
                        <LSPDetailRow agentId={agentId} name={inst} row={l} nameOf={name} />
                      </td>
                    </tr>
                  )}
                </Fragment>
              );
            })}
          </tbody>
        </table>
      </div>

      <h4>Counters</h4>
      <div className="counters">
        {counters.map(([k, v]) => (
          <div className="counter" key={k}>
            <span className="counter-k">{k.replace(/_/g, " ")}</span>
            <span className={/error|mismatch/.test(k) && v ? "counter-v warn-text" : "counter-v"}>{v}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

registerView("isis", { summary, Status });
