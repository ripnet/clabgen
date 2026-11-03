import type { JsonObject } from "@bufbuild/protobuf";
import { HealthText } from "../ui";
import { registerView, type Health } from "./registry";

type RecordedHop = { address: string; label?: number; protection?: string };
type HeadLSP = {
  name: string;
  to: string;
  tunnel_id: number;
  lsp_id?: number;
  state: string;
  label?: number;
  next_hop?: string;
  protection?: string;
  hops?: { address: string; loose?: boolean }[];
  path?: RecordedHop[];
  since?: string;
  last_error?: string;
  locally_repaired?: boolean;
  setups: number;
  resignaling?: boolean;
};
type TailLSP = {
  name: string;
  from: string;
  to: string;
  tunnel_id: number;
  lsp_id: number;
  label: number;
  previous_hop: string;
  style: string;
  protection?: string;
  path?: RecordedHop[];
  expires_in_s: number;
  since: string;
  torn?: boolean;
};
type Neighbor = { address: string; hellos: string; since?: string };
type RSVPStatus = { router_id: string; heads: HeadLSP[]; tails: TailLSP[]; neighbors: Neighbor[]; counters: Record<string, number>; silent_remaining_s?: number };

const as = (s: JsonObject) => s as unknown as RSVPStatus;

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const up = s.heads.filter((h) => h.state === "up").length;
  const text = `${s.router_id}: ${s.heads.length ? `${up}/${s.heads.length} LSPs up, ` : ""}${s.tails.length} ending here`;
  if (s.silent_remaining_s) return { health: "warn", text: `silent for ${Math.ceil(s.silent_remaining_s)} s; ${text}` };
  if (!s.heads.length) return { health: s.tails.length ? "ok" : "idle", text };
  return { health: up === s.heads.length ? "ok" : up ? "warn" : "bad", text };
}

const labelText = (l?: number) => (l === undefined ? "" : l === 3 ? "3 (implicit null)" : l === 0 ? "0 (explicit null)" : String(l));

const stateHealth = (st: string): Health => (st === "up" ? "ok" : st === "signaling" ? "warn" : "idle");

// Path shows the hops an LSP recorded, with labels and local protection.
function Path({ hops }: { hops?: RecordedHop[] }) {
  if (!hops?.length) return <span className="muted">not recorded</span>;
  return (
    <span className="mono rsvp-path">
      {hops.map((h, i) => (
        <span key={h.address + i}>
          {i > 0 && " → "}
          {h.address}
          {h.label !== undefined && <span className="muted"> [{h.label}]</span>}
          {h.protection && <span className="muted"> ({h.protection})</span>}
        </span>
      ))}
    </span>
  );
}

function Status({ status }: { status: JsonObject }) {
  const s = as(status);
  return (
    <div>
      <p className="muted">
        Router ID <span className="mono">{s.router_id}</span>.{s.silent_remaining_s ? ` Silent for ${Math.ceil(s.silent_remaining_s)} s more.` : ""}
      </p>
      {s.heads.length > 0 && <h3>LSPs from here</h3>}
      {s.heads.map((h) => (
        <div key={h.name} className="card rsvp-lsp">
          <div className="card-head">
            <h3 className="mono">{h.name}</h3>
            <HealthText health={stateHealth(h.state)}>{h.state}</HealthText>
          </div>
          <p className="muted">
            to <span className="mono">{h.to}</span>, tunnel {h.tunnel_id}
            {h.lsp_id ? `, LSP ID ${h.lsp_id}` : ""}
            {h.label !== undefined ? `, label ${labelText(h.label)}` : ""}
            {h.next_hop ? `, next hop ${h.next_hop}` : ""}
            {h.protection && h.protection !== "none" ? `, ${h.protection} protection asked` : ""}, set up {h.setups} time{h.setups === 1 ? "" : "s"}
            {h.since ? `, ${h.state} since ${new Date(h.since).toLocaleTimeString()}` : ""}
          </p>
          {h.hops?.length ? (
            <p className="muted">
              Explicit route: <span className="mono">{h.hops.map((x) => x.address + (x.loose ? " (loose)" : "")).join(" → ")}</span>
            </p>
          ) : null}
          {h.state === "up" && (
            <p className="muted">
              Recorded: <Path hops={h.path} />
            </p>
          )}
          {h.resignaling && <p className="muted small-note">Signalling a new path; traffic stays on this one until it's up.</p>}
          {h.locally_repaired && <p className="warn-box">A router on the way has repaired this LSP locally: traffic is on a bypass.</p>}
          {h.last_error && <p className="muted small-note">last error: {h.last_error}</p>}
        </div>
      ))}
      {s.tails.length > 0 && (
        <>
          <h3>LSPs ending here</h3>
          <div className="table-wrap">
            <table className="table dense">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>From</th>
                  <th className="num">Tunnel</th>
                  <th className="num">LSP ID</th>
                  <th className="num">Label</th>
                  <th>Previous hop</th>
                  <th>Protection</th>
                  <th>Path</th>
                  <th className="num">Expires in</th>
                </tr>
              </thead>
              <tbody>
                {s.tails.map((t) => (
                  <tr key={`${t.from}/${t.tunnel_id}/${t.lsp_id}`}>
                    <td className="mono">{t.name || "-"}</td>
                    <td className="mono">{t.from}</td>
                    <td className="num mono">{t.tunnel_id}</td>
                    <td className="num mono">{t.lsp_id}</td>
                    <td className="num mono">{t.torn ? "torn down" : labelText(t.label)}</td>
                    <td className="mono">{t.previous_hop}</td>
                    <td>{t.protection || "none"}</td>
                    <td>
                      <Path hops={t.path} />
                    </td>
                    <td className="num mono">{Math.max(0, t.expires_in_s).toFixed(0)} s</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
      {s.heads.length === 0 && s.tails.length === 0 && <div className="muted">No LSPs: none listed here, and none signalled to this router yet.</div>}
      {s.neighbors.length > 0 && (
        <>
          <h3>Neighbors</h3>
          <div className="table-wrap">
            <table className="table dense">
              <thead>
                <tr>
                  <th>Address</th>
                  <th>Hellos</th>
                </tr>
              </thead>
              <tbody>
                {s.neighbors.map((n) => (
                  <tr key={n.address}>
                    <td className="mono">{n.address}</td>
                    <td>
                      <HealthText health={n.hellos === "up" ? "ok" : "idle"}>{n.hellos}</HealthText>
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

registerView("rsvp", { summary, Status });
