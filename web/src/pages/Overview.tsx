import { useEffect, useState } from "react";
import type { Port, Protocol } from "../gen/clabgen/v1/agent_pb";
import { client, errorText } from "../api";
import { agentLabel, useAgents } from "../agents";
import { href } from "../router";
import { summarize, type Health } from "../protocols/registry";
import { Dot, Empty, HealthText } from "../ui";

type Snapshot = { protocols?: Protocol[]; ports?: Port[]; error?: string };

const rank: Record<Health, number> = { bad: 0, warn: 1, idle: 2, ok: 3 };

// Overview polls every agent and shows each protocol's health in one table,
// worst first, so a lab-wide fault is visible at a glance.
export function Overview() {
  const { agents, info } = useAgents();
  const [snap, setSnap] = useState<Record<string, Snapshot>>({});
  const ids = agents.map((a) => a.id).join("|");

  useEffect(() => {
    let cancelled = false;
    const poll = () => {
      if (document.visibilityState !== "visible") return;
      for (const a of agents) {
        const c = client(a.id);
        Promise.all([c.listProtocols({ withStatus: true }, { timeoutMs: 4000 }), c.listPorts({}, { timeoutMs: 4000 })]).then(
          ([p, ports]) => !cancelled && setSnap((m) => ({ ...m, [a.id]: { protocols: p.protocols, ports: ports.ports } })),
          (e) => !cancelled && setSnap((m) => ({ ...m, [a.id]: { error: errorText(e) } })),
        );
      }
    };
    poll();
    const t = setInterval(poll, 2000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids]);

  if (!agents.length) {
    return (
      <div className="page">
        <h1>Lab overview</h1>
        <Empty title="No agents found">
          clabgen-ui discovers running containerlab nodes whose image matches <code>clabgen</code>. You can also start it
          with <code>-lab topo.clab.yml</code> or <code>-agent host:port</code>, or add an agent from the sidebar.
        </Empty>
      </div>
    );
  }

  const rows = agents.flatMap((a) =>
    (snap[a.id]?.protocols ?? []).map((p) => ({ a, p, s: summarize(p.type, p.state, p.error, p.status) })),
  );
  rows.sort((x, y) => rank[x.s.health] - rank[y.s.health] || agentLabel(x.a).localeCompare(agentLabel(y.a)) || x.p.name.localeCompare(y.p.name));
  const counts = { ok: 0, warn: 0, bad: 0, idle: 0 } as Record<Health, number>;
  rows.forEach((r) => counts[r.s.health]++);

  return (
    <div className="page">
      <div className="page-head">
        <h1>Lab overview</h1>
        <div className="stat-row">
          <Stat label="Healthy" n={counts.ok} h="ok" />
          <Stat label="Warning" n={counts.warn} h="warn" />
          <Stat label="Fault" n={counts.bad} h="bad" />
          <Stat label="Waiting" n={counts.idle} h="idle" />
        </div>
      </div>

      <div className="agent-cards">
        {agents.map((a) => {
          const s = snap[a.id];
          const i = info[a.id];
          const portsUp = s?.ports?.filter((p) => p.adminUp && p.operUp).length ?? 0;
          return (
            <a key={a.id} className="agent-card" href={href({ page: "agent", agent: a.id, tab: "protocols" })}>
              <div className="agent-card-head">
                <Dot health={i?.reachable ? "ok" : i ? "bad" : "idle"} />
                <span className="agent-card-name">{agentLabel(a, i)}</span>
                {a.lab && <span className="tag">{a.lab}</span>}
              </div>
              {s?.error ? (
                <div className="agent-card-body error-text">{s.error}</div>
              ) : (
                <div className="agent-card-body muted">
                  {s?.ports ? `${portsUp}/${s.ports.length} ports up · ${s.protocols?.length ?? 0} protocols` : "loading…"}
                </div>
              )}
            </a>
          );
        })}
      </div>

      <div className="card">
        <div className="card-head">
          <h2>Protocols</h2>
          <span className="muted">worst first · refreshes every 2 s</span>
        </div>
        {rows.length === 0 ? (
          <Empty title="No protocols configured">Open an agent and create one, or apply a scenario.</Empty>
        ) : (
          <div className="table-wrap">
            <table className="table hover">
              <thead>
                <tr>
                  <th>Agent</th>
                  <th>Protocol</th>
                  <th>Type</th>
                  <th>Device</th>
                  <th>State</th>
                </tr>
              </thead>
              <tbody>
                {rows.map(({ a, p, s }) => (
                  <tr key={a.id + "/" + p.name} onClick={() => (window.location.hash = href({ page: "protocol", agent: a.id, protocol: p.name }))}>
                    <td>{agentLabel(a, info[a.id])}</td>
                    <td className="mono strong">{p.name}</td>
                    <td className="mono">{p.type}</td>
                    <td className="mono">{p.device}</td>
                    <td>
                      <HealthText health={s.health}>{s.text}</HealthText>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

function Stat({ label, n, h }: { label: string; n: number; h: Health }) {
  return (
    <div className={`stat ${n && h !== "ok" && h !== "idle" ? h : ""}`}>
      <div className="stat-n">{n}</div>
      <div className="stat-l">
        <Dot health={h} /> {label}
      </div>
    </div>
  );
}
