import { useEffect, useMemo, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, errorText } from "../api";
import { agentLabel, useAgents } from "../agents";
import { asFlow, dur, flowSummary, ms, VerdictBadge } from "../components/Flow";
import { useClearFlows } from "../components/FlowViews";
import { href, navigate } from "../router";
import type { Protocol } from "../gen/clabgen/v1/agent_pb";
import { Empty, ErrorBox, HealthText, useBusy } from "../ui";

type AgentFlows = { flows: Protocol[]; error?: string };
type Row = { agentId: string; agent: string; lab?: string; p: Protocol };

const running = (p: Protocol) => {
  const st = p.status ? asFlow(p.status).state : "";
  return st === "running" || st === "resolving";
};
const verdictOf = (p: Protocol) => (p.status ? asFlow(p.status).verdict?.result : undefined);

// FlowsPage lists every agent's traffic flows with their checks: one place
// to start a test, run it, and see what passed.
export function FlowsPage() {
  const { agents, info } = useAgents();
  const [by, setBy] = useState<Record<string, AgentFlows>>({});
  const [filter, setFilter] = useState("");
  const [failing, setFailing] = useState(false);
  const ids = agents.map((a) => a.id).join("|");
  const { busy, run } = useBusy();
  const clearFlows = useClearFlows();

  // Traffic types per agent (flows are protocols of the "traffic" category).
  const [traffic, setTraffic] = useState<Record<string, Set<string>>>({});
  useEffect(() => {
    for (const a of agents) {
      client(a.id)
        .listProtocolTypes({}, { timeoutMs: 4000 })
        .then((r) => setTraffic((m) => ({ ...m, [a.id]: new Set(r.types.filter((t) => t.category === "traffic").map((t) => t.name)) })))
        .catch(() => {});
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids]);

  const [tick, setTick] = useState(0);
  useEffect(() => {
    let cancelled = false;
    const poll = () => {
      if (document.visibilityState !== "visible") return;
      for (const a of agents) {
        client(a.id)
          .listProtocols({ withStatus: true, brief: true, noGroupMembers: true }, { timeoutMs: 4000 })
          .then(
            (r) => !cancelled && setBy((m) => ({ ...m, [a.id]: { flows: r.protocols } })),
            (e) => !cancelled && setBy((m) => ({ ...m, [a.id]: { flows: m[a.id]?.flows ?? [], error: errorText(e) } })),
          );
      }
    };
    poll();
    const t = setInterval(poll, 1000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids, tick]);

  const all: Row[] = useMemo(
    () =>
      agents.flatMap((a) =>
        (by[a.id]?.flows ?? [])
          .filter((p) => traffic[a.id]?.has(p.type) && p.type !== "udp-echo")
          .map((p) => ({ agentId: a.id, agent: agentLabel(a, info[a.id]), lab: a.lab, p })),
      ),
    [agents, by, traffic, info],
  );
  const f = filter.trim().toLowerCase();
  const rows = all.filter(
    (r) =>
      (!failing || verdictOf(r.p) === "fail") &&
      (!f || [r.agent, r.lab ?? "", r.p.name, r.p.device, r.p.type, asFlow(r.p.status ?? {}).destination ?? ""].some((x) => x.toLowerCase().includes(f))),
  );
  const count = (pred: (r: Row) => boolean) => all.filter(pred).length;
  const stats = {
    flows: all.length,
    running: count((r) => running(r.p)),
    pass: count((r) => verdictOf(r.p) === "pass"),
    fail: count((r) => verdictOf(r.p) === "fail"),
    pending: count((r) => verdictOf(r.p) === "pending"),
  };
  const errors = agents.filter((a) => by[a.id]?.error).map((a) => `${agentLabel(a, info[a.id])}: ${by[a.id]!.error}`);

  // Lab-wide actions apply to the rows shown (the filter narrows them).
  const each = (action: "start" | "stop") =>
    run(async () => {
      const todo = rows.filter((r) => (action === "start" ? !running(r.p) : running(r.p)));
      await Promise.all(todo.map((r) => client(r.agentId).runProtocolAction({ name: r.p.name, action })));
    }).then(() => setTick((t) => t + 1));
  const clearAll = () =>
    run(async () => {
      const byAgent = new Map<string, Row[]>();
      for (const r of rows) byAgent.set(r.agentId, [...(byAgent.get(r.agentId) ?? []), r]);
      for (const [agentId, rs] of byAgent) await clearFlows(agentId, rs.map((r) => ({ name: r.p.name, flowId: (r.p.status as { flow_id?: string } | undefined)?.flow_id })));
    }, "Statistics cleared").then(() => setTick((t) => t + 1));

  return (
    <div className="page">
      <div className="page-head">
        <h1>Flows</h1>
        <div className="stat-row">
          <Stat label="Flows" n={stats.flows} />
          <Stat label="Running" n={stats.running} />
          <Stat label="Pass" n={stats.pass} tone="ok" />
          <Stat label="Fail" n={stats.fail} tone="bad" />
          <Stat label="Pending" n={stats.pending} />
        </div>
      </div>
      <p className="muted">
        Every agent's traffic flows. Give a flow <b>checks</b> (in its config: max loss, outage, RTT, jitter) and it passes or fails on what it
        measures from its start or last clear: start the flows, run the test (flap a peer, fail a link), and see the result here.
      </p>
      {errors.map((e) => (
        <ErrorBox key={e} error={e} />
      ))}
      <div className="card">
        <div className="card-head">
          <div className="btn-row">
            <input className="input" style={{ width: 280 }} placeholder="Filter: agent, lab, flow, device, destination" value={filter} onChange={(e) => setFilter(e.target.value)} />
            <label className="switch small">
              <input type="checkbox" checked={failing} onChange={(e) => setFailing(e.target.checked)} /> <span>failing only</span>
            </label>
          </div>
          <div className="btn-row">
            <button className="btn" disabled={busy || !rows.some((r) => !running(r.p))} onClick={() => each("start")} title="Start the flows listed">
              ▶ Start all
            </button>
            <button className="btn" disabled={busy || !rows.some((r) => running(r.p))} onClick={() => each("stop")} title="Stop the flows listed">
              ■ Stop all
            </button>
            <button className="btn" disabled={busy || !rows.length} onClick={clearAll} title="Reset the listed flows' statistics and checks, here and at their receivers">
              ↺ Clear all stats
            </button>
          </div>
        </div>
        {all.length === 0 ? (
          <Empty title="No flows">Create flows on an agent's Traffic tab, or with a scenario.</Empty>
        ) : rows.length === 0 ? (
          <div className="muted">No flow matches.</div>
        ) : (
          <div className="table-wrap">
            <table className="table hover dense">
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Flow</th>
                  <th>Path</th>
                  <th>State</th>
                  <th className="num">Sent</th>
                  <th className="num">Lost</th>
                  <th className="num" title="Longest outage (consecutive losses)">Outage</th>
                  <th className="num">RTT avg</th>
                  <th>Checks</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((r) => {
                  const s = r.p.status ? asFlow(r.p.status as JsonObject) : undefined;
                  const sum = r.p.state === "running" && r.p.status ? flowSummary(r.p.status) : { health: "bad" as const, text: r.p.error || r.p.state };
                  const link = href({ page: "protocol", agent: r.agentId, protocol: r.p.name });
                  return (
                    <tr key={r.agentId + "/" + r.p.name} onClick={() => navigate({ page: "protocol", agent: r.agentId, protocol: r.p.name })}>
                      <td>
                        {r.agent}
                        {r.lab && <span className="tag">{r.lab}</span>}
                      </td>
                      <td className="mono strong">
                        <a href={link} onClick={(e) => e.stopPropagation()}>
                          {r.p.name}
                        </a>
                      </td>
                      <td className="mono">
                        <span className="muted">{r.p.device} · </span>
                        {s?.source || "?"} → {s?.destination ?? "?"}
                      </td>
                      <td>
                        {/* A failed check shows in the Checks column; the state stays short. */}
                        <HealthText health={sum.health}>{s && (sum.health !== "bad" || s.verdict?.result === "fail") ? s.state : sum.text}</HealthText>
                      </td>
                      <td className="num mono">{s?.tx.toLocaleString() ?? "—"}</td>
                      <td className={s?.lost ? "num mono bad-text" : "num mono"}>
                        {s && s.tracks_replies !== false ? `${s.lost.toLocaleString()} (${s.loss_pct.toFixed(2)}%)` : "one way"}
                      </td>
                      <td className={s?.outage_current_ms ? "num mono bad-text" : "num mono"}>
                        {s?.outage_current_ms ? `now ${dur(s.outage_current_ms)}` : s?.outage_max_ms ? dur(s.outage_max_ms) : "—"}
                      </td>
                      <td className="num mono">{ms(s?.rtt?.avg_us)}</td>
                      <td>
                        <VerdictBadge v={s?.verdict} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

function Stat({ label, n, tone }: { label: string; n: number; tone?: "ok" | "bad" }) {
  return (
    <div className={`stat ${tone === "bad" && n > 0 ? "bad" : ""}`}>
      <div className={`stat-n ${tone && n > 0 ? tone + "-text" : ""}`}>{n}</div>
      <div className="stat-l">{label}</div>
    </div>
  );
}
