import { agentLabel, useAgents } from "../agents";
import { agentTabs, href, type AgentTab } from "../router";
import { Dot, Empty } from "../ui";
import { ago } from "../components/ValueView";
import { displayVersion, skew } from "../version";
import { ProtocolsTab } from "./ProtocolsTab";
import { TrafficTab } from "./TrafficTab";
import { DevicesTab } from "./DevicesTab";
import { GroupsTab } from "./GroupsTab";
import { PortsTab } from "./PortsTab";
import { CaptureTab } from "./CaptureTab";
import { EventsTab } from "./EventsTab";
import { ScenarioTab } from "./ScenarioTab";

export function AgentHeader({ agentId, children }: { agentId: string; children?: React.ReactNode }) {
  const { agents, info, ui } = useAgents();
  const a = agents.find((x) => x.id === agentId);
  const i = info[agentId];
  const k = i?.reachable ? skew(i.version, ui?.version) : undefined;
  return (
    <div className="agent-head">
      <div className="agent-title">
        <Dot health={i?.reachable ? "ok" : i ? "bad" : "idle"} title={i?.error} />
        <h1>{agentLabel(a, i)}</h1>
        {a?.lab && <span className="tag">{a.lab}</span>}
      </div>
      <div className="agent-meta muted">
        {i?.reachable ? (
          <>
            {i.hostname} · {displayVersion(i.version)}
            {k && <span className={k.kind === "older" ? "warn-text" : ""} title={k.text}> (UI {displayVersion(ui?.version)})</span>}
            {i.started && <> · up since {ago(i.started)}</>}
            {a?.url && <> · {a.url}</>}
          </>
        ) : (
          i?.error ?? "connecting…"
        )}
      </div>
      {children}
    </div>
  );
}

export function AgentPage({ agentId, tab }: { agentId: string; tab: AgentTab }) {
  const { agents } = useAgents();
  if (agents.length && !agents.some((a) => a.id === agentId)) {
    return (
      <div className="page">
        <Empty title="Agent not found">
          <a href="#/">Back to overview</a>
        </Empty>
      </div>
    );
  }
  return (
    <div className="page">
      <AgentHeader agentId={agentId}>
        <nav className="tabs" aria-label="Agent sections">
          {agentTabs.map((t) => (
            <a key={t} href={href({ page: "agent", agent: agentId, tab: t })} className={t === tab ? "tab active" : "tab"}>
              {t}
            </a>
          ))}
        </nav>
      </AgentHeader>
      {tab === "ports" && <PortsTab agentId={agentId} />}
      {tab === "devices" && <DevicesTab agentId={agentId} />}
      {tab === "groups" && <GroupsTab agentId={agentId} />}
      {tab === "protocols" && <ProtocolsTab agentId={agentId} />}
      {tab === "traffic" && <TrafficTab agentId={agentId} />}
      {tab === "capture" && <CaptureTab agentId={agentId} />}
      {tab === "events" && <EventsTab agentId={agentId} />}
      {tab === "scenario" && <ScenarioTab agentId={agentId} />}
    </div>
  );
}
