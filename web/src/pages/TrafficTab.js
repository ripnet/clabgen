import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useState } from "react";
import { client, usePoll } from "../api";
import { asFlow, dur, flowSummary, ms, VerdictBadge } from "../components/Flow";
import { href, navigate } from "../router";
import { Empty, ErrorBox, HealthText, useBusy } from "../ui";
import { CreateProtocol } from "./ProtocolsTab";
import { ReceivedTable, useClearFlows, useFlowIndex, useReceived } from "../components/FlowViews";
// TrafficTab lists the agent's traffic flows (types in the "traffic"
// category) with their live statistics.
export function TrafficTab({ agentId }) {
    const c = client(agentId);
    const list = usePoll(() => c.listProtocols({ withStatus: true, brief: true, noGroupMembers: true }), [agentId], 1000);
    const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
    const devices = usePoll(() => c.listDevices({}), [agentId], 5000);
    // Before a flow first runs it hasn't picked a source: show the device's
    // first address of the destination's family.
    const guessSource = (device, dst) => {
        const v6 = !!dst?.includes(":");
        const d = devices.data?.devices.find((d) => d.name === device);
        const addrs = [...(d?.addresses ?? []), ...(d?.dynamicAddresses ?? [])];
        return addrs.map((a) => a.split("/")[0]).find((a) => a.includes(":") === v6 && !a.startsWith("fe80:"));
    };
    const [creating, setCreating] = useState(false);
    const { busy, run } = useBusy();
    const traffic = new Set(types.data?.types.filter((t) => t.category === "traffic").map((t) => t.name));
    // One-way flows: received/lost come from the clabgen receivers.
    const rx = useReceived();
    const oneWay = (id) => {
        const rs = rx.list.filter((r) => r.f.flow_id === id);
        return rs.length ? { rx: rs.reduce((a, r) => a + r.f.rx, 0), lost: rs.reduce((a, r) => a + r.f.lost, 0) } : undefined;
    };
    const flows = (list.data?.protocols ?? []).filter((p) => traffic.has(p.type));
    const running = (p) => {
        const st = p.status ? asFlow(p.status).state : "";
        return st === "running" || st === "resolving";
    };
    const clearFlows = useClearFlows();
    const flowRef = (p) => ({ name: p.name, flowId: p.status?.flow_id });
    const all = (action) => run(async () => {
        for (const p of flows.filter((p) => (action === "start" ? !running(p) : running(p))))
            await c.runProtocolAction({ name: p.name, action });
    }).then(list.refresh);
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Traffic" }), _jsxs("div", { className: "btn-row", children: [_jsx("button", { className: "btn", disabled: busy || !flows.some((p) => !running(p)), onClick: () => all("start"), children: "\u25B6 Start all" }), _jsx("button", { className: "btn", disabled: busy || !flows.some(running), onClick: () => all("stop"), children: "\u25A0 Stop all" }), _jsx("button", { className: "btn", disabled: busy || !flows.length, title: "Reset every flow's statistics, here and at their receivers", onClick: () => run(() => clearFlows(agentId, flows.map(flowRef)), "Statistics cleared").then(list.refresh), children: "\u21BA Clear all stats" }), _jsx("button", { className: "btn primary", onClick: () => setCreating(true), children: "New flow" })] })] }), _jsx("p", { className: "muted", children: "Flows send packets from a device and measure what comes back: loss, outages (consecutive losses) and round-trip time." }), _jsx(ErrorBox, { error: list.error }), !list.loading && flows.length === 0 ? (_jsx(Empty, { title: "No flows yet", children: "Create one here, or use Ping\u2026 on a device with an IP address." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table hover dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Flow" }), _jsx("th", { children: "Path" }), _jsx("th", { children: "Kind" }), _jsx("th", { className: "num", children: "Rate" }), _jsx("th", { children: "State" }), _jsx("th", { className: "num", children: "Sent" }), _jsx("th", { className: "num", children: "Received" }), _jsx("th", { className: "num", children: "Lost" }), _jsx("th", { className: "num", children: "Longest outage" }), _jsx("th", { className: "num", children: "RTT avg" }), _jsx("th", { title: "Pass/fail checks (set in the flow's config)", children: "Checks" }), _jsx("th", {})] }) }), _jsx("tbody", { children: flows.map((p) => {
                                const s = p.status ? asFlow(p.status) : undefined;
                                const sum = p.state === "running" && p.status ? flowSummary(p.status) : { health: "bad", text: p.error || p.state };
                                const on = running(p);
                                const ow = s && s.tracks_replies === false ? oneWay(p.status.flow_id) : undefined;
                                const owPct = ow && ow.rx + ow.lost ? (100 * ow.lost) / (ow.rx + ow.lost) : 0;
                                return (_jsxs("tr", { onClick: () => navigate({ page: "protocol", agent: agentId, protocol: p.name }), children: [_jsx("td", { className: "mono strong", children: _jsx("a", { href: href({ page: "protocol", agent: agentId, protocol: p.name }), onClick: (e) => e.stopPropagation(), children: p.name }) }), _jsxs("td", { className: "mono", children: [_jsxs("span", { className: "muted", children: [p.device, " \u00B7 "] }), s?.source || _jsx("span", { title: "expected source (not started yet)", children: guessSource(p.device, s?.destination) ?? "?" }), " \u2192 ", s?.destination ?? "?"] }), _jsxs("td", { className: "mono", children: [p.type, s?.kind ? ` ${s.kind}` : ""] }), _jsx("td", { className: "num mono", children: s ? `${s.rate} pps` : "—" }), _jsx("td", { children: _jsx(HealthText, { health: sum.health, children: s && (sum.health !== "bad" || s.verdict?.result === "fail") ? s.state : sum.text }) }), _jsx("td", { className: "num mono", children: s?.tx.toLocaleString() ?? "—" }), _jsx("td", { className: "num mono", title: ow ? "Received at clabgen receivers (one way)" : undefined, children: s && s.tracks_replies !== false ? s.rx.toLocaleString() : ow ? `${ow.rx.toLocaleString()} ↦` : "—" }), _jsx("td", { className: s?.lost || ow?.lost ? "num mono bad-text" : "num mono", children: s && s.tracks_replies !== false ? `${s.lost.toLocaleString()} (${s.loss_pct.toFixed(2)}%)` : ow ? `${ow.lost.toLocaleString()} (${owPct.toFixed(2)}%)` : "—" }), _jsx("td", { className: s?.outage_current_ms ? "num mono bad-text" : "num mono", children: s?.outage_current_ms ? `now ${dur(s.outage_current_ms)}` : s?.outage_max_ms ? dur(s.outage_max_ms) : "—" }), _jsx("td", { className: "num mono", children: ms(s?.rtt?.avg_us) }), _jsx("td", { children: _jsx(VerdictBadge, { v: s?.verdict }) }), _jsxs("td", { className: "actions", children: [_jsx("button", { className: "btn small", disabled: busy || p.state !== "running", onClick: (e) => {
                                                        e.stopPropagation();
                                                        run(() => c.runProtocolAction({ name: p.name, action: on ? "stop" : "start" })).then(list.refresh);
                                                    }, children: on ? "Stop" : "Start" }), _jsx("button", { className: "icon-btn row-clear", disabled: busy, title: "Clear this flow's statistics", "aria-label": `Clear ${p.name} statistics`, onClick: (e) => {
                                                        e.stopPropagation();
                                                        run(() => clearFlows(agentId, [flowRef(p)])).then(list.refresh);
                                                    }, children: "\u21BA" }), _jsx("button", { className: "btn small danger-ghost", disabled: busy, onClick: (e) => {
                                                        e.stopPropagation();
                                                        if (confirm(`Delete flow ${p.name}?`))
                                                            run(() => c.deleteProtocol({ name: p.name }), `Deleted ${p.name}`).then(list.refresh);
                                                    }, children: "Delete" })] })] }, p.name));
                            }) })] }) })), creating && _jsx(CreateProtocol, { agentId: agentId, category: "traffic", onClose: () => setCreating(false), onDone: list.refresh }), _jsx(Received, { agentId: agentId })] }));
}
// Received lists signed flows arriving at this agent's devices.
function Received({ agentId }) {
    const { list } = useReceived(agentId);
    const index = useFlowIndex();
    const { busy, run } = useBusy();
    return (_jsxs(_Fragment, { children: [_jsxs("div", { className: "card-head sub-head", children: [_jsx("h3", { children: "Received here" }), list.length > 0 && (_jsx("button", { className: "btn small", disabled: busy, onClick: () => run(() => client(agentId).clearReceivedFlows({}), "Cleared"), children: "Clear" }))] }), _jsx("p", { className: "muted", children: "Devices count every signed clabgen packet addressed to them: one-way loss, reordering, latency and outages." }), list.length ? _jsx(ReceivedTable, { rows: list, index: index }) : _jsx("div", { className: "muted", children: "Nothing received yet." })] }));
}
