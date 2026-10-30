import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { ValueView, fmtScalar } from "../components/ValueView";
import { registerView } from "./registry";
const leased = (state) => state === "bound" || state === "renewing" || state === "rebinding";
// Waiting for a lease is a warning: in a test, a client without one is
// what to look at.
function idle(state, why) {
    if (state === "released")
        return { health: "idle", text: "released" };
    return { health: "warn", text: why ? `${state} · ${why}` : state };
}
function summary4(st) {
    const s = st;
    if (!leased(s.state) || !s.address)
        return idle(s.state, s.last_nak !== undefined ? `NAK ${s.last_nak}`.trim() : undefined);
    const tail = s.state === "bound" ? "" : ` · ${s.state}`;
    return { health: "ok", text: `${s.address}${s.gateway ? ` · gw ${s.gateway}` : ""}${tail}` };
}
function summary6(st) {
    const s = st;
    const got = [...(s.addresses ?? []), ...(s.prefixes ?? [])].map((b) => b.prefix);
    if (!leased(s.state) || !got.length)
        return idle(s.state);
    const missing = [s.ia_na_status, s.ia_pd_status].filter(Boolean);
    const tail = s.state === "bound" ? "" : ` · ${s.state}`;
    return { health: missing.length ? "warn" : "ok", text: [...got, ...missing].join(" · ") + tail };
}
registerView("dhcp", { summary: summary4 });
registerView("dhcpv6", { summary: summary6 });
const pausedText = (s) => (s.paused_for_s ? `paused for ${Math.ceil(s.paused_for_s)} s` : "paused");
function summaryServer4(st) {
    const s = st;
    const text = (s.pools ?? []).map((p) => `${p.subnet} ${p.bound}/${p.size}`).join(" · ");
    return s.paused ? { health: "warn", text: `${pausedText(s)} · ${text}` } : { health: "ok", text };
}
function summaryServer6(st) {
    const s = st;
    const text = (s.pools ?? []).map((p) => `${p.link} ${p.addresses_bound} addr, ${p.prefixes_bound} pd`).join(" · ");
    return s.paused ? { health: "warn", text: `${pausedText(s)} · ${text}` } : { health: "ok", text };
}
// relay4Text is how a DHCPv4 lease was relayed, in one line.
function relay4Text(r) {
    if (!r)
        return "direct";
    const parts = [`${r.giaddr} (hops ${r.hops})`];
    if (r.circuit_id)
        parts.push(`circuit ${r.circuit_id}`);
    if (r.remote_id)
        parts.push(`remote ${r.remote_id}`);
    if (r.link_selection)
        parts.push(`link ${r.link_selection}`);
    if (r.subscriber_id)
        parts.push(`subscriber ${r.subscriber_id}`);
    for (const o of r.other_suboptions ?? [])
        parts.push(o);
    return parts.join(" · ");
}
// A DHCPv6 binding's relays, the one nearest the client first.
function hopsText(hs) {
    if (!hs?.length)
        return ["direct"];
    return [...hs].reverse().map((h) => {
        const parts = [`link ${h.link_address}`];
        if (h.interface_id)
            parts.push(`interface ${h.interface_id}`);
        if (h.remote_id)
            parts.push(`remote ${h.remote_id}`);
        if (h.subscriber_id)
            parts.push(`subscriber ${h.subscriber_id}`);
        if (h.client_link_layer_address)
            parts.push(`client ${h.client_link_layer_address}`);
        for (const o of h.other_options ?? [])
            parts.push(o);
        return parts.join(" · ");
    });
}
const secsText = (s) => `${Math.round(s)} s`;
function Counters({ c }) {
    return _jsx(ValueView, { value: { counters: c } });
}
function Server4Status({ status }) {
    const s = status;
    const leases = s.leases ?? [];
    return (_jsxs("div", { children: [s.paused && _jsxs("p", { className: "warn-text", children: [pausedText(s), ": requests are dropped"] }), _jsx("h4", { children: "Pools" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "subnet" }), _jsx("th", { children: "range" }), _jsx("th", { children: "size" }), _jsx("th", { children: "bound" }), _jsx("th", { children: "offered" })] }) }), _jsx("tbody", { children: (s.pools ?? []).map((p) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: p.subnet }), _jsx("td", { className: "mono", children: p.range }), _jsx("td", { className: "mono", children: p.size }), _jsx("td", { className: "mono", children: p.bound }), _jsx("td", { className: "mono", children: p.offered })] }, p.subnet))) })] }) }), _jsxs("h4", { children: ["Leases (", leases.length, ")"] }), leases.length > 0 && (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "address" }), _jsx("th", { children: "state" }), _jsx("th", { children: "client" }), _jsx("th", { children: "expires in" }), _jsx("th", { children: "relayed by" })] }) }), _jsx("tbody", { children: leases.map((l) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: l.address }), _jsx("td", { children: l.state }), _jsxs("td", { className: "mono", children: [l.mac, l.hostname && _jsx("div", { className: "muted", children: l.hostname })] }), _jsx("td", { className: "mono", children: secsText(l.expires_in_s) }), _jsx("td", { className: "mono", children: relay4Text(l.relay) })] }, l.address))) })] }) })), _jsx(Counters, { c: s.counters ?? {} })] }));
}
function Server6Status({ status }) {
    const s = status;
    const bs = s.bindings ?? [];
    return (_jsxs("div", { children: [s.paused && _jsxs("p", { className: "warn-text", children: [pausedText(s), ": requests are dropped"] }), _jsx("dl", { className: "kv", children: _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "server DUID" }), _jsx("dd", { className: "mono", children: s.duid })] }) }), _jsx("h4", { children: "Pools" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "link" }), _jsx("th", { children: "addresses" }), _jsx("th", { children: "bound" }), _jsx("th", { children: "prefixes" }), _jsx("th", { children: "delegated" })] }) }), _jsx("tbody", { children: (s.pools ?? []).map((p) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: p.link }), _jsx("td", { className: "mono", children: p.addresses || "—" }), _jsx("td", { className: "mono", children: p.addresses_bound }), _jsx("td", { className: "mono", children: p.prefixes || "—" }), _jsx("td", { className: "mono", children: p.prefixes_bound })] }, p.link))) })] }) }), _jsxs("h4", { children: ["Bindings (", bs.length, ")"] }), bs.length > 0 && (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "address or prefix" }), _jsx("th", { children: "state" }), _jsx("th", { children: "client DUID \u00B7 IAID" }), _jsx("th", { children: "valid for" }), _jsx("th", { children: "relayed by (nearest the client first)" })] }) }), _jsx("tbody", { children: bs.map((b) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: b.prefix }), _jsx("td", { children: b.state }), _jsxs("td", { className: "mono", children: [b.duid, " \u00B7 ", fmtScalar(b.iaid, "iaid")] }), _jsx("td", { className: "mono", children: secsText(b.valid_in_s) }), _jsx("td", { className: "mono", children: hopsText(b.relays).map((h, i) => (_jsx("div", { children: h }, i))) })] }, b.prefix))) })] }) })), _jsx(Counters, { c: s.counters ?? {} })] }));
}
registerView("dhcp-server", { summary: summaryServer4, Status: Server4Status });
registerView("dhcpv6-server", { summary: summaryServer6, Status: Server6Status });
