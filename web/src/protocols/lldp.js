import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { registerView, restartNote } from "./registry";
const as = (s) => s;
const who = (n) => n.system_name || n.chassis_id;
function extras(n) {
    const out = [];
    if (n.capabilities?.length)
        out.push(`caps ${n.capabilities.map((c) => (n.enabled_capabilities?.includes(c) ? c : `(${c})`)).join(",")}`);
    if (n.management_addresses?.length)
        out.push(`mgmt ${n.management_addresses.join(", ")}`);
    if (n.port_vlan_id)
        out.push(`PVID ${n.port_vlan_id}`);
    if (n.vlan_names?.length)
        out.push(`VLANs ${n.vlan_names.map((v) => `${v.vid}:${v.name}`).join(", ")}`);
    if (n.max_frame_size)
        out.push(`max frame ${n.max_frame_size}`);
    if (n.mac_phy)
        out.push(`MAU ${n.mac_phy.mau_type}${n.mac_phy.autoneg_enabled ? " autoneg" : ""}`);
    if (n.link_aggregation)
        out.push(`LAG ${n.link_aggregation.enabled ? "member" : n.link_aggregation.capable ? "capable" : "no"}${n.link_aggregation.aggregated_port_id ? ` #${n.link_aggregation.aggregated_port_id}` : ""}`);
    if (n.other_tlvs?.length)
        out.push(`${n.other_tlvs.length} other TLV${n.other_tlvs.length > 1 ? "s" : ""}`);
    return out;
}
function summary(st) {
    const s = as(st);
    const ns = s.neighbors ?? [];
    const tx = s.transmit === "on" ? "" : ` · tx ${s.transmit}`;
    if (!ns.length)
        return { health: s.mode === "tx-only" || s.mode === "disabled" ? "idle" : "warn", text: `no neighbors${tx}` };
    return {
        health: s.transmit === "on" || s.mode === "rx-only" ? "ok" : "warn",
        text: ns.map((n) => `${who(n)} ${n.port_id}`).join(", ") + tx,
    };
}
function Status({ status }) {
    const s = as(status);
    const l = s.local;
    const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) => x.localeCompare(y));
    return (_jsxs("div", { children: [_jsxs("dl", { className: "kv", children: [_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "mode \u00B7 scope" }), _jsxs("dd", { className: "mono", children: [s.mode, " \u00B7 ", s.dest] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "transmit" }), _jsxs("dd", { className: "mono", children: [s.transmit === "on" ? `on · next in ${(s.next_tx_s ?? 0).toFixed(0)} s` : _jsx("span", { className: "warn-text", children: s.transmit }), s.paused_until && ` until ${new Date(s.paused_until).toLocaleTimeString()}`] })] }), l && (_jsxs("div", { className: "kv-row kv-wide", children: [_jsx("dt", { children: "we advertise" }), _jsxs("dd", { className: "mono", children: [l.system_name, " \u00B7 chassis ", l.chassis_id_subtype, " ", l.chassis_id, " \u00B7 port ", l.port_id_subtype, " ", l.port_id, " \u00B7 TTL ", l.ttl, extras(l).length > 0 && _jsx("div", { className: "muted", children: extras(l).join(" · ") })] })] }))] }), _jsx("h4", { children: "Neighbors" }), (s.neighbors ?? []).length === 0 ? (_jsx("div", { className: "muted", children: "None heard." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "System" }), _jsx("th", { children: "Chassis ID" }), _jsx("th", { children: "Port" }), _jsx("th", { className: "num", children: "TTL" }), _jsx("th", { children: "Details" })] }) }), _jsx("tbody", { children: s.neighbors.map((n) => (_jsxs("tr", { children: [_jsxs("td", { className: "cell-wrap", children: [n.system_name || "—", n.system_description && _jsx("div", { className: "muted small-note", children: n.system_description })] }), _jsxs("td", { className: "mono", children: [n.chassis_id, _jsx("div", { className: "muted small-note", children: n.chassis_id_subtype })] }), _jsxs("td", { className: "mono", children: [n.port_id, _jsxs("div", { className: "muted small-note", children: [n.port_id_subtype, n.port_description ? ` · ${n.port_description}` : ""] })] }), _jsxs("td", { className: "num mono", children: [Math.max(0, n.ttl_remaining_s).toFixed(0), "/", n.ttl, " s"] }), _jsxs("td", { className: "mono small-note", children: [extras(n).join(" · ") || "—", n.changes > 0 && _jsxs("div", { className: "muted", children: [n.changes, " change", n.changes > 1 ? "s" : "", " \u00B7 from ", n.mac] })] })] }, `${n.chassis_id}/${n.port_id}`))) })] }) })), counters.length > 0 && (_jsx("div", { className: "muted mono small-note", children: counters.map(([k, v]) => `${k} ${v}`).join(" · ") }))] }));
}
// Only a new destination or mode restarts (protocols/lldp/lldp.go).
registerView("lldp", {
    summary,
    Status,
    applyNote: (was, now) => was.dest === now.dest && was.mode === now.mode ? { restart: false, text: "Applies in place: the next LLDPDU, sent at once, carries it; neighbors keep us." } : restartNote,
});
