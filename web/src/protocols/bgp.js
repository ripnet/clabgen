import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useState } from "react";
import { client, usePoll } from "../api";
import { registerView, restartNote } from "./registry";
const as = (s) => s;
const sum = (m) => Object.values(m ?? {}).reduce((a, b) => a + b, 0);
const famShort = (f) => ({
    "ipv4-unicast": "v4",
    "ipv6-unicast": "v6",
    "ipv4-vpn": "vpnv4",
    "ipv6-vpn": "vpnv6",
})[f] ?? f;
const perFam = (m, fams) => fams.map((f) => `${famShort(f)} ${m[f] ?? 0}`).join(" · ");
function summary(st) {
    const s = as(st);
    const ps = s.peers ?? [];
    const up = ps.filter((p) => p.state === "established").length;
    const rx = ps.reduce((a, p) => a + sum(p.received), 0);
    const text = `AS ${s.local_as} · ${up}/${ps.length} peers up · ${rx} received · ${sum(s.advertised)} advertised`;
    if (!ps.length)
        return { health: "idle", text };
    return { health: up === ps.length ? "ok" : up ? "warn" : "bad", text };
}
function RIB({ agentId, name, peer, }) {
    const [filter, setFilter] = useState("");
    const d = usePoll(async () => {
        if (!agentId || !name)
            return undefined;
        const r = await client(agentId).runProtocolAction({
            name,
            action: "rib",
            args: { peer, prefix: filter, limit: 200 },
        });
        return r.result;
    }, [agentId, name, peer, filter], 3000);
    return (_jsxs("div", { children: [_jsx("input", { className: "input search mono", placeholder: "within a prefix (198.51.100.0/22), or text in VPN / EVPN routes (65100:500, mac:02:00)", value: filter, onChange: (e) => setFilter(e.target.value.trim()) }), d.error && _jsx("div", { className: "error-text", children: d.error }), d.data && (_jsxs(_Fragment, { children: [_jsxs("div", { className: "muted small-note", children: [d.data.total, " route", d.data.total === 1 ? "" : "s", d.data.total > d.data.routes.length
                                ? ` (first ${d.data.routes.length})`
                                : ""] }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Prefix" }), _jsx("th", { children: "Next hop" }), _jsx("th", { children: "AS path" }), _jsx("th", { children: "Attributes" })] }) }), _jsx("tbody", { children: d.data.routes.map((r) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: r.prefix }), _jsx("td", { className: "mono", children: r.next_hop }), _jsx("td", { className: "mono", children: r.as_path || "(empty)" }), _jsx("td", { className: "mono small-note", children: [
                                                    r.origin && r.origin !== "igp" && r.origin,
                                                    r.med !== undefined && `med ${r.med}`,
                                                    r.local_pref !== undefined && `lp ${r.local_pref}`,
                                                    r.communities?.join(" "),
                                                    r.large_communities?.join(" "),
                                                    r.ext_communities?.join(" "),
                                                    r.other_attributes?.join(" "),
                                                ]
                                                    .filter(Boolean)
                                                    .join(" · ") })] }, `${r.family}/${r.prefix}`))) })] }) })] }))] }));
}
function Status({ status, agentId, name, }) {
    const s = as(status);
    const [open, setOpen] = useState();
    return (_jsxs("div", { children: [_jsxs("dl", { className: "kv", children: [_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "AS \u00B7 router ID" }), _jsxs("dd", { className: "mono", children: [s.local_as, " \u00B7 ", s.router_id] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "advertising" }), _jsx("dd", { className: "mono", children: Object.entries(s.advertised ?? {})
                                    .map(([f, n]) => `${famShort(f)} ${n}`)
                                    .join(" · ") || "nothing" })] })] }), _jsx("h4", { children: "Peers" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense hover", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Peer" }), _jsx("th", { children: "State" }), _jsx("th", { children: "Received" }), _jsx("th", { children: "Advertised" }), _jsx("th", { children: "Details" })] }) }), _jsx("tbody", { children: (s.peers ?? []).map((p) => (_jsxs("tr", { onClick: () => setOpen(open === p.address ? undefined : p.address), children: [_jsxs("td", { className: "mono", children: [p.address, _jsxs("div", { className: "muted small-note", children: ["AS ", p.remote_as, " \u00B7 ", p.type, p.description ? ` · ${p.description}` : ""] })] }), _jsxs("td", { children: [_jsx("span", { className: p.state === "established"
                                                    ? "pill pill-on"
                                                    : p.state === "idle" || p.state === "active"
                                                        ? "pill"
                                                        : "pill pill-warn", children: p.state }), p.flapping && _jsx("span", { className: "tag tag-warn", children: "flapping" }), _jsxs("div", { className: "muted small-note", children: ["since ", new Date(p.since).toLocaleTimeString()] })] }), _jsxs("td", { className: "mono", children: [perFam(p.received, p.families), p.end_of_rib_received?.length ? (_jsxs("div", { className: "muted small-note", children: ["EoR ", p.end_of_rib_received.map(famShort).join(", ")] })) : null] }), _jsx("td", { className: "mono", children: perFam(p.advertised, p.families) }), _jsxs("td", { className: "mono small-note", children: [p.state === "established" &&
                                                `id ${p.remote_id} · hold ${p.hold_time} s · from ${p.local_address}`, p.bfd && (_jsxs("div", { children: ["BFD ", p.bfd.state, p.bfd.state === "up" && p.bfd.detect_ms ? ` (detect ${p.bfd.detect_ms} ms)` : "", p.bfd.state !== "up" && p.bfd.remote_state ? ` · remote ${p.bfd.remote_state}` : "", p.bfd.silent ? " · silenced" : ""] })), _jsxs("div", { className: "muted", children: [p.ups, " up \u00B7 ", p.downs, " down", p.last_down ? ` · last down: ${p.last_down}` : "", p.state !== "established" && p.last_error
                                                        ? ` · ${p.last_error}`
                                                        : ""] })] })] }, p.address))) })] }) }), open && (_jsxs(_Fragment, { children: [_jsxs("h4", { children: ["Routes from ", open] }), _jsx(RIB, { agentId: agentId, name: name, peer: open })] }))] }));
}
// applyNote mirrors the agent's in-place config changes
// (protocols/bgp/reconfig.go): which peers' sessions reset, if any.
function applyNote(was, now) {
    const bfd = (c) => (c.bfd ?? {});
    if (was.local_as !== now.local_as || (was.router_id ?? "") !== (now.router_id ?? "") || JSON.stringify(was.bfd ?? {}) !== JSON.stringify(now.bfd ?? {}))
        return restartNote;
    const key = (p, c) => JSON.stringify([p.remote_as, p.local_address ?? "", (p.families ?? []).join(","), p.hold_time || c.hold_time, p.ttl ?? 0, p.bfd ?? !!bfd(c).enabled]);
    const peers = (c) => new Map((c.peers ?? []).map((p) => [p.address, p]));
    const before = peers(was);
    const reset = [];
    const removed = [];
    for (const [a, p] of peers(now)) {
        const o = before.get(a);
        if (o && key(o, was) !== key(p, now))
            reset.push(a);
    }
    for (const a of before.keys())
        if (!peers(now).has(a))
            removed.push(a);
    const parts = [];
    if (reset.length)
        parts.push(`resets the session with ${reset.join(", ")} (session settings changed)`);
    if (removed.length)
        parts.push(`closes the session with ${removed.join(", ")} (removed)`);
    if (!parts.length)
        return { restart: false, text: "Applies in place: routes are advertised and withdrawn as updates, and no session goes down." };
    return { restart: true, text: `Applying ${parts.join(" and ")}. Other sessions stay up; routes change as updates.` };
}
registerView("bgp", { summary, Status, applyNote });
