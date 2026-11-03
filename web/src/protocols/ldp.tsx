import { useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, errorText } from "../api";
import { ErrorBox, HealthText } from "../ui";
import { registerView, type Health } from "./registry";

type Adjacency = { kind: string; source: string; hold_remaining_s: number; since: string };
type Peer = {
  lsr: string;
  transport: string;
  state: string;
  role: string;
  adjacencies: Adjacency[];
  keepalive_s?: number;
  up_since?: string;
  addresses?: string[];
  received: number;
  last_error?: string;
  sessions: number;
  counters?: Record<string, number>;
};
type LDPStatus = { router_id: string; transport: string; advertised: number; peers: Peer[]; counters: Record<string, number>; silent_remaining_s?: number };

const as = (s: JsonObject) => s as unknown as LDPStatus;

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const up = s.peers.filter((p) => p.state === "operational").length;
  const text = `${s.router_id}: ${up}/${s.peers.length} sessions, ${s.advertised.toLocaleString()} FECs`;
  if (s.silent_remaining_s) return { health: "warn", text: `silent for ${Math.ceil(s.silent_remaining_s)} s; ${text}` };
  if (!s.peers.length) return { health: "idle", text: `${s.router_id}: no neighbors` };
  return { health: up === s.peers.length ? "ok" : up ? "warn" : "bad", text };
}

const stateHealth = (st: string): Health => (st === "operational" ? "ok" : st === "discovered" || st === "flapped" ? "warn" : "bad");

// Bindings loads the labels received from a peer.
function Bindings({ agentId, name, lsr }: { agentId: string; name: string; lsr: string }) {
  const [rows, setRows] = useState<{ prefix: string; label: number }[]>();
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string>();
  const load = async () => {
    setError(undefined);
    try {
      const r = await client(agentId).runProtocolAction({ name, action: "bindings", args: { peer: lsr, limit: 1000 } });
      const v = r.result as unknown as { bindings: { prefix: string; label: number }[]; total: number };
      setRows(v.bindings);
      setTotal(v.total);
    } catch (e) {
      setError(errorText(e));
    }
  };
  return (
    <>
      <button className="btn small" onClick={load}>
        {rows ? "Refresh labels" : "Show labels received"}
      </button>
      <ErrorBox error={error} />
      {rows && (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>Prefix</th>
                <th className="num">Label</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((b) => (
                <tr key={b.prefix}>
                  <td className="mono">{b.prefix}</td>
                  <td className="num mono">{b.label === 3 ? "3 (implicit null)" : b.label === 0 ? "0 (explicit null)" : b.label}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {total > rows.length && <div className="muted">The first {rows.length} of {total.toLocaleString()}.</div>}
        </div>
      )}
    </>
  );
}

function Status({ status, agentId, name }: { status: JsonObject; agentId?: string; name?: string }) {
  const s = as(status);
  return (
    <div>
      <p className="muted">
        LSR ID <span className="mono">{s.router_id}:0</span>, transport address <span className="mono">{s.transport}</span>, {s.advertised.toLocaleString()} FECs advertised.
        {s.silent_remaining_s ? ` Silent for ${Math.ceil(s.silent_remaining_s)} s more.` : ""}
      </p>
      {s.peers.length === 0 ? (
        <div className="muted">No neighbors: no hellos heard yet.</div>
      ) : (
        s.peers.map((p) => (
          <div key={p.lsr} className="card ldp-peer">
            <div className="card-head">
              <h3 className="mono">{p.lsr}</h3>
              <HealthText health={stateHealth(p.state)}>{p.state}</HealthText>
            </div>
            <p className="muted">
              {p.role}, transport {p.transport}
              {p.keepalive_s ? `, keepalive ${p.keepalive_s} s` : ""}, {p.received.toLocaleString()} labels received, session came up {p.sessions} time{p.sessions === 1 ? "" : "s"}
              {p.up_since ? `, up since ${new Date(p.up_since).toLocaleTimeString()}` : ""}
              {p.addresses?.length ? `; addresses ${p.addresses.join(", ")}` : ""}
            </p>
            {p.last_error && <p className="muted small-note">last error: {p.last_error}</p>}
            <div className="table-wrap">
              <table className="table dense">
                <thead>
                  <tr>
                    <th>Adjacency</th>
                    <th>Source</th>
                    <th className="num">Hold left</th>
                  </tr>
                </thead>
                <tbody>
                  {p.adjacencies.map((a) => (
                    <tr key={a.kind + a.source}>
                      <td>{a.kind}</td>
                      <td className="mono">{a.source}</td>
                      <td className="num mono">{a.hold_remaining_s.toFixed(0)} s</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {agentId && name && p.state === "operational" && <Bindings agentId={agentId} name={name} lsr={p.lsr} />}
          </div>
        ))
      )}
    </div>
  );
}

registerView("ldp", { summary, Status });
