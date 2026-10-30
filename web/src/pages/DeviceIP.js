import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useState } from "react";
import { client, usePoll } from "../api";
import { ErrorBox, Modal, useBusy } from "../ui";
const fmtAge = (ms) => {
    const s = Number(ms) / 1000;
    return s < 60 ? `${s.toFixed(0)}s` : s < 3600 ? `${(s / 60).toFixed(0)}m` : `${(s / 3600).toFixed(1)}h`;
};
// DeviceIP shows a device's effective addresses, neighbor cache and IP
// counters, and resolves addresses on demand.
export function DeviceIP({ agentId, device, onClose }) {
    const c = client(agentId);
    const ip = usePoll(() => c.getDeviceIP({ name: device }), [agentId, device], 2000);
    const [target, setTarget] = useState("");
    const [result, setResult] = useState();
    const { busy, run } = useBusy();
    const m = ip.data;
    const counters = Object.entries(m?.counters ?? {})
        .filter(([, v]) => v > 0n)
        .sort(([a], [b]) => a.localeCompare(b));
    const resolve = () => run(() => c.resolveNeighbor({ device, ip: target.trim() })).then((r) => {
        if (!r)
            return setResult(undefined);
        const via = r.nextHop !== target.trim() ? ` via ${r.nextHop}` : "";
        setResult(`${target.trim()}${via} is at ${r.mac} (${r.cached ? "cached" : (Number(r.rttUs) / 1000).toFixed(2) + " ms"})`);
        ip.refresh();
    });
    return (_jsxs(Modal, { title: `IP on ${device}`, onClose: onClose, wide: true, children: [_jsx(ErrorBox, { error: ip.error }), m && (_jsxs(_Fragment, { children: [_jsxs("dl", { className: "kv ip-kv", children: [_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "addresses" }), _jsx("dd", { className: "mono", children: m.addresses.map((a) => (_jsx("div", { children: a }, a))) })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "gateways" }), _jsx("dd", { className: "mono", children: [m.gateway4, m.gateway6].filter(Boolean).join(", ") || "—" })] })] }), _jsxs("form", { className: "resolve-row", onSubmit: (e) => {
                            e.preventDefault();
                            resolve();
                        }, children: [_jsx("input", { className: "input mono", value: target, onChange: (e) => setTarget(e.target.value), placeholder: "IP to resolve (off-link: via gateway)", "aria-label": "IP to resolve" }), _jsx("button", { className: "btn", disabled: busy || !target.trim(), children: busy ? "Resolving…" : "Resolve" }), _jsx("button", { type: "button", className: "btn danger-ghost", disabled: busy, onClick: () => run(() => c.clearNeighbors({ device }), "Neighbor cache cleared").then(ip.refresh), children: "Clear cache" })] }), result && _jsx("div", { className: "result mono", children: result }), _jsx("h4", { children: "Neighbors" }), m.neighbors.length === 0 ? (_jsx("div", { className: "muted", children: "None learned yet." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "IP" }), _jsx("th", { children: "MAC" }), _jsx("th", { children: "State" }), _jsx("th", { children: "Age" })] }) }), _jsx("tbody", { children: m.neighbors.map((n) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: n.ip }), _jsx("td", { className: "mono", children: n.mac || "—" }), _jsx("td", { children: _jsx("span", { className: n.state === "reachable" ? "pill pill-on" : n.state === "incomplete" ? "pill pill-warn" : "pill", children: n.state }) }), _jsx("td", { className: "mono", children: n.mac ? fmtAge(n.ageMs) : "—" })] }, n.ip))) })] }) })), _jsx("h4", { children: "Counters" }), counters.length === 0 ? (_jsx("div", { className: "muted", children: "No IP traffic yet." })) : (_jsx("div", { className: "counters", children: counters.map(([k, v]) => (_jsxs("div", { className: "counter", children: [_jsx("span", { className: "counter-k", children: k.replace(/_/g, " ") }), _jsx("span", { className: k === "address_conflicts" ? "counter-v warn-text" : "counter-v", children: v.toString() })] }, k))) }))] }))] }));
}
