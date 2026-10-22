import { Fragment, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { ValueView } from "../components/ValueView";
import { registerView, type Health } from "./registry";

type If = {
  name: string;
  network: string;
  state: string;
  address?: string;
  cost: number;
  priority?: number;
  passive?: boolean;
  dr?: string;
  bdr?: string;
  neighbors: number;
  full: number;
  auth: string;
  error?: string;
  last_error?: string;
  flapping?: boolean;
};
type Nbr = {
  interface: string;
  router_id: string;
  address: string;
  state: string;
  priority: number;
  role?: string;
  dead_in_s: number;
  since: string;
  reason?: string;
  requests?: number;
  retransmit?: number;
};
type LSA = { type: string; id: string; adv_router: string; seq: string; age: number; checksum: string; length: number; own?: boolean; summary?: string };
type OSPFStatus = {
  router_id: string;
  area: string;
  max_metric?: boolean;
  asbr?: boolean;
  auth: string;
  loopbacks?: string[];
  advertised_prefixes: number;
  interfaces: If[];
  neighbors: Nbr[];
  lsdb: LSA[];
  lsdb_total: number;
  lsdb_types: Record<string, number>;
  counters: Record<string, number>;
};

const as = (s: JsonObject) => s as unknown as OSPFStatus;
const active = (s: OSPFStatus) => s.interfaces.filter((i) => !i.passive);

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const ifs = active(s);
  const full = s.neighbors.filter((n) => n.state === "full").length;
  const live = ifs.filter((i) => i.full > 0).length;
  const tail = `${s.lsdb_total} LSAs${s.max_metric ? " · max-metric" : ""}`;
  if (ifs.length === 0) return { health: "ok", text: `passive · ${tail}` };
  if (ifs.some((i) => i.flapping) && !full) return { health: "bad", text: "down (flap)" };
  if (!full) {
    const best = s.neighbors.map((n) => n.state).sort()[0];
    return { health: s.neighbors.length ? "warn" : "bad", text: s.neighbors.length ? `no full adjacency (${best})` : "no neighbors" };
  }
  const roles = ifs
    .filter((i) => i.network === "broadcast")
    .map((i) => i.state)
    .filter((x) => x === "dr" || x === "backup");
  const who = full === 1 ? s.neighbors.find((n) => n.state === "full")!.router_id : `${full} neighbors`;
  return {
    health: live < ifs.length || s.max_metric ? "warn" : "ok",
    text: `full with ${who}${ifs.length > 1 ? ` on ${live}/${ifs.length} interfaces` : ""}${roles.length ? ` · ${roles.join("/").toUpperCase().replace("BACKUP", "BDR")}` : ""} · ${tail}`,
  };
}

const pill = (state: string) => (state === "full" ? "pill pill-on" : state === "2-way" ? "pill" : "pill pill-warn");

function LSADetail({ agentId, name, row }: { agentId?: string; name?: string; row: LSA }) {
  const d = usePoll(
    async () => {
      if (!agentId || !name) return undefined;
      const r = await client(agentId).runProtocolAction({ name, action: "lsa", args: { type: row.type, id: row.id, adv_router: row.adv_router } });
      return r.result;
    },
    [agentId, name, row.type, row.id, row.adv_router],
    3000,
  );
  if (d.error) return <div className="error-text">{d.error}</div>;
  if (!d.data) return <div className="muted">Loading…</div>;
  const { header: _h, ...rest } = d.data as JsonObject;
  return <ValueView value={rest} />;
}

function Status({ status, agentId, name: inst }: { status: JsonObject; agentId?: string; name?: string }) {
  const s = as(status);
  const [open, setOpen] = useState<string>();
  const multi = s.interfaces.length > 1;
  const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) => x.localeCompare(y));
  const types = Object.entries(s.lsdb_types ?? {})
    .map(([k, v]) => `${v} ${k}`)
    .join(" · ");
  return (
    <div className="isis">
      <dl className="kv">
        <div className="kv-row">
          <dt>router ID · area</dt>
          <dd className="mono">
            {s.router_id} · {s.area}
          </dd>
        </div>
        <div className="kv-row">
          <dt>roles</dt>
          <dd className="mono">
            {[s.asbr && "ASBR", s.max_metric && "max-metric"].filter(Boolean).join(" · ") || "internal router"}
          </dd>
        </div>
        <div className="kv-row">
          <dt>authentication</dt>
          <dd className="mono">
            {s.auth}
            {(s.counters?.rx_auth_failures ?? 0) > 0 && <span className="warn-text"> · {s.counters.rx_auth_failures} failures</span>}
          </dd>
        </div>
        <div className="kv-row">
          <dt>advertised prefixes</dt>
          <dd className="mono">
            {s.advertised_prefixes} injected (+ interfaces{s.loopbacks?.length ? `, loopbacks ${s.loopbacks.join(", ")}` : ""})
          </dd>
        </div>
      </dl>

      <h4>Interfaces</h4>
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>Interface</th>
              <th>Network</th>
              <th>State</th>
              <th className="num">Cost</th>
              <th>DR · BDR</th>
              <th className="num">Full</th>
              <th>Address</th>
              <th>Auth</th>
            </tr>
          </thead>
          <tbody>
            {s.interfaces.map((i) => (
              <tr key={i.name}>
                <td className="mono">{i.name}</td>
                <td className="mono">
                  {i.network}
                  {i.network === "broadcast" && <span className="muted"> · prio {i.priority ?? 0}</span>}
                </td>
                <td>
                  <span className={i.state === "down" ? "pill pill-warn" : "pill pill-on"}>{i.passive ? "passive" : i.state}</span>
                  {i.flapping && <span className="tag tag-warn">flapping</span>}
                </td>
                <td className="num mono">{i.cost}</td>
                <td className="mono">{i.network === "broadcast" ? `${i.dr ?? "—"} · ${i.bdr ?? "—"}` : "—"}</td>
                <td className="num mono">
                  {i.full}/{i.neighbors}
                </td>
                <td className="mono">{i.address ?? i.error}</td>
                <td className="mono">
                  {i.auth}
                  {i.last_error && (
                    <div className="warn-text small" title="Last rejected packet">
                      {i.last_error}
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <h4>Neighbors</h4>
      {s.neighbors.length === 0 ? (
        <div className="muted">No neighbors heard.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                {multi && <th>Interface</th>}
                <th>Router ID</th>
                <th>Address</th>
                <th>State</th>
                <th className="num">Priority</th>
                <th className="num">Dead</th>
                <th>Since</th>
                <th>Last change</th>
              </tr>
            </thead>
            <tbody>
              {s.neighbors.map((n) => (
                <tr key={`${n.interface}/${n.router_id}`}>
                  {multi && <td className="mono">{n.interface}</td>}
                  <td className="mono">
                    {n.router_id}
                    {n.role && <span className="tag">{n.role.toUpperCase()}</span>}
                  </td>
                  <td className="mono">{n.address}</td>
                  <td>
                    <span className={pill(n.state)}>{n.state}</span>
                    {(n.requests ?? 0) > 0 && <span className="muted small"> · {n.requests} req</span>}
                    {(n.retransmit ?? 0) > 0 && <span className="muted small"> · {n.retransmit} rxmt</span>}
                  </td>
                  <td className="num mono">{n.priority}</td>
                  <td className="num mono">{Math.max(0, n.dead_in_s).toFixed(0)} s</td>
                  <td className="mono">{new Date(n.since).toLocaleTimeString()}</td>
                  <td className="muted">{n.reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <h4>Link-state database</h4>
      <div className="muted small-note">
        {types || "empty"}
        {s.lsdb_total > s.lsdb.length && ` · showing ${s.lsdb.length} of ${s.lsdb_total}`}
      </div>
      <div className="table-wrap">
        <table className="table dense hover">
          <thead>
            <tr>
              <th>Type</th>
              <th>Link state ID</th>
              <th>Advertising router</th>
              <th className="num">Seq</th>
              <th className="num">Age</th>
              <th>Checksum</th>
              <th>Content</th>
            </tr>
          </thead>
          <tbody>
            {s.lsdb.map((l) => {
              const key = `${l.type}/${l.id}/${l.adv_router}`;
              const flushed = l.age >= 3600;
              return (
                <Fragment key={key}>
                  <tr onClick={() => setOpen(open === key ? undefined : key)} className={flushed ? "muted" : ""}>
                    <td>{l.type}</td>
                    <td className="mono">
                      {open === key ? "▾ " : "▸ "}
                      {l.id}
                    </td>
                    <td className="mono">
                      {l.adv_router}
                      {l.own && <span className="tag">ours</span>}
                    </td>
                    <td className="num mono">{l.seq}</td>
                    <td className="num mono">{flushed ? "MaxAge" : `${l.age} s`}</td>
                    <td className="mono">{l.checksum}</td>
                    <td className="mono">{l.summary}</td>
                  </tr>
                  {open === key && (
                    <tr className="lsp-detail">
                      <td colSpan={7}>
                        <LSADetail agentId={agentId} name={inst} row={l} />
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
            <span className={/error|mismatch|failure/.test(k) && v ? "counter-v warn-text" : "counter-v"}>{v}</span>
          </div>
        ))}
      </div>
    </div>
  );
}

registerView("ospf", { summary, Status });
