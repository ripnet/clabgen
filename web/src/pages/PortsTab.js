import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useRef } from "react";
import { client, usePoll } from "../api";
import { Dot, Empty, ErrorBox, useBusy } from "../ui";
const rate = (d, ms) => (ms > 0 ? (Number(d) * 1000) / ms : 0);
const fmtRate = (r) => (r < 0.05 ? "—" : r < 10 ? r.toFixed(1) : Math.round(r).toLocaleString());
export function PortsTab({ agentId }) {
    const c = client(agentId);
    const ports = usePoll(() => c.listPorts({}), [agentId], 1000);
    const prev = useRef({});
    const rates = useRef({});
    const { busy, run } = useBusy();
    const now = Date.now();
    for (const p of ports.data?.ports ?? []) {
        const k = p.counters;
        if (!k)
            continue;
        const s = prev.current[p.name];
        if (!s || now - s.t >= 900) {
            if (s)
                rates.current[p.name] = { rx: rate(k.rxFrames - s.rx, now - s.t), tx: rate(k.txFrames - s.tx, now - s.t) };
            prev.current[p.name] = { t: now, rx: k.rxFrames, tx: k.txFrames };
        }
    }
    const toggle = (p) => run(() => c.setPortAdmin({ name: p.name, up: !p.adminUp }), `${p.name} admin ${p.adminUp ? "down" : "up"}`).then(ports.refresh);
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Ports" }), _jsx("span", { className: "muted", children: "Setting a port down drops carrier on its veth peer, which is how the DUT sees a link failure." })] }), _jsx(ErrorBox, { error: ports.error }), !ports.loading && !ports.data?.ports.length ? (_jsx(Empty, { title: "No data ports", children: "The agent adopts eth1, eth2, \u2026 as containerlab attaches them." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Port" }), _jsx("th", { children: "Admin" }), _jsx("th", { children: "Oper" }), _jsx("th", { children: "MAC" }), _jsx("th", { className: "num", children: "MTU" }), _jsx("th", { className: "num", children: "RX frames" }), _jsx("th", { className: "num", children: "RX pps" }), _jsx("th", { className: "num", children: "TX frames" }), _jsx("th", { className: "num", children: "TX pps" }), _jsx("th", { className: "num", children: "TX errors" }), _jsx("th", { className: "num", title: "Frames no device matched (wrong VLAN or unicast MAC)", children: "Unmatched" })] }) }), _jsx("tbody", { children: ports.data?.ports.map((p) => {
                                const k = p.counters;
                                const r = rates.current[p.name];
                                return (_jsxs("tr", { children: [_jsx("td", { className: "mono strong", children: p.name }), _jsx("td", { children: _jsxs("button", { className: `toggle ${p.adminUp ? "on" : ""}`, disabled: busy, onClick: () => toggle(p), "aria-pressed": p.adminUp, title: `Set admin ${p.adminUp ? "down" : "up"}`, children: [_jsx("span", { className: "toggle-knob" }), _jsx("span", { children: p.adminUp ? "up" : "down" })] }) }), _jsxs("td", { children: [_jsx(Dot, { health: p.operUp ? "ok" : "bad" }), " ", p.operUp ? "up" : "down"] }), _jsx("td", { className: "mono", children: p.mac }), _jsx("td", { className: "num mono", children: p.mtu }), _jsx("td", { className: "num mono", children: k?.rxFrames.toLocaleString() }), _jsx("td", { className: "num mono", children: fmtRate(r?.rx ?? 0) }), _jsx("td", { className: "num mono", children: k?.txFrames.toLocaleString() }), _jsx("td", { className: "num mono", children: fmtRate(r?.tx ?? 0) }), _jsx("td", { className: `num mono ${k?.txErrors ? "bad-text" : ""}`, children: k?.txErrors.toLocaleString() }), _jsx("td", { className: "num mono", children: k?.rxUnmatched.toLocaleString() })] }, p.name));
                            }) })] }) }))] }));
}
