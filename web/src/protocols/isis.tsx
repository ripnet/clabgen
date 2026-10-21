import { Fragment, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { registerView, type Health } from "./registry";

type Nbr = { id: string; metric: number };
type Pfx = { prefix: string; metric: number; down?: boolean };
type LSPRow = {
  level: number;
  lsp_id: string;
  seq: number;
  lifetime: number;
  checksum: string;
  own?: boolean;
  hostname?: string;
  overload?: boolean;
  areas?: string[];
  router_id?: string;
  addresses?: string[];
  neighbors?: Nbr[];
  prefixes?: Pfx[];
};
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
  };
  lsdb: LSPRow[];
  counters: Record<string, number>;
};

const as = (s: JsonObject) => s as unknown as ISISStatus;

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const a = s.adjacency;
  const who = a.hostname || a.neighbor;
  const tail = `${s.lsdb.filter((l) => l.lifetime > 0).length} LSPs${s.overload ? " · overload" : ""}`;
  if (a.state === "up") return { health: s.overload ? "warn" : "ok", text: `up with ${who} · ${tail}` };
  if (a.state === "initializing") return { health: "warn", text: `initializing with ${who}` };
  return { health: "bad", text: s.flapping ? "down (flap)" : "adjacency down" };
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

function Status({ status }: { status: JsonObject }) {
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

      <h4>Adjacency</h4>
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

      <h4>Link-state database</h4>
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
                    <td className="num mono">{l.neighbors?.length ?? 0}</td>
                    <td className="num mono">{l.prefixes?.length ?? 0}</td>
                    <td>{l.overload && <span className="pill pill-warn">OL</span>}</td>
                  </tr>
                  {open === key && (
                    <tr className="lsp-detail">
                      <td colSpan={8}>
                        <div className="lsp-grid">
                          <div>
                            <div className="field-label">neighbors</div>
                            {(l.neighbors ?? []).map((n) => (
                              <div key={n.id} className="mono">
                                {name(n.id)} <span className="muted">metric {n.metric}</span>
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
