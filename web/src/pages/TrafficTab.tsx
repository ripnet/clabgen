import { useState } from "react";
import { client, usePoll } from "../api";
import { asFlow, dur, flowSummary, ms } from "../components/Flow";
import { href, navigate } from "../router";
import { Empty, ErrorBox, HealthText, useBusy } from "../ui";
import { CreateProtocol } from "./ProtocolsTab";
import { ReceivedTable, useClearFlows, useFlowIndex, useReceived } from "../components/FlowViews";

// TrafficTab lists the agent's traffic flows (types in the "traffic"
// category) with their live statistics.
export function TrafficTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const list = usePoll(() => c.listProtocols({ withStatus: true }), [agentId], 1000);
  const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
  const devices = usePoll(() => c.listDevices({}), [agentId], 5000);
  // Before a flow first runs it hasn't picked a source: show the device's
  // first address of the destination's family.
  const guessSource = (device: string, dst?: string) => {
    const v6 = !!dst?.includes(":");
    const addrs = devices.data?.devices.find((d) => d.name === device)?.addresses ?? [];
    return addrs.map((a) => a.split("/")[0]).find((a) => a.includes(":") === v6 && !a.startsWith("fe80:"));
  };
  const [creating, setCreating] = useState(false);
  const { busy, run } = useBusy();
  const traffic = new Set(types.data?.types.filter((t) => t.category === "traffic").map((t) => t.name));
  // One-way flows: received/lost come from the clabgen receivers.
  const rx = useReceived();
  const oneWay = (id?: string) => {
    const rs = rx.list.filter((r) => r.f.flow_id === id);
    return rs.length ? { rx: rs.reduce((a, r) => a + r.f.rx, 0), lost: rs.reduce((a, r) => a + r.f.lost, 0) } : undefined;
  };
  const flows = (list.data?.protocols ?? []).filter((p) => traffic.has(p.type));
  const running = (p: (typeof flows)[number]) => {
    const st = p.status ? asFlow(p.status).state : "";
    return st === "running" || st === "resolving";
  };
  const clearFlows = useClearFlows();
  const flowRef = (p: (typeof flows)[number]) => ({ name: p.name, flowId: (p.status as { flow_id?: string } | undefined)?.flow_id });
  const all = (action: "start" | "stop") =>
    run(async () => {
      for (const p of flows.filter((p) => (action === "start" ? !running(p) : running(p)))) await c.runProtocolAction({ name: p.name, action });
    }).then(list.refresh);

  return (
    <div className="card">
      <div className="card-head">
        <h2>Traffic</h2>
        <div className="btn-row">
          <button className="btn" disabled={busy || !flows.some((p) => !running(p))} onClick={() => all("start")}>
            ▶ Start all
          </button>
          <button className="btn" disabled={busy || !flows.some(running)} onClick={() => all("stop")}>
            ■ Stop all
          </button>
          <button
            className="btn"
            disabled={busy || !flows.length}
            title="Reset every flow's statistics, here and at their receivers"
            onClick={() => run(() => clearFlows(agentId, flows.map(flowRef)), "Statistics cleared").then(list.refresh)}
          >
            ↺ Clear all stats
          </button>
          <button className="btn primary" onClick={() => setCreating(true)}>
            New flow
          </button>
        </div>
      </div>
      <p className="muted">Flows send packets from a device and measure what comes back: loss, outages (consecutive losses) and round-trip time.</p>
      <ErrorBox error={list.error} />
      {!list.loading && flows.length === 0 ? (
        <Empty title="No flows yet">Create one here, or use Ping… on a device with an IP address.</Empty>
      ) : (
        <div className="table-wrap">
          <table className="table hover dense">
            <thead>
              <tr>
                <th>Flow</th>
                <th>Path</th>
                <th>Kind</th>
                <th className="num">Rate</th>
                <th>State</th>
                <th className="num">Sent</th>
                <th className="num">Received</th>
                <th className="num">Lost</th>
                <th className="num">Longest outage</th>
                <th className="num">RTT avg</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {flows.map((p) => {
                const s = p.status ? asFlow(p.status) : undefined;
                const sum = p.state === "running" && p.status ? flowSummary(p.status) : { health: "bad" as const, text: p.error || p.state };
                const on = running(p);
                const ow = s && s.tracks_replies === false ? oneWay((p.status as { flow_id?: string }).flow_id) : undefined;
                const owPct = ow && ow.rx + ow.lost ? (100 * ow.lost) / (ow.rx + ow.lost) : 0;
                return (
                  <tr key={p.name} onClick={() => navigate({ page: "protocol", agent: agentId, protocol: p.name })}>
                    <td className="mono strong">
                      <a href={href({ page: "protocol", agent: agentId, protocol: p.name })} onClick={(e) => e.stopPropagation()}>
                        {p.name}
                      </a>
                    </td>
                    <td className="mono">
                      <span className="muted">{p.device} · </span>
                      {s?.source || <span title="expected source (not started yet)">{guessSource(p.device, s?.destination) ?? "?"}</span>} → {s?.destination ?? "?"}
                    </td>
                    <td className="mono">
                      {p.type}
                      {s?.kind ? ` ${s.kind}` : ""}
                    </td>
                    <td className="num mono">{s ? `${s.rate} pps` : "—"}</td>
                    <td>
                      <HealthText health={sum.health}>{s && sum.health !== "bad" ? s.state : sum.text}</HealthText>
                    </td>
                    <td className="num mono">{s?.tx.toLocaleString() ?? "—"}</td>
                    <td className="num mono" title={ow ? "Received at clabgen receivers (one way)" : undefined}>
                      {s && s.tracks_replies !== false ? s.rx.toLocaleString() : ow ? `${ow.rx.toLocaleString()} ↦` : "—"}
                    </td>
                    <td className={s?.lost || ow?.lost ? "num mono bad-text" : "num mono"}>
                      {s && s.tracks_replies !== false ? `${s.lost.toLocaleString()} (${s.loss_pct.toFixed(2)}%)` : ow ? `${ow.lost.toLocaleString()} (${owPct.toFixed(2)}%)` : "—"}
                    </td>
                    <td className={s?.outage_current_ms ? "num mono bad-text" : "num mono"}>
                      {s?.outage_current_ms ? `now ${dur(s.outage_current_ms)}` : s?.outage_max_ms ? dur(s.outage_max_ms) : "—"}
                    </td>
                    <td className="num mono">{ms(s?.rtt?.avg_us)}</td>
                    <td className="actions">
                      <button
                        className="btn small"
                        disabled={busy || p.state !== "running"}
                        onClick={(e) => {
                          e.stopPropagation();
                          run(() => c.runProtocolAction({ name: p.name, action: on ? "stop" : "start" })).then(list.refresh);
                        }}
                      >
                        {on ? "Stop" : "Start"}
                      </button>
                      <button
                        className="icon-btn row-clear"
                        disabled={busy}
                        title="Clear this flow's statistics"
                        aria-label={`Clear ${p.name} statistics`}
                        onClick={(e) => {
                          e.stopPropagation();
                          run(() => clearFlows(agentId, [flowRef(p)])).then(list.refresh);
                        }}
                      >
                        ↺
                      </button>
                      <button
                        className="btn small danger-ghost"
                        disabled={busy}
                        onClick={(e) => {
                          e.stopPropagation();
                          if (confirm(`Delete flow ${p.name}?`)) run(() => c.deleteProtocol({ name: p.name }), `Deleted ${p.name}`).then(list.refresh);
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {creating && <CreateProtocol agentId={agentId} category="traffic" onClose={() => setCreating(false)} onDone={list.refresh} />}
      <Received agentId={agentId} />
    </div>
  );
}

// Received lists signed flows arriving at this agent's devices.
function Received({ agentId }: { agentId: string }) {
  const { list } = useReceived(agentId);
  const index = useFlowIndex();
  const { busy, run } = useBusy();
  return (
    <>
      <div className="card-head sub-head">
        <h3>Received here</h3>
        {list.length > 0 && (
          <button className="btn small" disabled={busy} onClick={() => run(() => client(agentId).clearReceivedFlows({}), "Cleared")}>
            Clear
          </button>
        )}
      </div>
      <p className="muted">Devices count every signed clabgen packet addressed to them: one-way loss, reordering, latency and outages.</p>
      {list.length ? <ReceivedTable rows={list} index={index} /> : <div className="muted">Nothing received yet.</div>}
    </>
  );
}
