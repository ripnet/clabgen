import type { JsonObject } from "@bufbuild/protobuf";
import { registerView, restartNote, type Health } from "./registry";

type Info = {
  chassis_id_subtype: string;
  chassis_id: string;
  port_id_subtype: string;
  port_id: string;
  ttl: number;
  port_description?: string;
  system_name?: string;
  system_description?: string;
  capabilities?: string[];
  enabled_capabilities?: string[];
  management_addresses?: string[];
  port_vlan_id?: number;
  vlan_names?: { vid: number; name: string }[];
  max_frame_size?: number;
  mac_phy?: { autoneg_supported: boolean; autoneg_enabled: boolean; advertised_capabilities: number; mau_type: number };
  link_aggregation?: { capable: boolean; enabled: boolean; aggregated_port_id: number };
  other_tlvs?: { type: number; oui?: string; subtype?: number; value: string }[];
};
type Neighbor = Info & { mac: string; ttl_remaining_s: number; since: string; last_rx: string; changes: number };
type LLDPStatus = {
  mode: string;
  dest: string;
  transmit: string;
  next_tx_s?: number;
  paused_until?: string;
  local?: Info;
  neighbors: Neighbor[];
  counters: Record<string, number>;
};

const as = (s: JsonObject) => s as unknown as LLDPStatus;
const who = (n: Info) => n.system_name || n.chassis_id;

function extras(n: Info): string[] {
  const out: string[] = [];
  if (n.capabilities?.length) out.push(`caps ${n.capabilities.map((c) => (n.enabled_capabilities?.includes(c) ? c : `(${c})`)).join(",")}`);
  if (n.management_addresses?.length) out.push(`mgmt ${n.management_addresses.join(", ")}`);
  if (n.port_vlan_id) out.push(`PVID ${n.port_vlan_id}`);
  if (n.vlan_names?.length) out.push(`VLANs ${n.vlan_names.map((v) => `${v.vid}:${v.name}`).join(", ")}`);
  if (n.max_frame_size) out.push(`max frame ${n.max_frame_size}`);
  if (n.mac_phy) out.push(`MAU ${n.mac_phy.mau_type}${n.mac_phy.autoneg_enabled ? " autoneg" : ""}`);
  if (n.link_aggregation) out.push(`LAG ${n.link_aggregation.enabled ? "member" : n.link_aggregation.capable ? "capable" : "no"}${n.link_aggregation.aggregated_port_id ? ` #${n.link_aggregation.aggregated_port_id}` : ""}`);
  if (n.other_tlvs?.length) out.push(`${n.other_tlvs.length} other TLV${n.other_tlvs.length > 1 ? "s" : ""}`);
  return out;
}

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const ns = s.neighbors ?? [];
  const tx = s.transmit === "on" ? "" : ` · tx ${s.transmit}`;
  if (!ns.length) return { health: s.mode === "tx-only" || s.mode === "disabled" ? "idle" : "warn", text: `no neighbors${tx}` };
  return {
    health: s.transmit === "on" || s.mode === "rx-only" ? "ok" : "warn",
    text: ns.map((n) => `${who(n)} ${n.port_id}`).join(", ") + tx,
  };
}

function Status({ status }: { status: JsonObject }) {
  const s = as(status);
  const l = s.local;
  const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) => x.localeCompare(y));
  return (
    <div>
      <dl className="kv">
        <div className="kv-row">
          <dt>mode · scope</dt>
          <dd className="mono">
            {s.mode} · {s.dest}
          </dd>
        </div>
        <div className="kv-row">
          <dt>transmit</dt>
          <dd className="mono">
            {s.transmit === "on" ? `on · next in ${(s.next_tx_s ?? 0).toFixed(0)} s` : <span className="warn-text">{s.transmit}</span>}
            {s.paused_until && ` until ${new Date(s.paused_until).toLocaleTimeString()}`}
          </dd>
        </div>
        {l && (
          <div className="kv-row kv-wide">
            <dt>we advertise</dt>
            <dd className="mono">
              {l.system_name} · chassis {l.chassis_id_subtype} {l.chassis_id} · port {l.port_id_subtype} {l.port_id} · TTL {l.ttl}
              {extras(l).length > 0 && <div className="muted">{extras(l).join(" · ")}</div>}
            </dd>
          </div>
        )}
      </dl>
      <h4>Neighbors</h4>
      {(s.neighbors ?? []).length === 0 ? (
        <div className="muted">None heard.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>System</th>
                <th>Chassis ID</th>
                <th>Port</th>
                <th className="num">TTL</th>
                <th>Details</th>
              </tr>
            </thead>
            <tbody>
              {s.neighbors.map((n) => (
                <tr key={`${n.chassis_id}/${n.port_id}`}>
                  <td className="cell-wrap">
                    {n.system_name || "—"}
                    {n.system_description && <div className="muted small-note">{n.system_description}</div>}
                  </td>
                  <td className="mono">
                    {n.chassis_id}
                    <div className="muted small-note">{n.chassis_id_subtype}</div>
                  </td>
                  <td className="mono">
                    {n.port_id}
                    <div className="muted small-note">
                      {n.port_id_subtype}
                      {n.port_description ? ` · ${n.port_description}` : ""}
                    </div>
                  </td>
                  <td className="num mono">
                    {Math.max(0, n.ttl_remaining_s).toFixed(0)}/{n.ttl} s
                  </td>
                  <td className="mono small-note">
                    {extras(n).join(" · ") || "—"}
                    {n.changes > 0 && <div className="muted">{n.changes} change{n.changes > 1 ? "s" : ""} · from {n.mac}</div>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {counters.length > 0 && (
        <div className="muted mono small-note">
          {counters.map(([k, v]) => `${k} ${v}`).join(" · ")}
        </div>
      )}
    </div>
  );
}

// Only a new destination or mode restarts (protocols/lldp/lldp.go).
registerView("lldp", {
  summary,
  Status,
  applyNote: (was, now) =>
    was.dest === now.dest && was.mode === now.mode ? { restart: false, text: "Applies in place: the next LLDPDU, sent at once, carries it; neighbors keep us." } : restartNote,
});
