import type { ReactNode } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { agentLabel, useAgents } from "../agents";
import { href } from "../router";
import { useBusy } from "../ui";
import { asFlow, dur, FlowLog, FlowTiles, FlowTimeline, ms, type FlowStatus } from "./Flow";

// One received flow at a clabgen device (internal/flowrx).
export type RxFlow = {
  flow_id: string;
  proto: string;
  src: string;
  dst: string;
  src_port?: number;
  dst_port?: number;
  rx: number;
  rate_pps: number;
  lost: number;
  loss_pct: number;
  reordered: number;
  duplicate: number;
  too_late: number;
  latency_min_us: number;
  latency_avg_us: number;
  latency_max_us: number;
  jitter_us: number;
  outages: number;
  outage_max_ms: number;
  outage_last_ms: number;
  idle_ms: number;
};
export type RxAt = { agentId: string; agent: string; device: string; f: RxFlow };

// useReceived polls received flows on every known agent (or one).
export function useReceived(only?: string): { list: RxAt[]; loading: boolean } {
  const { agents, info } = useAgents();
  const targets = agents.filter((a) => !only || a.id === only);
  const key = targets.map((a) => a.id).join(",");
  const p = usePoll(
    async () => {
      const res = await Promise.allSettled(targets.map((a) => client(a.id).listReceivedFlows({}, { timeoutMs: 3000 })));
      const out: RxAt[] = [];
      res.forEach((r, i) => {
        if (r.status !== "fulfilled") return;
        for (const x of r.value.flows)
          out.push({ agentId: targets[i].id, agent: agentLabel(targets[i], info[targets[i].id]), device: x.device, f: (x.stats ?? {}) as unknown as RxFlow });
      });
      return out;
    },
    [key],
    2000,
  );
  return { list: p.data ?? [], loading: p.loading };
}

// useFlowIndex maps flow IDs to the sending flow (agent + name) across
// agents, so receivers can name what they see.
export function useFlowIndex(): Map<string, { agentId: string; agent: string; name: string }> {
  const { agents, info } = useAgents();
  const key = agents.map((a) => a.id).join(",");
  const p = usePoll(
    async () => {
      const m = new Map<string, { agentId: string; agent: string; name: string }>();
      const res = await Promise.allSettled(agents.map((a) => client(a.id).listProtocols({ withStatus: true }, { timeoutMs: 3000 })));
      res.forEach((r, i) => {
        if (r.status !== "fulfilled") return;
        for (const pr of r.value.protocols) {
          const id = (pr.status as { flow_id?: string } | undefined)?.flow_id;
          if (id) m.set(id, { agentId: agents[i].id, agent: agentLabel(agents[i], info[agents[i].id]), name: pr.name });
        }
      });
      return m;
    },
    [key],
    5000,
  );
  return p.data ?? new Map();
}

// useClearFlows clears flows' statistics and, so one-way numbers stay
// consistent, the same flows' counts at every receiver on every agent.
export function useClearFlows() {
  const { agents } = useAgents();
  return async (agentId: string, flows: { name: string; flowId?: string }[]) => {
    for (const f of flows) await client(agentId).runProtocolAction({ name: f.name, action: "clear" });
    const ids = flows.map((f) => f.flowId).filter((x): x is string => !!x);
    await Promise.allSettled(agents.flatMap((a) => ids.map((flowId) => client(a.id).clearReceivedFlows({ flowId }))));
  };
}

const n = (v: number) => v.toLocaleString();
const ep = (addr: string, port?: number) => (port ? (addr.includes(":") ? `[${addr}]:${port}` : `${addr}:${port}`) : addr);

// ReceivedTable lists received flows; `sender` names each one's source flow.
export function ReceivedTable({ rows, showAgent, index }: { rows: RxAt[]; showAgent?: boolean; index?: Map<string, { agentId: string; agent: string; name: string }> }) {
  return (
    <div className="table-wrap">
      <table className="table dense">
        <thead>
          <tr>
            {showAgent && <th>Receiver</th>}
            {!showAgent && <th>Device</th>}
            {index && <th>Flow</th>}
            <th>From → to</th>
            <th className="num">Received</th>
            <th className="num">Rate</th>
            <th className="num">Lost</th>
            <th className="num">Reord / dup</th>
            <th className="num">Latency avg / max</th>
            <th className="num">Jitter</th>
            <th className="num">Longest outage</th>
            <th className="num">Last packet</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r) => {
            const f = r.f;
            const sender = index?.get(f.flow_id);
            const stale = f.idle_ms > 3000;
            return (
              <tr key={r.agentId + r.device + f.flow_id + f.proto}>
                <td className="mono">{showAgent ? `${r.agent} / ${r.device}` : r.device}</td>
                {index && (
                  <td className="mono">
                    {sender ? (
                      <a href={href({ page: "protocol", agent: sender.agentId, protocol: sender.name })}>
                        {sender.agent}/{sender.name}
                      </a>
                    ) : (
                      <span className="muted">{f.flow_id}</span>
                    )}
                  </td>
                )}
                <td className="mono">
                  {f.proto} {ep(f.src, f.src_port)} → {ep(f.dst, f.dst_port)}
                </td>
                <td className="num mono">{n(f.rx)}</td>
                <td className="num mono">{stale ? "—" : `${f.rate_pps.toFixed(0)} pps`}</td>
                <td className={f.lost ? "num mono bad-text" : "num mono"}>
                  {n(f.lost)} ({f.loss_pct.toFixed(2)}%)
                </td>
                <td className={f.reordered || f.duplicate ? "num mono warn-text" : "num mono"}>
                  {n(f.reordered)} / {n(f.duplicate)}
                </td>
                <td className="num mono">
                  {ms(f.latency_avg_us)} / {ms(f.latency_max_us)}
                </td>
                <td className="num mono">{ms(f.jitter_us)}</td>
                <td className={f.outage_max_ms ? "num mono warn-text" : "num mono"}>{f.outage_max_ms ? `${dur(f.outage_max_ms)} (${f.outages})` : "—"}</td>
                <td className="num mono">{f.idle_ms < 1000 ? "now" : `${dur(f.idle_ms)} ago`}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// Legs splits a round trip into its two directions when the far end is a
// clabgen device: the outbound leg is what the destination received; the
// return leg is the rest. One-way latencies are valid because the agents
// share the host clock.
function Legs({ s, fwd }: { s: FlowStatus; fwd: RxAt }) {
  const f = fwd.f;
  const retSent = f.rx; // the far end answers what it receives
  // Settled losses on both sides, so the two polls being a moment apart
  // (packets in flight) don't show up as return loss.
  const retLost = Math.max(0, s.lost - f.lost);
  const pct = (lost: number, total: number) => (total ? ((100 * lost) / total).toFixed(2) : "0.00");
  const retLat = s.rtt ? Math.max(0, s.rtt.avg_us - f.latency_avg_us) : undefined;
  return (
    <>
      <h4>Legs</h4>
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>Direction</th>
              <th className="num">Sent</th>
              <th className="num">Received</th>
              <th className="num">Lost</th>
              <th className="num">Longest outage</th>
              <th className="num">Latency avg</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td className="mono">
                outbound → {fwd.agent}/{fwd.device}
              </td>
              <td className="num mono">{n(s.tx)}</td>
              <td className="num mono">{n(f.rx)}</td>
              <td className={f.lost ? "num mono bad-text" : "num mono"}>
                {n(f.lost)} ({pct(f.lost, f.rx + f.lost)}%)
              </td>
              <td className="num mono">{f.outage_max_ms ? dur(f.outage_max_ms) : "—"}</td>
              <td className="num mono">{ms(f.latency_avg_us)}</td>
            </tr>
            <tr>
              <td className="mono">return ←</td>
              <td className="num mono">{n(retSent)}</td>
              <td className="num mono">{n(s.rx)}</td>
              <td className={retLost ? "num mono bad-text" : "num mono"}>
                {n(retLost)} ({pct(retLost, retSent)}%)
              </td>
              <td className="num mono muted" title="Only the round trip's outages are measured">
                —
              </td>
              <td className="num mono" title="Round-trip time minus the outbound latency">
                {ms(retLat)}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div className="muted small-note">
        Outbound numbers come from the destination's receive stats; return loss is the round trip's loss minus the outbound loss. Sent and received are read a
        moment apart, so they can differ by the packets in flight.
      </div>
    </>
  );
}

// Receivers shows every clabgen device that received this flow.
function Receivers({ flowId, s }: { flowId: string; s: FlowStatus }) {
  const { list, loading } = useReceived();
  const rows = list.filter((r) => r.f.flow_id === flowId);
  const fwd = s.tracks_replies !== false ? rows.find((r) => r.f.dst === s.destination) : undefined;
  return (
    <>
      {fwd && <Legs s={s} fwd={fwd} />}
      <h4>Receivers (one way)</h4>
      {rows.length ? (
        <ReceivedTable rows={rows} showAgent />
      ) : (
        <div className="muted">{loading ? "Looking…" : "No clabgen device has received this flow (receivers count signed packets automatically)."}</div>
      )}
    </>
  );
}

// IpFlowView is the status panel for IP flows (icmp, udp, …).
export function IpFlowView({ status, agentId, name, extra }: { status: JsonObject; agentId?: string; name?: string; extra?: ReactNode }) {
  const s = asFlow(status);
  const { busy, run } = useBusy();
  const clearFlows = useClearFlows();
  const flowId = (status as { flow_id?: string }).flow_id;
  const act = (action: string) =>
    agentId &&
    name &&
    run((): Promise<unknown> => (action === "clear" ? clearFlows(agentId, [{ name, flowId }]) : client(agentId).runProtocolAction({ name, action })));
  const running = s.state === "running" || s.state === "resolving";
  const st = status as { last_error?: string; last_error_age_s?: number };
  return (
    <div className="flow">
      <div className="flow-head">
        <div className="flow-path mono">
          <span className={`pill ${running ? "pill-on" : ""}`}>{s.state}</span> {s.source || "?"} → <b>{s.destination}</b>
          {s.next_hop && s.next_hop !== s.destination && <span className="muted"> via {s.next_hop}</span>}
          {s.dst_mac && <span className="muted"> ({s.dst_mac})</span>}
        </div>
        <div className="muted">
          {s.kind} · {s.rate} pps · {s.mode}
          {s.elapsed_s ? ` · ${s.elapsed_s.toFixed(0)} s` : ""}
          {extra}
        </div>
        {agentId && name && (
          <div className="btn-row">
            {running ? (
              <button className="btn" disabled={busy} onClick={() => act("stop")}>
                ■ Stop
              </button>
            ) : (
              <button className="btn primary" disabled={busy} onClick={() => act("start")}>
                ▶ Start
              </button>
            )}
            <button className="btn" disabled={busy} onClick={() => act("clear")} title="Reset this flow's statistics, here and at its receivers">
              Clear stats
            </button>
          </div>
        )}
      </div>
      {s.error && <div className="warn-box">{s.error}</div>}
      {!s.error && st.last_error && (
        <div className="muted small-note">
          last error {st.last_error_age_s?.toFixed(0)} s ago: {st.last_error}
        </div>
      )}
      <FlowTiles s={s} />
      {s.tracks_replies !== false && <FlowTimeline s={s} />}
      {flowId && <Receivers flowId={flowId} s={s} />}
      <h4>Packets</h4>
      <FlowLog s={s} />
    </div>
  );
}
