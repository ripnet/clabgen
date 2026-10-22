import { useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { registerView, type Health } from "./registry";

type Peer = {
  address: string;
  remote_as: number;
  description?: string;
  type: string;
  state: string;
  since: string;
  remote_id?: string;
  local_address?: string;
  hold_time?: number;
  families: string[];
  capabilities?: string[];
  received: Record<string, number>;
  advertised: Record<string, number>;
  end_of_rib_received?: string[];
  last_error?: string;
  last_down?: string;
  last_down_at?: string;
  ups: number;
  downs: number;
  flapping?: boolean;
  counters: Record<string, number>;
};
type BGPStatus = {
  local_as: number;
  router_id: string;
  advertised: Record<string, number>;
  peers: Peer[];
  counters: Record<string, number>;
};
type RIBRoute = {
  prefix: string;
  family: string;
  next_hop?: string;
  as_path: string;
  origin?: string;
  med?: number;
  local_pref?: number;
  communities?: string[];
  large_communities?: string[];
  ext_communities?: string[];
  other_attributes?: string[];
};

const as = (s: JsonObject) => s as unknown as BGPStatus;
const sum = (m?: Record<string, number>) =>
  Object.values(m ?? {}).reduce((a, b) => a + b, 0);
const famShort = (f: string) =>
  ({
    "ipv4-unicast": "v4",
    "ipv6-unicast": "v6",
    "ipv4-vpn": "vpnv4",
    "ipv6-vpn": "vpnv6",
  })[f] ?? f;
const perFam = (m: Record<string, number>, fams: string[]) =>
  fams.map((f) => `${famShort(f)} ${m[f] ?? 0}`).join(" · ");

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const ps = s.peers ?? [];
  const up = ps.filter((p) => p.state === "established").length;
  const rx = ps.reduce((a, p) => a + sum(p.received), 0);
  const text = `AS ${s.local_as} · ${up}/${ps.length} peers up · ${rx} received · ${sum(s.advertised)} advertised`;
  if (!ps.length) return { health: "idle", text };
  return { health: up === ps.length ? "ok" : up ? "warn" : "bad", text };
}

function RIB({
  agentId,
  name,
  peer,
}: {
  agentId?: string;
  name?: string;
  peer: string;
}) {
  const [filter, setFilter] = useState("");
  const d = usePoll(
    async () => {
      if (!agentId || !name) return undefined;
      const r = await client(agentId).runProtocolAction({
        name,
        action: "rib",
        args: { peer, prefix: filter, limit: 200 },
      });
      return r.result as unknown as { routes: RIBRoute[]; total: number };
    },
    [agentId, name, peer, filter],
    3000,
  );
  return (
    <div>
      <input
        className="input search mono"
        placeholder="within a prefix (198.51.100.0/22), or text in VPN / EVPN routes (65100:500, mac:02:00)"
        value={filter}
        onChange={(e) => setFilter(e.target.value.trim())}
      />
      {d.error && <div className="error-text">{d.error}</div>}
      {d.data && (
        <>
          <div className="muted small-note">
            {d.data.total} route{d.data.total === 1 ? "" : "s"}
            {d.data.total > d.data.routes.length
              ? ` (first ${d.data.routes.length})`
              : ""}
          </div>
          <div className="table-wrap">
            <table className="table dense">
              <thead>
                <tr>
                  <th>Prefix</th>
                  <th>Next hop</th>
                  <th>AS path</th>
                  <th>Attributes</th>
                </tr>
              </thead>
              <tbody>
                {d.data.routes.map((r) => (
                  <tr key={`${r.family}/${r.prefix}`}>
                    <td className="mono">{r.prefix}</td>
                    <td className="mono">{r.next_hop}</td>
                    <td className="mono">{r.as_path || "(empty)"}</td>
                    <td className="mono small-note">
                      {[
                        r.origin && r.origin !== "igp" && r.origin,
                        r.med !== undefined && `med ${r.med}`,
                        r.local_pref !== undefined && `lp ${r.local_pref}`,
                        r.communities?.join(" "),
                        r.large_communities?.join(" "),
                        r.ext_communities?.join(" "),
                        r.other_attributes?.join(" "),
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}

function Status({
  status,
  agentId,
  name,
}: {
  status: JsonObject;
  agentId?: string;
  name?: string;
}) {
  const s = as(status);
  const [open, setOpen] = useState<string>();
  return (
    <div>
      <dl className="kv">
        <div className="kv-row">
          <dt>AS · router ID</dt>
          <dd className="mono">
            {s.local_as} · {s.router_id}
          </dd>
        </div>
        <div className="kv-row">
          <dt>advertising</dt>
          <dd className="mono">
            {Object.entries(s.advertised ?? {})
              .map(([f, n]) => `${famShort(f)} ${n}`)
              .join(" · ") || "nothing"}
          </dd>
        </div>
      </dl>
      <h4>Peers</h4>
      <div className="table-wrap">
        <table className="table dense hover">
          <thead>
            <tr>
              <th>Peer</th>
              <th>State</th>
              <th>Received</th>
              <th>Advertised</th>
              <th>Details</th>
            </tr>
          </thead>
          <tbody>
            {(s.peers ?? []).map((p) => (
              <tr
                key={p.address}
                onClick={() =>
                  setOpen(open === p.address ? undefined : p.address)
                }
              >
                <td className="mono">
                  {p.address}
                  <div className="muted small-note">
                    AS {p.remote_as} · {p.type}
                    {p.description ? ` · ${p.description}` : ""}
                  </div>
                </td>
                <td>
                  <span
                    className={
                      p.state === "established"
                        ? "pill pill-on"
                        : p.state === "idle" || p.state === "active"
                          ? "pill"
                          : "pill pill-warn"
                    }
                  >
                    {p.state}
                  </span>
                  {p.flapping && <span className="tag tag-warn">flapping</span>}
                  <div className="muted small-note">
                    since {new Date(p.since).toLocaleTimeString()}
                  </div>
                </td>
                <td className="mono">
                  {perFam(p.received, p.families)}
                  {p.end_of_rib_received?.length ? (
                    <div className="muted small-note">
                      EoR {p.end_of_rib_received.map(famShort).join(", ")}
                    </div>
                  ) : null}
                </td>
                <td className="mono">{perFam(p.advertised, p.families)}</td>
                <td className="mono small-note">
                  {p.state === "established" &&
                    `id ${p.remote_id} · hold ${p.hold_time} s · from ${p.local_address}`}
                  <div className="muted">
                    {p.ups} up · {p.downs} down
                    {p.last_down ? ` · last down: ${p.last_down}` : ""}
                    {p.state !== "established" && p.last_error
                      ? ` · ${p.last_error}`
                      : ""}
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {open && (
        <>
          <h4>Routes from {open}</h4>
          <RIB agentId={agentId} name={name} peer={open} />
        </>
      )}
    </div>
  );
}

registerView("bgp", { summary, Status });
