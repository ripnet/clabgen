import { Fragment, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { ValueView } from "../components/ValueView";
import { registerView, type Health } from "./registry";

type If = {
  name: string;
  network: string;
  area?: string;
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
  bfd?: {
    state: string;
    remote_state?: string;
    detect_ms?: number;
    last_down?: string;
    silent?: boolean;
  };
};
type LSA = {
  type: string;
  id: string;
  adv_router: string;
  seq: string;
  age: number;
  checksum: string;
  length: number;
  own?: boolean;
  area?: string; // area-scoped: the area
  link?: string; // link-scoped (OSPFv3 link LSAs): the interface
  summary?: string;
};
type Area = {
  id: string;
  type: string;
  no_summary?: boolean;
  interfaces: string[];
  topology?: boolean;
  lsas: number;
  ranges?: string[];
  reachable_routers?: number;
  intra_area_prefixes?: number;
  nssa_translator?: boolean;
};
type SR = {
  srgb?: string;
  srlb?: string;
  algorithms: number[];
  traffic_engineering?: boolean;
  node_sids?: {
    prefix: string;
    index: number;
    label: number;
    algorithm?: number;
    router?: string;
  }[];
  adj_sids?: {
    interface: string;
    neighbor: string;
    label: number;
    lan?: boolean;
  }[];
};
type GR = {
  helper: boolean;
  restart?: string;
  restart_until?: string;
  helping?: string[];
  waiting_for?: number;
};
type OSPFStatus = {
  router_id: string;
  area: string;
  areas?: Area[];
  abr?: boolean;
  segment_routing?: SR;
  graceful_restart?: GR;
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
  topology?: Topo;
  counters: Record<string, number>;
};
type Topo = {
  kind: string;
  nodes: number;
  links: number;
  prefixes: number;
  attached: string[];
  down_nodes?: string[];
  down_links?: string[];
  overloaded?: string[];
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
  if (ifs.some((i) => i.flapping) && !full)
    return { health: "bad", text: "down (flap)" };
  if (!full) {
    const best = s.neighbors.map((n) => n.state).sort()[0];
    return {
      health: s.neighbors.length ? "warn" : "bad",
      text: s.neighbors.length ? `no full adjacency (${best})` : "no neighbors",
    };
  }
  const roles = ifs
    .filter((i) => i.network === "broadcast")
    .map((i) => i.state)
    .filter((x) => x === "dr" || x === "backup");
  const who =
    full === 1
      ? s.neighbors.find((n) => n.state === "full")!.router_id
      : `${full} neighbors`;
  return {
    health: live < ifs.length || s.max_metric ? "warn" : "ok",
    text: `full with ${who}${ifs.length > 1 ? ` on ${live}/${ifs.length} interfaces` : ""}${roles.length ? ` · ${roles.join("/").toUpperCase().replace("BACKUP", "BDR")}` : ""} · ${tail}`,
  };
}

const pill = (state: string) =>
  state === "full"
    ? "pill pill-on"
    : state === "2-way"
      ? "pill"
      : "pill pill-warn";

function LSADetail({
  agentId,
  name,
  row,
}: {
  agentId?: string;
  name?: string;
  row: LSA;
}) {
  const d = usePoll(
    async () => {
      if (!agentId || !name) return undefined;
      const r = await client(agentId).runProtocolAction({
        name,
        action: "lsa",
        args: {
          type: row.type,
          id: row.id,
          adv_router: row.adv_router,
          area: row.area ?? (row.link ? "" : "as"),
        },
      });
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

function Status({
  status,
  agentId,
  name: inst,
}: {
  status: JsonObject;
  agentId?: string;
  name?: string;
}) {
  const s = as(status);
  const [open, setOpen] = useState<string>();
  const multi = s.interfaces.length > 1;
  const multiArea = (s.areas?.length ?? 0) > 1;
  const anyBFD = s.neighbors.some((n) => n.bfd);
  const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) =>
    x.localeCompare(y),
  );
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
            {[s.abr && "ABR", s.asbr && "ASBR", s.max_metric && "max-metric"]
              .filter(Boolean)
              .join(" · ") || "internal router"}
          </dd>
        </div>
        <div className="kv-row">
          <dt>authentication</dt>
          <dd className="mono">
            {s.auth}
            {(s.counters?.rx_auth_failures ?? 0) > 0 && (
              <span className="warn-text">
                {" "}
                · {s.counters.rx_auth_failures} failures
              </span>
            )}
          </dd>
        </div>
        {s.graceful_restart && (
          <div className="kv-row">
            <dt>graceful restart</dt>
            <dd className="mono">
              {s.graceful_restart.restart ? (
                <span className="warn-text">
                  restarting ({s.graceful_restart.restart}
                  {s.graceful_restart.waiting_for
                    ? `, waiting for ${s.graceful_restart.waiting_for} adjacencies`
                    : ""}
                  )
                </span>
              ) : (
                `helper ${s.graceful_restart.helper ? "on" : "off"}`
              )}
              {s.graceful_restart.helping?.length
                ? ` · helping ${s.graceful_restart.helping.join(", ")}`
                : ""}
            </dd>
          </div>
        )}
        <div className="kv-row">
          <dt>advertised prefixes</dt>
          <dd className="mono">
            {s.advertised_prefixes} injected (+ interfaces
            {s.loopbacks?.length ? `, loopbacks ${s.loopbacks.join(", ")}` : ""}
            )
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
              {multiArea && <th>Area</th>}
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
                  {(i.network === "broadcast" ||
                    i.network === "non-broadcast") && (
                    <span className="muted"> · prio {i.priority ?? 0}</span>
                  )}
                </td>
                {multiArea && <td className="mono">{i.area}</td>}
                <td>
                  <span
                    className={
                      i.state === "down" ? "pill pill-warn" : "pill pill-on"
                    }
                  >
                    {i.passive ? "passive" : i.state}
                  </span>
                  {i.flapping && <span className="tag tag-warn">flapping</span>}
                </td>
                <td className="num mono">{i.cost}</td>
                <td className="mono">
                  {i.network === "broadcast" || i.network === "non-broadcast"
                    ? `${i.dr ?? "—"} · ${i.bdr ?? "—"}`
                    : "—"}
                </td>
                <td className="num mono">
                  {i.full}/{i.neighbors}
                </td>
                <td className="mono">{i.address ?? i.error}</td>
                <td className="mono">
                  {i.auth}
                  {i.last_error && (
                    <div
                      className="warn-text small"
                      title="Last rejected packet"
                    >
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
                {anyBFD && <th>BFD</th>}
                <th>Last change</th>
              </tr>
            </thead>
            <tbody>
              {s.neighbors.map((n) => (
                <tr key={`${n.interface}/${n.router_id}`}>
                  {multi && <td className="mono">{n.interface}</td>}
                  <td className="mono">
                    {n.router_id}
                    {n.role && (
                      <span className="tag">{n.role.toUpperCase()}</span>
                    )}
                  </td>
                  <td className="mono">{n.address}</td>
                  <td>
                    <span className={pill(n.state)}>{n.state}</span>
                    {(n.requests ?? 0) > 0 && (
                      <span className="muted small"> · {n.requests} req</span>
                    )}
                    {(n.retransmit ?? 0) > 0 && (
                      <span className="muted small">
                        {" "}
                        · {n.retransmit} rxmt
                      </span>
                    )}
                  </td>
                  <td className="num mono">{n.priority}</td>
                  <td className="num mono">
                    {Math.max(0, n.dead_in_s).toFixed(0)} s
                  </td>
                  <td className="mono">
                    {new Date(n.since).toLocaleTimeString()}
                  </td>
                  {anyBFD && (
                    <td>
                      {n.bfd ? (
                        <span
                          className={
                            n.bfd.state === "up"
                              ? "pill pill-on"
                              : "pill pill-warn"
                          }
                          title={
                            n.bfd.last_down
                              ? `last down: ${n.bfd.last_down}`
                              : undefined
                          }
                        >
                          {n.bfd.state}
                          {n.bfd.detect_ms ? ` · ${n.bfd.detect_ms} ms` : ""}
                          {n.bfd.silent ? " · silent" : ""}
                        </span>
                      ) : (
                        "—"
                      )}
                    </td>
                  )}
                  <td className="muted">{n.reason}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {multiArea && (
        <>
          <h4>Areas</h4>
          <div className="table-wrap">
            <table className="table dense">
              <thead>
                <tr>
                  <th>Area</th>
                  <th>Type</th>
                  <th>Interfaces</th>
                  <th className="num">LSAs</th>
                  <th>Ranges</th>
                  <th className="num">Routers</th>
                  <th className="num">Prefixes</th>
                </tr>
              </thead>
              <tbody>
                {s.areas!.map((a) => (
                  <tr key={a.id}>
                    <td className="mono">{a.id}</td>
                    <td>
                      {a.type}
                      {a.no_summary && <span className="tag">no summary</span>}
                      {a.nssa_translator && (
                        <span className="tag">translator</span>
                      )}
                    </td>
                    <td className="mono">
                      {[...a.interfaces, a.topology ? "emulated topology" : ""]
                        .filter(Boolean)
                        .join(", ") || "—"}
                    </td>
                    <td className="num mono">{a.lsas}</td>
                    <td className="mono">{a.ranges?.join(", ") ?? "—"}</td>
                    <td className="num mono">{a.reachable_routers ?? "—"}</td>
                    <td className="num mono">{a.intra_area_prefixes ?? "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}

      {s.segment_routing && (
        <>
          <h4>Segment routing</h4>
          <div className="flow-path mono small-note">
            {[
              s.segment_routing.srgb && `SRGB ${s.segment_routing.srgb}`,
              s.segment_routing.srlb && `SRLB ${s.segment_routing.srlb}`,
              `algorithms ${s.segment_routing.algorithms.join(", ")}`,
              s.segment_routing.traffic_engineering && "TE LSAs",
            ]
              .filter(Boolean)
              .join(" · ")}
          </div>
          {(s.segment_routing.node_sids?.length ?? 0) > 0 && (
            <div className="table-wrap">
              <table className="table dense">
                <thead>
                  <tr>
                    <th>Prefix</th>
                    <th className="num">Index</th>
                    <th className="num">Label</th>
                    <th className="num">Algorithm</th>
                    <th>Router</th>
                  </tr>
                </thead>
                <tbody>
                  {s.segment_routing.node_sids!.map((x) => (
                    <tr key={`${x.prefix}/${x.algorithm ?? 0}`}>
                      <td className="mono">{x.prefix}</td>
                      <td className="num mono">{x.index}</td>
                      <td className="num mono">{x.label}</td>
                      <td className="num mono">{x.algorithm ?? 0}</td>
                      <td className="mono">{x.router ?? "us"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {(s.segment_routing.adj_sids?.length ?? 0) > 0 && (
            <div className="muted small-note">
              Adjacency SIDs:{" "}
              {s.segment_routing
                .adj_sids!.map(
                  (x) =>
                    `${x.label} → ${x.neighbor} (${x.interface}${x.lan ? ", LAN" : ""})`,
                )
                .join(" · ")}
            </div>
          )}
        </>
      )}

      {s.topology && (
        <>
          <h4>Emulated topology</h4>
          <div className="flow-path mono small-note">
            {s.topology.kind} · {s.topology.nodes} routers · {s.topology.links}{" "}
            links · {s.topology.prefixes} prefixes · attached to us:{" "}
            {s.topology.attached.join(", ") || "none"}
          </div>
          {s.topology.down_nodes?.length ||
          s.topology.down_links?.length ||
          s.topology.overloaded?.length ? (
            <div className="warn-box">
              {s.topology.down_nodes?.length ? (
                <div>routers down: {s.topology.down_nodes.join(", ")}</div>
              ) : null}
              {s.topology.down_links?.length ? (
                <div>links down: {s.topology.down_links.join(", ")}</div>
              ) : null}
              {s.topology.overloaded?.length ? (
                <div>max-metric: {s.topology.overloaded.join(", ")}</div>
              ) : null}
            </div>
          ) : (
            <div className="muted small-note">
              All simulated routers and links are up. Use the topology-link and
              topology-node actions to fail them.
            </div>
          )}
        </>
      )}

      <h4>Link-state database</h4>
      <div className="muted small-note">
        {types || "empty"}
        {s.lsdb_total > s.lsdb.length &&
          ` · showing ${s.lsdb.length} of ${s.lsdb_total}`}
      </div>
      <div className="table-wrap">
        <table className="table dense hover">
          <thead>
            <tr>
              <th>Type</th>
              {multiArea && <th>Area</th>}
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
              const key = `${l.type}/${l.id}/${l.adv_router}/${l.link ?? ""}`;
              const flushed = l.age >= 3600;
              return (
                <Fragment key={key}>
                  <tr
                    onClick={() => setOpen(open === key ? undefined : key)}
                    className={flushed ? "muted" : ""}
                  >
                    <td>
                      {l.type}
                      {l.link && (
                        <span
                          className="tag"
                          title="Link scope: flooded only on this interface"
                        >
                          {l.link}
                        </span>
                      )}
                    </td>
                    {multiArea && <td className="mono">{l.area ?? "AS"}</td>}
                    <td className="mono">
                      {open === key ? "▾ " : "▸ "}
                      {l.id}
                    </td>
                    <td className="mono">
                      {l.adv_router}
                      {l.own && <span className="tag">ours</span>}
                    </td>
                    <td className="num mono">{l.seq}</td>
                    <td className="num mono">
                      {flushed ? "MaxAge" : `${l.age} s`}
                    </td>
                    <td className="mono">{l.checksum}</td>
                    <td className="mono">{l.summary}</td>
                  </tr>
                  {open === key && (
                    <tr className="lsp-detail">
                      <td colSpan={multiArea ? 8 : 7}>
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
            <span
              className={
                /error|mismatch|failure/.test(k) && v
                  ? "counter-v warn-text"
                  : "counter-v"
              }
            >
              {v}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

registerView("ospf", { summary, Status });
registerView("ospfv3", { summary, Status });
