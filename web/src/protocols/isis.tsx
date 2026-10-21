import { Fragment, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { registerView, type Health } from "./registry";

type Nbr = { id: string; metric: number };
type Pfx = { prefix: string; metric: number; down?: boolean };
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
  areas?: string[];
  router_id?: string;
  addresses?: string[];
  neighbors?: Nbr[];
  prefixes?: Pfx[];
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
          <div key={n.id} className="mono">
            {nameOf(n.id)} <span className="muted">metric {n.metric}</span>
          </div>
        ))}
        {!l.neighbors?.length && <div className="muted">none</div>}
        {l.router_id && <div className="mono muted">router ID {l.router_id}</div>}
        {l.addresses && <div className="mono muted">{l.addresses.join(", ")}</div>}
      </div>
      <div>
        <div className="field-label">prefixes ({l.prefixes?.length ?? 0})</div>
        <div className="pfx-list mono">
          {(l.prefixes ?? []).slice(0, 500).map((p) => (
            <div key={p.prefix}>
              {p.prefix} <span className="muted">{p.metric}</span>
              {p.down && <span className="tag">down</span>}
            </div>
          ))}
          {(l.prefixes?.length ?? 0) > 500 && <div className="muted">… {l.prefixes!.length - 500} more</div>}
        </div>
      </div>
    </div>
  );
}
type Adj = ISISStatus["adjacency"];
type LANRow = { level: number; we_are_dis: boolean; dis: string; lan_id: string; priority: number; elections: number };
type ISISStatus = {
  system_id: string;
  hostname: string;
  level: string;
  circuit: string;
  areas: string[];
  metric: number;
  overload: boolean;
  flapping?: boolean;
  advertised_prefixes: number;
  adjacency: {
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

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
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

function LANAdjacencies({ s }: { s: ISISStatus }) {
  return (
    <>
      {(s.lan ?? []).map((l) => (
        <div key={l.level} className="flow-path mono small-note">
          L{l.level} DIS: <b>{l.we_are_dis ? `us (${s.hostname})` : hostOf(s, l.dis)}</b> · LAN ID {l.lan_id} · our priority {l.priority} · {l.elections} election
          {l.elections === 1 ? "" : "s"}
        </div>
      ))}
      {(s.adjacencies ?? []).length === 0 ? (
        <div className="muted">No neighbors heard on the LAN.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
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
                <tr key={`${a.levels?.[0]}/${a.mac}`}>
                  <td className="mono">
                    {a.hostname ? `${a.hostname} · ` : ""}
                    {a.neighbor}
                    {a.dis && <span className="tag">DIS</span>}
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
          </dd>
        </div>
        <div className="kv-row">
          <dt>advertised prefixes</dt>
          <dd className="mono">{s.advertised_prefixes} injected (+ connected)</dd>
        </div>
      </dl>

      <h4>{s.lan ? "LAN adjacencies" : "Adjacency"}</h4>
      {s.lan ? (
        <LANAdjacencies s={s} />
      ) : (
        <>
      <div className="flow-head">
        <div className="flow-path mono">
          <span className={pill}>{a.state}</span> {a.neighbor ? `${a.hostname ? a.hostname + " · " : ""}${a.neighbor}` : "no neighbor"}
          {a.levels && <span className="muted"> · L{a.levels.join("/L")}</span>}
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
