import { useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import type { Summary } from "../protocols/registry";

// Shared pieces for traffic flows. Every flow type reports the same
// statistics shape (internal/flowstats), so these work for all of them.

export type Bucket = { t: number; tx: number; rx: number; lost: number; rtt_avg_us?: number; rtt_max_us?: number };
export type Line = { seq: number; time: string; result: string; rtt_us?: number; detail?: string };
export type FlowStatus = {
  state: string;
  error?: string;
  destination?: string;
  source?: string;
  next_hop?: string;
  dst_mac?: string;
  kind?: string;
  rate: number;
  mode?: string;
  tracks_replies?: boolean;
  elapsed_s?: number;
  tx: number;
  tx_errors: number;
  rx: number;
  lost: number;
  late: number;
  duplicate: number;
  errors: number;
  pending: number;
  loss_pct: number;
  rtt?: { min_us: number; avg_us: number; max_us: number; jitter_us: number; last_us: number };
  outages: number;
  outage_current_ms: number;
  outage_last_ms: number;
  outage_max_ms: number;
  outage_total_ms: number;
  recent: Line[];
  timeline: Bucket[];
  verdict?: Verdict;
};

// Verdict is a flow's pass/fail checks (with checks configured).
export type CheckResult = { check: string; limit: number; value: number; pass: boolean; unavailable?: boolean };
export type Verdict = { result: "pass" | "fail" | "pending"; checks: CheckResult[]; failed?: string };

const checkNames: Record<string, string> = {
  loss_pct: "loss",
  outage_ms: "longest outage",
  rtt_avg_ms: "RTT average",
  rtt_ms: "RTT maximum",
  jitter_ms: "jitter",
};
const checkUnit = (c: string) => (c === "loss_pct" ? "%" : " ms");
const fmtCheck = (c: string, v: number) => `${v < 10 ? v.toFixed(2).replace(/\.?0+$/, "") : Math.round(v)}${checkUnit(c)}`;

// VerdictBadge is a flow's checks result in a word.
export function VerdictBadge({ v }: { v?: Verdict }) {
  if (!v) return <span className="muted">—</span>;
  const cls = v.result === "pass" ? "verdict pass" : v.result === "fail" ? "verdict fail" : "verdict pending";
  const title = v.failed ? `failed: ${v.failed}` : v.result === "pending" ? "nothing measured yet" : v.checks.map((c) => `${checkNames[c.check] ?? c.check} ≤ ${fmtCheck(c.check, c.limit)}`).join(", ");
  return (
    <span className={cls} title={title}>
      {v.result}
    </span>
  );
}

// VerdictPanel lists a flow's checks with what it measured.
export function VerdictPanel({ v }: { v: Verdict }) {
  return (
    <div className="verdict-panel">
      <h4>
        Checks <VerdictBadge v={v} />
      </h4>
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>Check</th>
              <th className="num">Limit</th>
              <th className="num">Measured</th>
              <th>Result</th>
            </tr>
          </thead>
          <tbody>
            {v.checks.map((c) => (
              <tr key={c.check}>
                <td>{checkNames[c.check] ?? c.check}</td>
                <td className="num mono">≤ {fmtCheck(c.check, c.limit)}</td>
                <td className="num mono">{c.unavailable ? "—" : fmtCheck(c.check, c.value)}</td>
                <td>
                  {c.unavailable ? (
                    <span className="muted" title="One-way flows are measured at their receivers">not measured here</span>
                  ) : c.pass ? (
                    <span className="verdict pass">pass</span>
                  ) : (
                    <span className="verdict fail">fail</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

export const asFlow = (s: JsonObject) => s as unknown as FlowStatus;

export const ms = (us?: number) => (us === undefined ? "—" : us < 1000 ? `${(us / 1000).toFixed(3)} ms` : `${(us / 1000).toFixed(2)} ms`);
export const dur = (m: number) => (m < 1000 ? `${Math.round(m)} ms` : `${(m / 1000).toFixed(m < 10000 ? 2 : 1)} s`);
const n = (v: number) => v.toLocaleString();

export function flowSummary(st: JsonObject): Summary {
  const s = asFlow(st);
  // A failed check outranks everything else; it stays until cleared.
  if (s.verdict?.result === "fail") return { health: "bad", text: `checks failed: ${s.verdict.failed}` };
  if (s.state === "resolving") return { health: "warn", text: s.error ? `resolving: ${s.error}` : "resolving" };
  if (s.state !== "running") {
    const tail = s.tx ? ` · ${n(s.rx)}/${n(s.tx)} · ${s.loss_pct.toFixed(1)}% lost` : "";
    return { health: "idle", text: s.state + tail };
  }
  if (s.outage_current_ms > 0) return { health: "bad", text: `outage ${dur(s.outage_current_ms)}` };
  const rtt = s.rtt ? ` · ${ms(s.rtt.avg_us)}` : "";
  if (!s.tracks_replies) return { health: "ok", text: `sending · ${n(s.tx)} tx` };
  const checks = s.verdict?.result === "pass" ? " · checks pass" : "";
  return { health: s.lost || s.errors ? "warn" : "ok", text: `${n(s.rx)}/${n(s.tx)} · ${s.loss_pct.toFixed(2)}% lost${rtt}${checks}` };
}

function Tile({ k, v, tone }: { k: string; v: string; tone?: "bad" | "warn" }) {
  return (
    <div className="stat">
      <div className={tone ? `stat-n ${tone}-text` : "stat-n"}>{v}</div>
      <div className="stat-l">{k}</div>
    </div>
  );
}

export function FlowTiles({ s }: { s: FlowStatus }) {
  return (
    <div className="flow-tiles">
      <Tile k="sent" v={n(s.tx)} />
      {s.tracks_replies !== false && (
        <>
          <Tile k="received" v={n(s.rx)} />
          <Tile k={`lost (${s.loss_pct.toFixed(2)}%)`} v={n(s.lost)} tone={s.lost ? "bad" : undefined} />
          <Tile k="outage now" v={s.outage_current_ms ? dur(s.outage_current_ms) : "—"} tone={s.outage_current_ms ? "bad" : undefined} />
          <Tile k={`longest outage (${s.outages})`} v={s.outage_max_ms ? dur(s.outage_max_ms) : "—"} tone={s.outage_max_ms ? "warn" : undefined} />
          <Tile k="rtt avg" v={ms(s.rtt?.avg_us)} />
          <Tile k="rtt min / max" v={s.rtt ? `${ms(s.rtt.min_us)} / ${ms(s.rtt.max_us)}` : "—"} />
          <Tile k="jitter" v={ms(s.rtt?.jitter_us)} />
        </>
      )}
      {(s.errors > 0 || s.tx_errors > 0 || s.late > 0 || s.duplicate > 0) && (
        <Tile k="errors · tx errors · late · dup" v={`${s.errors} · ${s.tx_errors} · ${s.late} · ${s.duplicate}`} tone="warn" />
      )}
    </div>
  );
}

const W = 720;
const H = 120;
const PAD_L = 44;
const PAD_B = 18;

// Timeline: one stacked bar per second (by send time): received at the
// bottom, lost on top. Lost is red, received the accent blue (a validated
// colour-blind-safe pair); the legend names both.
export function FlowTimeline({ s, seconds = 120 }: { s: FlowStatus; seconds?: number }) {
  const [hover, setHover] = useState<number>();
  const byT = new Map(s.timeline.map((b) => [b.t, b]));
  const last = s.timeline.length ? Math.max(...s.timeline.map((b) => b.t)) : Math.floor(Date.now() / 1000);
  const ts = Array.from({ length: seconds }, (_, i) => last - seconds + 1 + i);
  const max = Math.max(1, ...s.timeline.map((b) => b.tx));
  const bw = (W - PAD_L) / seconds;
  const y = (v: number) => (H - PAD_B) * (1 - v / max);
  const hb = hover !== undefined ? byT.get(ts[hover]) : undefined;
  const replies = s.tracks_replies !== false;
  const rttMax = Math.max(1, ...s.timeline.map((b) => b.rtt_avg_us ?? 0));

  return (
    <div className="flow-chart">
      <div className="chart-head">
        <span className="chart-title">Packets per second, by send time · last {seconds}s</span>
        <span className="legend">
          <span className="sw sw-rx" /> {replies ? "received" : "sent"}
          {replies && (
            <>
              <span className="sw sw-lost" /> lost
            </>
          )}
        </span>
      </div>
      <div className="chart-wrap" onMouseLeave={() => setHover(undefined)}>
        <svg viewBox={`0 0 ${W} ${H}`} className="chart" role="img" aria-label="Packets per second timeline">
          <line x1={PAD_L} x2={W} y1={H - PAD_B} y2={H - PAD_B} className="axis" />
          <line x1={PAD_L} x2={W} y1={0.5} y2={0.5} className="grid" />
          <text x={PAD_L - 6} y={10} className="tick" textAnchor="end">
            {n(max)}
          </text>
          <text x={PAD_L - 6} y={H - PAD_B} className="tick" textAnchor="end">
            0
          </text>
          {[seconds, seconds / 2, 0].map((ago, i) => (
            <text key={i} x={PAD_L + (seconds - ago) * bw - (i === 2 ? 0 : 0)} y={H - 4} className="tick" textAnchor={i === 0 ? "start" : i === 2 ? "end" : "middle"}>
              {ago ? `-${ago}s` : "now"}
            </text>
          ))}
          {ts.map((t, i) => {
            const b = byT.get(t);
            if (!b) return null;
            const x = PAD_L + i * bw + 0.5;
            const w = Math.max(1, bw - 2);
            const ok = replies ? b.rx : b.tx;
            const lostTop = y(ok + b.lost);
            return (
              <g key={t}>
                {ok > 0 && <rect x={x} y={y(ok)} width={w} height={H - PAD_B - y(ok)} rx={Math.min(2, w / 2)} className="bar-rx" />}
                {replies && b.lost > 0 && (
                  <rect x={x} y={lostTop} width={w} height={Math.max(1, y(ok) - lostTop - (ok > 0 ? 2 : 0))} rx={Math.min(2, w / 2)} className="bar-lost" />
                )}
              </g>
            );
          })}
          {hover !== undefined && <rect x={PAD_L + hover * bw} y={0} width={bw} height={H - PAD_B} className="hover-col" />}
          {ts.map((t, i) => (
            <rect key={"h" + t} x={PAD_L + i * bw} y={0} width={bw} height={H} fill="transparent" onMouseEnter={() => setHover(i)} />
          ))}
        </svg>
        {hover !== undefined && (
          <div className="chart-tip" style={{ left: `${Math.min(80, Math.max(0, ((PAD_L + hover * bw) / W) * 100))}%` }}>
            <div className="mono">{new Date(ts[hover] * 1000).toLocaleTimeString()}</div>
            {hb ? (
              <>
                <div>
                  sent <b>{n(hb.tx)}</b>
                  {replies && (
                    <>
                      {" "}
                      · received <b>{n(hb.rx)}</b> · lost <b>{n(hb.lost)}</b>
                    </>
                  )}
                </div>
                {hb.rtt_avg_us !== undefined && (
                  <div>
                    rtt avg {ms(hb.rtt_avg_us)} · max {ms(hb.rtt_max_us)}
                  </div>
                )}
              </>
            ) : (
              <div className="muted">nothing sent</div>
            )}
          </div>
        )}
      </div>
      {replies && s.rtt && (
        <>
          <div className="chart-head">
            <span className="chart-title">Round-trip time, per second (avg)</span>
          </div>
          <svg viewBox={`0 0 ${W} 60`} className="chart" role="img" aria-label="Round-trip time">
            <line x1={PAD_L} x2={W} y1={59.5} y2={59.5} className="axis" />
            <text x={PAD_L - 6} y={10} className="tick" textAnchor="end">
              {ms(rttMax)}
            </text>
            <text x={PAD_L - 6} y={58} className="tick" textAnchor="end">
              0
            </text>
            <line x1={PAD_L} x2={W} y1={6.5} y2={6.5} className="grid" />
            <polyline
              className="rtt-line"
              points={ts
                .map((t, i) => {
                  const b = byT.get(t);
                  return b?.rtt_avg_us !== undefined ? `${PAD_L + i * bw + bw / 2},${59 - (52 * b.rtt_avg_us) / rttMax}` : null;
                })
                .filter(Boolean)
                .join(" ")}
            />
          </svg>
        </>
      )}
    </div>
  );
}

// FlowLog shows per-packet results like ping output, newest last.
export function FlowLog({ s }: { s: FlowStatus }) {
  if (!s.recent.length) return <div className="muted">No packets yet.</div>;
  return (
    <pre className="flow-log mono">
      {s.recent.map((l, i) => (
        <div key={i} className={l.result === "reply" ? "" : l.result === "timeout" || l.result === "error" || l.result === "tx-error" ? "bad-text" : "warn-text"}>
          {l.time} seq={l.seq} {l.result === "reply" ? `${l.detail ? l.detail + " " : ""}time=${ms(l.rtt_us)}` : `${l.result}${l.detail ? " " + l.detail : ""}`}
        </div>
      ))}
    </pre>
  );
}
