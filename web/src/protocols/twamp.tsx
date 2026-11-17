import type { JsonObject } from "@bufbuild/protobuf";
import { registerView, type Health } from "./registry";

type Session = {
  sender: string;
  local: string;
  ssid: number;
  kind: "stamp" | "twamp-light";
  timestamps: string;
  state: "active" | "idle";
  last_ago_s: number;
  rx: number;
  tx: number;
  dropped: number;
  burst_remaining?: number;
  gaps: number;
  reordered: number;
  duplicates: number;
  last_seq: number;
  ttl: number;
  dscp: number;
  dwell_us: number;
  tlvs?: Record<string, number>;
  tlv_length_with_header?: boolean;
};
type ReflectorStatus = {
  mode: string;
  ports: number[];
  muted: boolean;
  faults?: string[];
  active: number;
  rx: number;
  tx: number;
  dropped: number;
  refused: number;
  errors: number;
  pending: number;
  sessions: Session[];
};

const as = (s: JsonObject) => s as unknown as ReflectorStatus;
const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
const kind = (s: Session) => (s.kind === "stamp" ? `STAMP SSID ${s.ssid}` : "TWAMP Light");

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const faults = s.faults?.length ? ` · ${s.faults.join(", ")}` : "";
  if (s.muted) return { health: "warn", text: `muted · ${plural(s.active, "session")}${faults}` };
  if (!s.active) return { health: "idle", text: `no sessions · port ${s.ports.join(", ")}${faults}` };
  const senders = [...new Set(s.sessions.filter((x) => x.state === "active").map((x) => x.sender.replace(/:\d+$/, "")))];
  return { health: faults ? "warn" : "ok", text: `${plural(s.active, "session")} from ${senders.join(", ")}${faults}` };
}

function Status({ status }: { status: JsonObject }) {
  const s = as(status);
  return (
    <div>
      <dl className="kv">
        <div className="kv-row">
          <dt>mode · ports</dt>
          <dd className="mono">
            {s.mode === "stamp" ? "STAMP" : "TWAMP Light"} · {s.ports.join(", ")}
          </dd>
        </div>
        <div className="kv-row">
          <dt>replies</dt>
          <dd className="mono">
            {s.muted ? <span className="warn-text">muted</span> : "answering"}
            {s.faults?.length ? <span className="warn-text"> · {s.faults.join(" · ")}</span> : null}
          </dd>
        </div>
        <div className="kv-row kv-wide">
          <dt>packets</dt>
          <dd className="mono">
            {s.rx} received · {s.tx} replied · {s.dropped} dropped
            {s.pending > 0 && ` · ${s.pending} held`}
            {s.refused > 0 && <span className="warn-text"> · {s.refused} refused (sender not allowed)</span>}
            {s.errors > 0 && <span className="warn-text"> · {s.errors} errors</span>}
          </dd>
        </div>
      </dl>
      <h4>Sessions</h4>
      {s.sessions.length === 0 ? (
        <div className="muted">No test packets received yet.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>Sender</th>
                <th>Kind</th>
                <th className="num">Received</th>
                <th className="num">Replied</th>
                <th className="num">Dropped</th>
                <th className="num">Sender gaps</th>
                <th className="num">TTL · DSCP</th>
                <th className="num">Dwell</th>
                <th>Last</th>
                <th>TLVs</th>
              </tr>
            </thead>
            <tbody>
              {s.sessions.map((x) => (
                <tr key={`${x.sender}/${x.local}/${x.ssid}`} className={x.state === "idle" ? "stale" : undefined}>
                  <td className="mono">
                    {x.sender}
                    <div className="muted small-note">to {x.local}</div>
                  </td>
                  <td className="mono">
                    {kind(x)}
                    <div className="muted small-note">{x.timestamps.toUpperCase()} timestamps</div>
                  </td>
                  <td className="num mono">{x.rx}</td>
                  <td className="num mono">{x.tx}</td>
                  <td className="num mono">
                    {x.dropped}
                    {x.burst_remaining ? <div className="warn-text small-note">{x.burst_remaining} more to drop</div> : null}
                  </td>
                  <td className="num mono" title="Missing sender sequence numbers (forward loss), and packets out of order or repeated">
                    {x.gaps}
                    {(x.reordered > 0 || x.duplicates > 0) && (
                      <div className="muted small-note">
                        {x.reordered} reordered · {x.duplicates} dup
                      </div>
                    )}
                  </td>
                  <td className="num mono">
                    {x.ttl} · {x.dscp}
                  </td>
                  <td className="num mono">{x.dwell_us.toFixed(0)} µs</td>
                  <td className="mono">
                    {x.state === "idle" ? "idle, " : ""}
                    {x.last_ago_s.toFixed(1)} s ago
                    <div className="muted small-note">seq {x.last_seq}</div>
                  </td>
                  <td className="mono small-note">
                    {x.tlvs ? Object.keys(x.tlvs).sort().join(", ") : "—"}
                    {x.tlv_length_with_header && <div className="muted">length counts the TLV header</div>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

// Every change applies in place (protocols/twamp/twamp.go Reconfigure).
registerView("twamp-reflector", {
  summary,
  Status,
  applyNote: () => ({ restart: false, text: "Applies in place: sessions and counters stay; faults affect the next replies." }),
});
