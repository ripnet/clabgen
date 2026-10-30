import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { client, usePoll } from "../api";
import { agentLabel, useAgents } from "../agents";
import { href } from "../router";
import { useBusy } from "../ui";
import { asFlow, dur, FlowLog, FlowTiles, FlowTimeline, ms, VerdictPanel } from "./Flow";
// useReceived polls received flows on every known agent (or one).
export function useReceived(only) {
    const { agents, info } = useAgents();
    const targets = agents.filter((a) => !only || a.id === only);
    const key = targets.map((a) => a.id).join(",");
    const p = usePoll(async () => {
        const res = await Promise.allSettled(targets.map((a) => client(a.id).listReceivedFlows({}, { timeoutMs: 3000 })));
        const out = [];
        res.forEach((r, i) => {
            if (r.status !== "fulfilled")
                return;
            for (const x of r.value.flows)
                out.push({ agentId: targets[i].id, agent: agentLabel(targets[i], info[targets[i].id]), device: x.device, f: (x.stats ?? {}) });
        });
        return out;
    }, [key], 2000);
    return { list: p.data ?? [], loading: p.loading };
}
// useFlowIndex maps flow IDs to the sending flow (agent + name) across
// agents, so receivers can name what they see.
export function useFlowIndex() {
    const { agents, info } = useAgents();
    const key = agents.map((a) => a.id).join(",");
    const p = usePoll(async () => {
        const m = new Map();
        const res = await Promise.allSettled(agents.map((a) => client(a.id).listProtocols({ withStatus: true, brief: true, noGroupMembers: true }, { timeoutMs: 3000 })));
        res.forEach((r, i) => {
            if (r.status !== "fulfilled")
                return;
            for (const pr of r.value.protocols) {
                const id = pr.status?.flow_id;
                if (id)
                    m.set(id, { agentId: agents[i].id, agent: agentLabel(agents[i], info[agents[i].id]), name: pr.name });
            }
        });
        return m;
    }, [key], 5000);
    return p.data ?? new Map();
}
// useClearFlows clears flows' statistics and, so one-way numbers stay
// consistent, the same flows' counts at every receiver on every agent.
export function useClearFlows() {
    const { agents } = useAgents();
    return async (agentId, flows) => {
        for (const f of flows)
            await client(agentId).runProtocolAction({ name: f.name, action: "clear" });
        const ids = flows.map((f) => f.flowId).filter((x) => !!x);
        await Promise.allSettled(agents.flatMap((a) => ids.map((flowId) => client(a.id).clearReceivedFlows({ flowId }))));
    };
}
const n = (v) => v.toLocaleString();
const ep = (addr, port) => (port ? (addr.includes(":") ? `[${addr}]:${port}` : `${addr}:${port}`) : addr);
// ReceivedTable lists received flows; `sender` names each one's source flow.
export function ReceivedTable({ rows, showAgent, index }) {
    return (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [showAgent && _jsx("th", { children: "Receiver" }), !showAgent && _jsx("th", { children: "Device" }), index && _jsx("th", { children: "Flow" }), _jsx("th", { children: "From \u2192 to" }), _jsx("th", { className: "num", children: "Received" }), _jsx("th", { className: "num", children: "Rate" }), _jsx("th", { className: "num", children: "Lost" }), _jsx("th", { className: "num", children: "Reord / dup" }), _jsx("th", { className: "num", children: "Latency avg / max" }), _jsx("th", { className: "num", children: "Jitter" }), _jsx("th", { className: "num", children: "Longest outage" }), _jsx("th", { className: "num", children: "Last packet" })] }) }), _jsx("tbody", { children: rows.map((r) => {
                        const f = r.f;
                        const sender = index?.get(f.flow_id);
                        const stale = f.idle_ms > 3000;
                        return (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: showAgent ? `${r.agent} / ${r.device}` : r.device }), index && (_jsx("td", { className: "mono", children: sender ? (_jsxs("a", { href: href({ page: "protocol", agent: sender.agentId, protocol: sender.name }), children: [sender.agent, "/", sender.name] })) : (_jsx("span", { className: "muted", children: f.flow_id })) })), _jsxs("td", { className: "mono", children: [f.proto, " ", ep(f.src, f.src_port), f.src_macs && f.src_macs > 1 ? _jsxs("span", { className: "muted", children: [" \u00D7", f.src_macs] }) : null, " \u2192 ", ep(f.dst, f.dst_port), f.ethertype && _jsxs("span", { className: "muted", children: [" ", f.ethertype] })] }), _jsx("td", { className: "num mono", children: n(f.rx) }), _jsx("td", { className: "num mono", children: stale ? "—" : `${f.rate_pps.toFixed(0)} pps` }), _jsxs("td", { className: f.lost ? "num mono bad-text" : "num mono", children: [n(f.lost), " (", f.loss_pct.toFixed(2), "%)"] }), _jsxs("td", { className: f.reordered || f.duplicate ? "num mono warn-text" : "num mono", children: [n(f.reordered), " / ", n(f.duplicate)] }), _jsxs("td", { className: "num mono", children: [ms(f.latency_avg_us), " / ", ms(f.latency_max_us)] }), _jsx("td", { className: "num mono", children: ms(f.jitter_us) }), _jsx("td", { className: f.outage_max_ms ? "num mono warn-text" : "num mono", children: f.outage_max_ms ? `${dur(f.outage_max_ms)} (${f.outages})` : "—" }), _jsx("td", { className: "num mono", children: f.idle_ms < 1000 ? "now" : `${dur(f.idle_ms)} ago` })] }, r.agentId + r.device + f.flow_id + f.proto));
                    }) })] }) }));
}
// Legs splits a round trip into its two directions when the far end is a
// clabgen device: the outbound leg is what the destination received; the
// return leg is the rest. One-way latencies are valid because the agents
// share the host clock.
function Legs({ s, fwd }) {
    const f = fwd.f;
    const retSent = f.rx; // the far end answers what it receives
    // Settled losses on both sides, so the two polls being a moment apart
    // (packets in flight) don't show up as return loss.
    const retLost = Math.max(0, s.lost - f.lost);
    const pct = (lost, total) => (total ? ((100 * lost) / total).toFixed(2) : "0.00");
    const retLat = s.rtt ? Math.max(0, s.rtt.avg_us - f.latency_avg_us) : undefined;
    return (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Legs" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Direction" }), _jsx("th", { className: "num", children: "Sent" }), _jsx("th", { className: "num", children: "Received" }), _jsx("th", { className: "num", children: "Lost" }), _jsx("th", { className: "num", children: "Longest outage" }), _jsx("th", { className: "num", children: "Latency avg" })] }) }), _jsxs("tbody", { children: [_jsxs("tr", { children: [_jsxs("td", { className: "mono", children: ["outbound \u2192 ", fwd.agent, "/", fwd.device] }), _jsx("td", { className: "num mono", children: n(s.tx) }), _jsx("td", { className: "num mono", children: n(f.rx) }), _jsxs("td", { className: f.lost ? "num mono bad-text" : "num mono", children: [n(f.lost), " (", pct(f.lost, f.rx + f.lost), "%)"] }), _jsx("td", { className: "num mono", children: f.outage_max_ms ? dur(f.outage_max_ms) : "—" }), _jsx("td", { className: "num mono", children: ms(f.latency_avg_us) })] }), _jsxs("tr", { children: [_jsx("td", { className: "mono", children: "return \u2190" }), _jsx("td", { className: "num mono", children: n(retSent) }), _jsx("td", { className: "num mono", children: n(s.rx) }), _jsxs("td", { className: retLost ? "num mono bad-text" : "num mono", children: [n(retLost), " (", pct(retLost, retSent), "%)"] }), _jsx("td", { className: "num mono muted", title: "Only the round trip's outages are measured", children: "\u2014" }), _jsx("td", { className: "num mono", title: "Round-trip time minus the outbound latency", children: ms(retLat) })] })] })] }) }), _jsx("div", { className: "muted small-note", children: "Outbound numbers come from the destination's receive stats; return loss is the round trip's loss minus the outbound loss. Sent and received are read a moment apart, so they can differ by the packets in flight." })] }));
}
// Receivers shows every clabgen device that received this flow.
function Receivers({ flowId, s }) {
    const { list, loading } = useReceived();
    const rows = list.filter((r) => r.f.flow_id === flowId);
    const fwd = s.tracks_replies !== false ? rows.find((r) => r.f.dst === s.destination) : undefined;
    return (_jsxs(_Fragment, { children: [fwd && _jsx(Legs, { s: s, fwd: fwd }), _jsx("h4", { children: "Receivers (one way)" }), rows.length ? (_jsx(ReceivedTable, { rows: rows, showAgent: true })) : (_jsx("div", { className: "muted", children: loading ? "Looking…" : "No clabgen device has received this flow (receivers count signed packets automatically)." }))] }));
}
// IpFlowView is the status panel for IP flows (icmp, udp, …).
export function IpFlowView({ status, agentId, name, extra }) {
    const s = asFlow(status);
    const { busy, run } = useBusy();
    const clearFlows = useClearFlows();
    const flowId = status.flow_id;
    const act = (action) => agentId &&
        name &&
        run(() => (action === "clear" ? clearFlows(agentId, [{ name, flowId }]) : client(agentId).runProtocolAction({ name, action })));
    const running = s.state === "running" || s.state === "resolving";
    const st = status;
    return (_jsxs("div", { className: "flow", children: [_jsxs("div", { className: "flow-head", children: [_jsxs("div", { className: "flow-path mono", children: [_jsx("span", { className: `pill ${running ? "pill-on" : ""}`, children: s.state }), " ", s.source || "?", " \u2192 ", _jsx("b", { children: s.destination }), s.next_hop && s.next_hop !== s.destination && _jsxs("span", { className: "muted", children: [" via ", s.next_hop] }), s.dst_mac && _jsxs("span", { className: "muted", children: [" (", s.dst_mac, ")"] })] }), _jsxs("div", { className: "muted", children: [s.kind, " \u00B7 ", s.rate, " pps \u00B7 ", s.mode, s.elapsed_s ? ` · ${s.elapsed_s.toFixed(0)} s` : "", extra] }), agentId && name && (_jsxs("div", { className: "btn-row", children: [running ? (_jsx("button", { className: "btn", disabled: busy, onClick: () => act("stop"), children: "\u25A0 Stop" })) : (_jsx("button", { className: "btn primary", disabled: busy, onClick: () => act("start"), children: "\u25B6 Start" })), _jsx("button", { className: "btn", disabled: busy, onClick: () => act("clear"), title: "Reset this flow's statistics, here and at its receivers", children: "Clear stats" })] }))] }), s.error && _jsx("div", { className: "warn-box", children: s.error }), !s.error && st.last_error && (_jsxs("div", { className: "muted small-note", children: ["last error ", st.last_error_age_s?.toFixed(0), " s ago: ", st.last_error] })), _jsx(FlowTiles, { s: s }), s.verdict && _jsx(VerdictPanel, { v: s.verdict }), s.tracks_replies !== false && _jsx(FlowTimeline, { s: s }), flowId && _jsx(Receivers, { flowId: flowId, s: s }), (s.tracks_replies !== false || s.recent.length > 0) && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Packets" }), _jsx(FlowLog, { s: s })] }))] }));
}
