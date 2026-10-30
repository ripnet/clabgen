import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { agentLabel, useAgents } from "../agents";
import { agentTabs, href } from "../router";
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
export function AgentHeader({ agentId, children }) {
    const { agents, info, ui } = useAgents();
    const a = agents.find((x) => x.id === agentId);
    const i = info[agentId];
    const k = i?.reachable ? skew(i.version, ui?.version) : undefined;
    return (_jsxs("div", { className: "agent-head", children: [_jsxs("div", { className: "agent-title", children: [_jsx(Dot, { health: i?.reachable ? "ok" : i ? "bad" : "idle", title: i?.error }), _jsx("h1", { children: agentLabel(a, i) }), a?.lab && _jsx("span", { className: "tag", children: a.lab })] }), _jsx("div", { className: "agent-meta muted", children: i?.reachable ? (_jsxs(_Fragment, { children: [i.hostname, " \u00B7 ", displayVersion(i.version), k && _jsxs("span", { className: k.kind === "older" ? "warn-text" : "", title: k.text, children: [" (UI ", displayVersion(ui?.version), ")"] }), i.started && _jsxs(_Fragment, { children: [" \u00B7 up since ", ago(i.started)] }), a?.url && _jsxs(_Fragment, { children: [" \u00B7 ", a.url] })] })) : (i?.error ?? "connecting…") }), children] }));
}
// Crumbs is the trail on a detail page, where the agent's tabs would be: a
// back arrow to the parent, then agent › section › … › the page itself.
export function Crumbs({ agentId, trail, current }) {
    const { agents, info } = useAgents();
    const parent = trail[trail.length - 1];
    return (_jsxs("nav", { className: "crumbs", "aria-label": "Breadcrumb", children: [parent && (_jsx("a", { className: "crumb-back", href: parent.href, title: `Back to ${parent.label}`, "aria-label": `Back to ${parent.label}`, children: "\u2190" })), _jsx("a", { href: href({ page: "agent", agent: agentId, tab: "protocols" }), children: agentLabel(agents.find((a) => a.id === agentId), info[agentId]) }), trail.map((t) => (_jsxs("span", { className: "crumb", children: [_jsx("span", { className: "crumb-sep", children: "\u203A" }), _jsx("a", { href: t.href, children: t.label })] }, t.href))), _jsxs("span", { className: "crumb", children: [_jsx("span", { className: "crumb-sep", children: "\u203A" }), _jsx("span", { className: "mono crumb-current", "aria-current": "page", children: current })] })] }));
}
export function AgentPage({ agentId, tab }) {
    const { agents } = useAgents();
    if (agents.length && !agents.some((a) => a.id === agentId)) {
        return (_jsx("div", { className: "page", children: _jsx(Empty, { title: "Agent not found", children: _jsx("a", { href: "#/", children: "Back to overview" }) }) }));
    }
    return (_jsxs("div", { className: "page", children: [_jsx(AgentHeader, { agentId: agentId, children: _jsx("nav", { className: "tabs", "aria-label": "Agent sections", children: agentTabs.map((t) => (_jsx("a", { href: href({ page: "agent", agent: agentId, tab: t }), className: t === tab ? "tab active" : "tab", children: t }, t))) }) }), tab === "ports" && _jsx(PortsTab, { agentId: agentId }), tab === "devices" && _jsx(DevicesTab, { agentId: agentId }), tab === "groups" && _jsx(GroupsTab, { agentId: agentId }), tab === "protocols" && _jsx(ProtocolsTab, { agentId: agentId }), tab === "traffic" && _jsx(TrafficTab, { agentId: agentId }), tab === "capture" && _jsx(CaptureTab, { agentId: agentId }), tab === "events" && _jsx(EventsTab, { agentId: agentId }), tab === "scenario" && _jsx(ScenarioTab, { agentId: agentId })] }));
}
