import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useEffect, useMemo, useState } from "react";
import { client, errorText } from "../api";
import { agentLabel, useAgents } from "../agents";
import { asFlow, dur, flowSummary, ms, VerdictBadge } from "../components/Flow";
import { useClearFlows } from "../components/FlowViews";
import { href, navigate } from "../router";
import { Empty, ErrorBox, HealthText, useBusy } from "../ui";
const running = (p) => {
    const st = p.status ? asFlow(p.status).state : "";
    return st === "running" || st === "resolving";
};
const verdictOf = (p) => (p.status ? asFlow(p.status).verdict?.result : undefined);
// FlowsPage lists every agent's traffic flows with their checks: one place
// to start a test, run it, and see what passed.
export function FlowsPage() {
    const { agents, info } = useAgents();
    const [by, setBy] = useState({});
    const [filter, setFilter] = useState("");
    const [failing, setFailing] = useState(false);
    const ids = agents.map((a) => a.id).join("|");
    const { busy, run } = useBusy();
    const clearFlows = useClearFlows();
    // Traffic types per agent (flows are protocols of the "traffic" category).
    const [traffic, setTraffic] = useState({});
    useEffect(() => {
        for (const a of agents) {
            client(a.id)
                .listProtocolTypes({}, { timeoutMs: 4000 })
                .then((r) => setTraffic((m) => ({ ...m, [a.id]: new Set(r.types.filter((t) => t.category === "traffic").map((t) => t.name)) })))
                .catch(() => { });
        }
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [ids]);
    const [tick, setTick] = useState(0);
    useEffect(() => {
        let cancelled = false;
        const poll = () => {
            if (document.visibilityState !== "visible")
                return;
            for (const a of agents) {
                client(a.id)
                    .listProtocols({ withStatus: true, brief: true, noGroupMembers: true }, { timeoutMs: 4000 })
                    .then((r) => !cancelled && setBy((m) => ({ ...m, [a.id]: { flows: r.protocols } })), (e) => !cancelled && setBy((m) => ({ ...m, [a.id]: { flows: m[a.id]?.flows ?? [], error: errorText(e) } })));
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
    const all = useMemo(() => agents.flatMap((a) => (by[a.id]?.flows ?? [])
        .filter((p) => traffic[a.id]?.has(p.type) && p.type !== "udp-echo")
        .map((p) => ({ agentId: a.id, agent: agentLabel(a, info[a.id]), lab: a.lab, p }))), [agents, by, traffic, info]);
    const f = filter.trim().toLowerCase();
    const rows = all.filter((r) => (!failing || verdictOf(r.p) === "fail") &&
        (!f || [r.agent, r.lab ?? "", r.p.name, r.p.device, r.p.type, asFlow(r.p.status ?? {}).destination ?? ""].some((x) => x.toLowerCase().includes(f))));
    const count = (pred) => all.filter(pred).length;
    const stats = {
        flows: all.length,
        running: count((r) => running(r.p)),
        pass: count((r) => verdictOf(r.p) === "pass"),
        fail: count((r) => verdictOf(r.p) === "fail"),
        pending: count((r) => verdictOf(r.p) === "pending"),
    };
    const errors = agents.filter((a) => by[a.id]?.error).map((a) => `${agentLabel(a, info[a.id])}: ${by[a.id].error}`);
    // Lab-wide actions apply to the rows shown (the filter narrows them).
    const each = (action) => run(async () => {
        const todo = rows.filter((r) => (action === "start" ? !running(r.p) : running(r.p)));
        await Promise.all(todo.map((r) => client(r.agentId).runProtocolAction({ name: r.p.name, action })));
    }).then(() => setTick((t) => t + 1));
    const clearAll = () => run(async () => {
        const byAgent = new Map();
        for (const r of rows)
            byAgent.set(r.agentId, [...(byAgent.get(r.agentId) ?? []), r]);
        for (const [agentId, rs] of byAgent)
            await clearFlows(agentId, rs.map((r) => ({ name: r.p.name, flowId: r.p.status?.flow_id })));
    }, "Statistics cleared").then(() => setTick((t) => t + 1));
    return (_jsxs("div", { className: "page", children: [_jsxs("div", { className: "page-head", children: [_jsx("h1", { children: "Flows" }), _jsxs("div", { className: "stat-row", children: [_jsx(Stat, { label: "Flows", n: stats.flows }), _jsx(Stat, { label: "Running", n: stats.running }), _jsx(Stat, { label: "Pass", n: stats.pass, tone: "ok" }), _jsx(Stat, { label: "Fail", n: stats.fail, tone: "bad" }), _jsx(Stat, { label: "Pending", n: stats.pending })] })] }), _jsxs("p", { className: "muted", children: ["Every agent's traffic flows. Give a flow ", _jsx("b", { children: "checks" }), " (in its config: max loss, outage, RTT, jitter) and it passes or fails on what it measures from its start or last clear: start the flows, run the test (flap a peer, fail a link), and see the result here."] }), errors.map((e) => (_jsx(ErrorBox, { error: e }, e))), _jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsxs("div", { className: "btn-row", children: [_jsx("input", { className: "input", style: { width: 280 }, placeholder: "Filter: agent, lab, flow, device, destination", value: filter, onChange: (e) => setFilter(e.target.value) }), _jsxs("label", { className: "switch small", children: [_jsx("input", { type: "checkbox", checked: failing, onChange: (e) => setFailing(e.target.checked) }), " ", _jsx("span", { children: "failing only" })] })] }), _jsxs("div", { className: "btn-row", children: [_jsx("button", { className: "btn", disabled: busy || !rows.some((r) => !running(r.p)), onClick: () => each("start"), title: "Start the flows listed", children: "\u25B6 Start all" }), _jsx("button", { className: "btn", disabled: busy || !rows.some((r) => running(r.p)), onClick: () => each("stop"), title: "Stop the flows listed", children: "\u25A0 Stop all" }), _jsx("button", { className: "btn", disabled: busy || !rows.length, onClick: clearAll, title: "Reset the listed flows' statistics and checks, here and at their receivers", children: "\u21BA Clear all stats" })] })] }), all.length === 0 ? (_jsx(Empty, { title: "No flows", children: "Create flows on an agent's Traffic tab, or with a scenario." })) : rows.length === 0 ? (_jsx("div", { className: "muted", children: "No flow matches." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table hover dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Agent" }), _jsx("th", { children: "Flow" }), _jsx("th", { children: "Path" }), _jsx("th", { children: "State" }), _jsx("th", { className: "num", children: "Sent" }), _jsx("th", { className: "num", children: "Lost" }), _jsx("th", { className: "num", title: "Longest outage (consecutive losses)", children: "Outage" }), _jsx("th", { className: "num", children: "RTT avg" }), _jsx("th", { children: "Checks" })] }) }), _jsx("tbody", { children: rows.map((r) => {
                                        const s = r.p.status ? asFlow(r.p.status) : undefined;
                                        const sum = r.p.state === "running" && r.p.status ? flowSummary(r.p.status) : { health: "bad", text: r.p.error || r.p.state };
                                        const link = href({ page: "protocol", agent: r.agentId, protocol: r.p.name });
                                        return (_jsxs("tr", { onClick: () => navigate({ page: "protocol", agent: r.agentId, protocol: r.p.name }), children: [_jsxs("td", { children: [r.agent, r.lab && _jsx("span", { className: "tag", children: r.lab })] }), _jsx("td", { className: "mono strong", children: _jsx("a", { href: link, onClick: (e) => e.stopPropagation(), children: r.p.name }) }), _jsxs("td", { className: "mono", children: [_jsxs("span", { className: "muted", children: [r.p.device, " \u00B7 "] }), s?.source || "?", " \u2192 ", s?.destination ?? "?"] }), _jsx("td", { children: _jsx(HealthText, { health: sum.health, children: s && (sum.health !== "bad" || s.verdict?.result === "fail") ? s.state : sum.text }) }), _jsx("td", { className: "num mono", children: s?.tx.toLocaleString() ?? "—" }), _jsx("td", { className: s?.lost ? "num mono bad-text" : "num mono", children: s && s.tracks_replies !== false ? `${s.lost.toLocaleString()} (${s.loss_pct.toFixed(2)}%)` : "one way" }), _jsx("td", { className: s?.outage_current_ms ? "num mono bad-text" : "num mono", children: s?.outage_current_ms ? `now ${dur(s.outage_current_ms)}` : s?.outage_max_ms ? dur(s.outage_max_ms) : "—" }), _jsx("td", { className: "num mono", children: ms(s?.rtt?.avg_us) }), _jsx("td", { children: _jsx(VerdictBadge, { v: s?.verdict }) })] }, r.agentId + "/" + r.p.name));
                                    }) })] }) }))] })] }));
}
function Stat({ label, n, tone }) {
    return (_jsxs("div", { className: `stat ${tone === "bad" && n > 0 ? "bad" : ""}`, children: [_jsx("div", { className: `stat-n ${tone && n > 0 ? tone + "-text" : ""}`, children: n }), _jsx("div", { className: "stat-l", children: label })] }));
}
