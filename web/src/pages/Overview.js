import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { skew } from "../version";
import { useEffect, useState } from "react";
import { client, errorText } from "../api";
import { agentLabel, useAgents } from "../agents";
import { href } from "../router";
import { summarize } from "../protocols/registry";
import { Dot, Empty, HealthText } from "../ui";
import { groupHealth, groupStatus } from "./GroupsTab";
const rank = { bad: 0, warn: 1, idle: 2, ok: 3 };
// Overview polls every agent and shows each protocol's health in one table,
// worst first, so a lab-wide fault is visible at a glance.
export function Overview() {
    const { agents, info, ui } = useAgents();
    const [snap, setSnap] = useState({});
    const ids = agents.map((a) => a.id).join("|");
    useEffect(() => {
        let cancelled = false;
        const poll = () => {
            if (document.visibilityState !== "visible")
                return;
            for (const a of agents) {
                const c = client(a.id);
                Promise.all([
                    c.listProtocols({ withStatus: true, brief: true, noGroupMembers: true }, { timeoutMs: 4000 }),
                    // Agents from before device groups don't have the call.
                    c.listDeviceGroups({}, { timeoutMs: 4000 }).catch(() => ({ groups: [] })),
                    c.listPorts({}, { timeoutMs: 4000 }),
                ]).then(([p, g, ports]) => !cancelled && setSnap((m) => ({ ...m, [a.id]: { protocols: p.protocols, groups: g.groups, ports: ports.ports } })), (e) => !cancelled && setSnap((m) => ({ ...m, [a.id]: { error: errorText(e) } })));
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
        return (_jsxs("div", { className: "page", children: [_jsx("h1", { children: "Lab overview" }), _jsxs(Empty, { title: "No agents found", children: ["clabgen-ui discovers running containerlab nodes whose image matches ", _jsx("code", { children: "clabgen" }), ". You can also start it with ", _jsx("code", { children: "-lab topo.clab.yml" }), " or ", _jsx("code", { children: "-agent host:port" }), ", or add an agent from the sidebar."] })] }));
    }
    // One row per protocol, and per group protocol (its members rolled up).
    const rows = agents.flatMap((a) => [
        ...(snap[a.id]?.protocols ?? []).map((p) => ({
            a,
            key: p.name,
            name: p.name,
            type: p.type,
            device: p.device,
            link: href({ page: "protocol", agent: a.id, protocol: p.name }),
            s: summarize(p.type, p.state, p.error, p.status),
        })),
        ...(snap[a.id]?.groups ?? []).flatMap((g) => {
            const st = groupStatus(g);
            return st.protocols.map((p) => ({
                a,
                key: "group/" + g.name + "/" + p.name,
                name: `${g.name}/${p.name}`,
                type: p.type,
                device: `group ${g.name} (${st.devices})`,
                link: href({ page: "group", agent: a.id, group: g.name }),
                s: groupHealth(p),
            }));
        }),
    ]);
    rows.sort((x, y) => rank[x.s.health] - rank[y.s.health] || agentLabel(x.a).localeCompare(agentLabel(y.a)) || x.name.localeCompare(y.name));
    const counts = { ok: 0, warn: 0, bad: 0, idle: 0 };
    rows.forEach((r) => counts[r.s.health]++);
    return (_jsxs("div", { className: "page", children: [_jsxs("div", { className: "page-head", children: [_jsx("h1", { children: "Lab overview" }), _jsxs("div", { className: "stat-row", children: [_jsx(Stat, { label: "Healthy", n: counts.ok, h: "ok" }), _jsx(Stat, { label: "Warning", n: counts.warn, h: "warn" }), _jsx(Stat, { label: "Fault", n: counts.bad, h: "bad" }), _jsx(Stat, { label: "Waiting", n: counts.idle, h: "idle" })] })] }), _jsx(VersionNote, { agents: agents.map((a) => skew(info[a.id]?.reachable ? info[a.id]?.version : undefined, ui?.version)) }), _jsx("div", { className: "agent-cards", children: agents.map((a) => {
                    const s = snap[a.id];
                    const i = info[a.id];
                    const portsUp = s?.ports?.filter((p) => p.adminUp && p.operUp).length ?? 0;
                    return (_jsxs("a", { className: "agent-card", href: href({ page: "agent", agent: a.id, tab: "protocols" }), children: [_jsxs("div", { className: "agent-card-head", children: [_jsx(Dot, { health: i?.reachable ? "ok" : i ? "bad" : "idle" }), _jsx("span", { className: "agent-card-name", children: agentLabel(a, i) }), a.lab && _jsx("span", { className: "tag", children: a.lab })] }), s?.error ? (_jsx("div", { className: "agent-card-body error-text", children: s.error })) : (_jsx("div", { className: "agent-card-body muted", children: s?.ports ? `${portsUp}/${s.ports.length} ports up · ${s.protocols?.length ?? 0} protocols${s.groups?.length ? ` · ${s.groups.length} groups` : ""}` : "loading…" }))] }, a.id));
                }) }), _jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Protocols" }), _jsx("span", { className: "muted", children: "worst first \u00B7 refreshes every 2 s" })] }), rows.length === 0 ? (_jsx(Empty, { title: "No protocols configured", children: "Open an agent and create one, or apply a scenario." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table hover", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Agent" }), _jsx("th", { children: "Protocol" }), _jsx("th", { children: "Type" }), _jsx("th", { children: "Device" }), _jsx("th", { children: "State" })] }) }), _jsx("tbody", { children: rows.map(({ a, key, name, type, device, link, s }) => (_jsxs("tr", { onClick: () => (window.location.hash = link), children: [_jsx("td", { children: agentLabel(a, info[a.id]) }), _jsx("td", { className: "mono strong", children: name }), _jsx("td", { className: "mono", children: type }), _jsx("td", { className: "mono", children: device }), _jsx("td", { children: _jsx(HealthText, { health: s.health, children: s.text }) })] }, a.id + "/" + key))) })] }) }))] })] }));
}
function Stat({ label, n, h }) {
    return (_jsxs("div", { className: `stat ${n && h !== "ok" && h !== "idle" ? h : ""}`, children: [_jsx("div", { className: "stat-n", children: n }), _jsxs("div", { className: "stat-l", children: [_jsx(Dot, { health: h }), " ", label] })] }));
}
// VersionNote says how many agents run another release than the UI.
function VersionNote({ agents }) {
    const older = agents.filter((k) => k?.kind === "older").length;
    const newer = agents.filter((k) => k?.kind === "newer").length;
    if (!older && !newer)
        return null;
    return (_jsxs("div", { className: "warn-box", children: [older > 0 && `${older} agent${older > 1 ? "s run" : " runs"} an older clabgen than this UI: some features may fail there until the lab is redeployed with the newer image. `, newer > 0 && `${newer} agent${newer > 1 ? "s run" : " runs"} a newer clabgen than this UI: update clabgen-ui to see everything.`] }));
}
