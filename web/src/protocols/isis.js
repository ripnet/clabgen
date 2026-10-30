import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { Fragment, useState } from "react";
import { client, usePoll } from "../api";
import { inPlaceNote, registerView } from "./registry";
const ranges = (rs) => (rs ?? []).map((g) => `${g.base}–${g.base + g.size - 1}`).join(", ");
// Admin group bit mask → "3,5".
const groups = (m) => Array.from({ length: 32 }, (_, i) => i)
    .filter((i) => ((m ?? 0) >>> i) & 1)
    .join(",");
const fadText = (f) => `algo ${f.algorithm}: ${f.metric_type}, prio ${f.priority}` +
    (f.exclude_any ? `, exclude ${groups(f.exclude_any)}` : "") +
    (f.include_any ? `, include-any ${groups(f.include_any)}` : "") +
    (f.include_all ? `, include-all ${groups(f.include_all)}` : "");
const mtTag = (mt) => (mt ? _jsx("span", { className: "tag", children: { 2: "MT ipv6", 3: "MT mcast4", 4: "MT mcast6" }[mt] ?? `MT ${mt}` }) : null);
function LSPDetailRow({ agentId, name, row, nameOf }) {
    const d = usePoll(async () => {
        if (!agentId || !name)
            return undefined;
        const r = await client(agentId).runProtocolAction({ name, action: "lsp", args: { id: row.lsp_id, level: row.level } });
        return r.result;
    }, [agentId, name, row.lsp_id, row.level], 3000);
    const l = d.data;
    if (d.error)
        return _jsx("div", { className: "error-text", children: d.error });
    if (!l)
        return _jsx("div", { className: "muted", children: "Loading\u2026" });
    return (_jsxs("div", { className: "lsp-grid", children: [_jsxs("div", { children: [_jsx("div", { className: "field-label", children: "neighbors" }), (l.neighbors ?? []).map((n) => (_jsxs("div", { className: "mono", children: [nameOf(n.id), " ", _jsxs("span", { className: "muted", children: ["metric ", n.metric] }), mtTag(n.mt), n.adj_sids?.map((a, i) => (_jsxs("span", { className: "tag", title: a.neighbor ? `LAN-Adj-SID for ${nameOf(a.neighbor + ".00")}` : "Adj-SID", children: [a.ipv6 ? "v6 " : "", "adj ", a.label, a.neighbor ? ` → ${nameOf(a.neighbor + ".00").replace(/\.00$/, "")}` : ""] }, i))), n.link_attributes && (_jsx("span", { className: "tag", title: "Application-specific link attributes (flex-algo)", children: [
                                    n.link_attributes.te_metric !== undefined && `te ${n.link_attributes.te_metric}`,
                                    n.link_attributes.delay_us !== undefined && `${n.link_attributes.delay_us} µs`,
                                    n.link_attributes.admin_groups && `groups ${groups(n.link_attributes.admin_groups)}`,
                                ]
                                    .filter(Boolean)
                                    .join(" · ") || "ASLA" })), n.end_x_sids?.map((x) => (_jsx("span", { className: "tag", title: `SRv6 ${x.behavior}${x.neighbor ? ` toward ${nameOf(x.neighbor + ".00")}` : ""}`, children: x.sid }, x.sid + (x.neighbor ?? ""))))] }, `${n.id}/${n.mt ?? 0}`))), !l.neighbors?.length && _jsx("div", { className: "muted", children: "none" }), l.router_id && _jsxs("div", { className: "mono muted", children: ["router ID ", l.router_id] }), l.topologies && _jsxs("div", { className: "mono muted", children: ["topologies: ", l.topologies.join(", ")] }), l.sr && (_jsxs("div", { className: "mono muted", children: ["SR:", l.sr.srgb?.length ? ` SRGB ${ranges(l.sr.srgb)} · MPLS ${[l.sr.mpls_ipv4 && "v4", l.sr.mpls_ipv6 && "v6"].filter(Boolean).join("/")} ·` : "", l.sr.srlb?.length ? ` SRLB ${ranges(l.sr.srlb)} ·` : "", l.sr.srv6 ? " SRv6 ·" : "", " algorithms ", (l.sr.algorithms ?? [0]).join(", "), " \u00B7 router ID ", l.sr.router_id, l.sr.flex_algo_definitions?.map((f) => _jsxs("div", { children: ["FAD ", fadText(f)] }, f.algorithm))] })), l.srv6_locators?.map((x) => (_jsxs("div", { className: "mono muted", children: ["locator ", x.locator, x.algorithm ? ` (algo ${x.algorithm})` : "", x.end_sids?.map((e) => ` · ${e.sid} ${e.behavior}`).join("")] }, `${x.locator}/${x.algorithm ?? 0}`))), l.sid_bindings?.map((b) => (_jsxs("div", { className: "mono muted", children: ["mapping ", b.prefix, " \u00D7", b.range, " \u2192 SID ", b.index, b.algorithm ? ` (algo ${b.algorithm})` : ""] }, b.prefix))), l.addresses && _jsx("div", { className: "mono muted", children: l.addresses.join(", ") })] }), _jsxs("div", { children: [_jsxs("div", { className: "field-label", children: ["prefixes (", l.prefixes?.length ?? 0, ")"] }), _jsxs("div", { className: "pfx-list mono", children: [(l.prefixes ?? []).slice(0, 500).map((p) => (_jsxs("div", { children: [p.prefix, " ", _jsx("span", { className: "muted", children: p.metric }), p.down && _jsx("span", { className: "tag", children: "down" }), mtTag(p.mt), p.sid && (_jsxs("span", { className: "tag", title: `Prefix-SID flags ${p.sid.flags || "none"}`, children: ["SID ", p.sid.index ?? `label ${p.sid.label}`, p.sid.flags ? ` ${p.sid.flags}` : ""] })), p.flex_sids?.map((f) => (_jsxs("span", { className: "tag", title: `Prefix-SID for flex-algo ${f.algorithm}`, children: ["algo ", f.algorithm, " SID ", f.index ?? `label ${f.label}`] }, f.algorithm)))] }, `${p.prefix}/${p.mt ?? 0}`))), (l.prefixes?.length ?? 0) > 500 && _jsxs("div", { className: "muted", children: ["\u2026 ", l.prefixes.length - 500, " more"] })] })] })] }));
}
const as = (s) => s;
// AdjExtras shows an adjacency's BFD sessions and restart state.
function AdjExtras({ a }) {
    return (_jsxs(_Fragment, { children: [(a.bfd ?? []).map((b) => (_jsxs("span", { className: b.state === "up" ? "tag" : "tag tag-warn", title: `BFD with ${b.address}${b.detect_ms ? `, detection ${b.detect_ms} ms` : ""}`, children: ["BFD ", b.family, " ", b.state] }, b.family))), a.waiting_for_bfd && _jsx("span", { className: "tag tag-warn", children: "waiting for BFD" }), a.restarting && _jsx("span", { className: "tag tag-warn", children: "restarting (helping)" }), a.suppressed && _jsx("span", { className: "tag", children: "SA: not advertised" })] }));
}
const multi = (s) => (s.interfaces?.length ?? 1) > 1;
// lspCount is the live LSDB size; compact (list) statuses carry only the
// total, not the rows.
const lspCount = (s) => (s.lsdb.length ? s.lsdb.filter((l) => l.lifetime > 0).length : (s.lsdb_total ?? 0));
function summary(st) {
    const s = as(st);
    if (multi(s)) {
        const ifs = s.interfaces;
        const up = (s.adjacencies ?? []).filter((x) => x.state === "up").length;
        const active = ifs.filter((i) => !i.passive);
        const live = active.filter((i) => i.adjacencies_up > 0).length;
        const tail = `${lspCount(s)} LSPs${s.overload ? " · overload" : ""}${s.graceful_restart?.restarting ? " · restarting" : ""}`;
        if (!up)
            return { health: "bad", text: `no adjacencies on ${ifs.length} interfaces` };
        return {
            health: live < active.length || s.overload ? "warn" : "ok",
            text: `${up} neighbor${up > 1 ? "s" : ""} on ${live}/${active.length} interfaces · ${tail}`,
        };
    }
    if (s.lan) {
        const up = (s.adjacencies ?? []).filter((x) => x.state === "up").length;
        const disNames = s.lan.map((l) => (l.we_are_dis ? "us" : hostOf(s, l.dis))).join("/");
        const tail = `${lspCount(s)} LSPs${s.overload ? " · overload" : ""}`;
        if (up)
            return { health: s.overload ? "warn" : "ok", text: `${up} neighbor${up > 1 ? "s" : ""} · DIS ${disNames} · ${tail}` };
        return { health: "bad", text: s.flapping ? "down (flap)" : "no LAN neighbors" };
    }
    const a = s.adjacency;
    const who = a.hostname || a.neighbor;
    const tail = `${lspCount(s)} LSPs${s.overload ? " · overload" : ""}`;
    if (a.state === "up")
        return { health: s.overload ? "warn" : "ok", text: `up with ${who} · ${tail}` };
    if (a.state === "initializing")
        return { health: "warn", text: `initializing with ${who}` };
    return { health: "bad", text: s.flapping ? "down (flap)" : "adjacency down" };
}
function hostOf(s, sys) {
    const l = s.lsdb.find((x) => x.lsp_id.startsWith(sys + ".00") && x.hostname);
    return l?.hostname ?? sys;
}
function Interfaces({ s }) {
    return (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Interface" }), _jsx("th", { children: "Circuit" }), _jsx("th", { className: "num", children: "Metric" }), _jsx("th", { className: "num", children: "Up" }), _jsx("th", { children: "Addresses" }), _jsx("th", { children: "Hello auth" })] }) }), _jsx("tbody", { children: (s.interfaces ?? []).map((i) => (_jsxs("tr", { children: [_jsxs("td", { className: "mono", children: [i.name, !i.attached && _jsx("span", { className: "tag tag-warn", title: i.error, children: "detached" }), i.passive && _jsx("span", { className: "tag", children: "passive" }), i.flapping && _jsx("span", { className: "tag tag-warn", children: "flapping" })] }), _jsxs("td", { children: [i.circuit, i.circuit === "broadcast" && _jsxs("span", { className: "muted", children: [" \u00B7 prio ", i.priority ?? 0] })] }), _jsx("td", { className: "num mono", children: i.metric }), _jsx("td", { className: "num mono", children: i.passive ? "—" : i.adjacencies_up }), _jsx("td", { className: "mono", children: i.addresses?.join(", ") || (i.attached ? "—" : i.error) }), _jsx("td", { className: "mono", children: i.hello_auth })] }, i.name))) })] }) }));
}
function LANAdjacencies({ s }) {
    const m = multi(s);
    return (_jsxs(_Fragment, { children: [(s.lan ?? []).map((l) => (_jsxs("div", { className: "flow-path mono small-note", children: [m && `${l.interface} `, "L", l.level, " DIS: ", _jsx("b", { children: l.we_are_dis ? `us (${s.hostname})` : hostOf(s, l.dis) }), " \u00B7 LAN ID ", l.lan_id, " \u00B7 our priority ", l.priority, " \u00B7 ", l.elections, " election", l.elections === 1 ? "" : "s"] }, `${l.interface}/${l.level}`))), (s.adjacencies ?? []).length === 0 ? (_jsx("div", { className: "muted", children: m ? "No neighbors heard on any interface." : "No neighbors heard on the LAN." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [m && _jsx("th", { children: "Interface" }), _jsx("th", { children: "Neighbor" }), _jsx("th", { children: "State" }), _jsx("th", { children: "Level" }), _jsx("th", { className: "num", children: "Priority" }), _jsx("th", { className: "num", children: "Hold" }), _jsx("th", { children: "MAC" }), _jsx("th", { children: "Addresses" })] }) }), _jsx("tbody", { children: (s.adjacencies ?? []).map((a) => (_jsxs("tr", { children: [m && _jsx("td", { className: "mono", children: a.interface }), _jsxs("td", { className: "mono", children: [a.hostname ? `${a.hostname} · ` : "", a.neighbor, a.dis && _jsx("span", { className: "tag", children: "DIS" }), _jsx(AdjExtras, { a: a })] }), _jsx("td", { children: _jsx("span", { className: a.state === "up" ? "pill pill-on" : "pill pill-warn", children: a.state }) }), _jsxs("td", { children: ["L", a.levels?.join("/L")] }), _jsx("td", { className: "num mono", children: a.priority }), _jsx("td", { className: "num mono", children: a.hold_remaining_s !== undefined ? `${Math.max(0, a.hold_remaining_s).toFixed(0)} s` : "—" }), _jsx("td", { className: "mono", children: a.mac }), _jsx("td", { className: "mono", children: a.addresses?.join(", ") })] }, `${a.interface}/${a.levels?.[0]}/${a.mac}`))) })] }) }))] }));
}
// Names LSP IDs by hostname where the LSDB knows one.
function namer(rows) {
    const names = new Map();
    for (const r of rows)
        if (r.hostname)
            names.set(r.lsp_id.slice(0, 14), r.hostname);
    return (id) => {
        const h = names.get(id.slice(0, 14));
        return h ? `${h}${id.slice(14)}` : id;
    };
}
function Status({ status, agentId, name: inst }) {
    const s = as(status);
    const a = s.adjacency;
    const [open, setOpen] = useState();
    const name = namer(s.lsdb);
    const pill = a.state === "up" ? "pill pill-on" : a.state === "initializing" ? "pill pill-warn" : "pill";
    const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) => x.localeCompare(y));
    return (_jsxs("div", { className: "isis", children: [_jsxs("dl", { className: "kv", children: [_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "system ID" }), _jsxs("dd", { className: "mono", children: [s.system_id, " (", s.hostname, ")"] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "level \u00B7 circuit" }), _jsxs("dd", { className: "mono", children: [s.level, " \u00B7 ", s.circuit] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "areas" }), _jsx("dd", { className: "mono", children: s.areas.join(", ") })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "metric \u00B7 overload" }), _jsxs("dd", { className: "mono", children: [s.metric, " \u00B7 ", s.overload ? _jsx("span", { className: "warn-text", children: "set" }) : "clear", s.overload_on_startup_until && _jsxs("span", { className: "muted", children: [" (on startup, until ", new Date(s.overload_on_startup_until).toLocaleTimeString(), ")"] })] })] }), s.inter_level && (_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: s.level === "L1L2" ? "between levels" : "attached routers" }), _jsx("dd", { className: "mono", children: s.level === "L1L2"
                                    ? `ATT ${s.inter_level.attached ? "set" : "clear"} · ${s.inter_level.to_l2} into L2 · ${s.inter_level.leaked_to_l1} leaked into L1`
                                    : s.inter_level.attached_routers.map((r) => hostOf(s, r)).join(", ") || "none (no way out of the area)" })] })), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "graceful restart \u00B7 BFD" }), _jsxs("dd", { className: "mono", children: [s.graceful_restart?.restarting ? (_jsxs("span", { className: "warn-text", children: ["restarting: ", s.graceful_restart.restarting.acked, " acked, ", s.graceful_restart.restarting.synced, " synced, ", s.graceful_restart.restarting.pending, " LSPs to go,", " ", s.graceful_restart.restarting.remaining_s, " s left"] })) : (`helper ${s.graceful_restart?.helper ? "on" : "off"}`), " ", "\u00B7 BFD ", s.bfd ? "on" : "off"] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "topologies" }), _jsx("dd", { className: "mono", children: (s.topologies ?? []).join(", ") })] }), s.segment_routing && (_jsxs("div", { className: "kv-row kv-wide", children: [_jsx("dt", { children: "segment routing" }), _jsxs("dd", { className: "mono", children: [s.segment_routing.mpls !== false && (_jsxs(_Fragment, { children: ["SRGB ", s.segment_routing.srgb_base, "\u2013", s.segment_routing.srgb_base + s.segment_routing.srgb_size - 1, s.segment_routing.srlb && ` · SRLB ${ranges([s.segment_routing.srlb])}`, (s.segment_routing.node_sids ?? []).map((n) => (_jsxs("div", { children: [n.prefix, " \u2192 index ", n.index, " (label ", n.label, ")", n.algorithm ? ` algo ${n.algorithm}` : "", n.flags ? ` ${n.flags}` : ""] }, `${n.prefix}/${n.algorithm ?? 0}`))), _jsxs("div", { className: "muted", children: ["adj SIDs:", " ", (s.segment_routing.adj_sids ?? []).map((a) => `${a.label}${a.ipv6 ? " v6" : ""} → ${hostOf(s, a.neighbor.slice(0, 14))}`).join(", ") || "none"] }), s.segment_routing.mappings?.map((b) => (_jsxs("div", { className: "muted", children: ["mapping ", b.prefix, " \u00D7", b.range, " \u2192 index ", b.index] }, b.prefix)))] })), s.segment_routing.flex_algos?.map((f) => (_jsxs("div", { className: "muted", children: ["flex-", fadText(f), f.participate_only ? " (participating; defined elsewhere)" : ""] }, f.algorithm))), s.segment_routing.srv6_sids?.map((x) => (_jsxs("div", { className: "muted", children: ["SRv6 ", x.sid, " ", x.behavior, x.locator ? ` (locator ${x.locator}${x.algorithm ? `, algo ${x.algorithm}` : ""})` : ` → ${hostOf(s, (x.neighbor ?? "").slice(0, 14))}`] }, x.sid)))] })] })), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "authentication" }), _jsxs("dd", { className: "mono", children: ["hellos ", s.hello_auth ?? "none", " \u00B7 LSPs ", s.lsp_auth ?? "none", Object.entries(s.counters ?? {}).some(([k, v]) => k.startsWith("rx_auth_failures") && v > 0) && _jsx("span", { className: "warn-text", children: " \u00B7 failures" })] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "advertised prefixes" }), _jsxs("dd", { className: "mono", children: [s.advertised_prefixes, " injected (+ connected", s.loopbacks?.length ? `, loopbacks ${s.loopbacks.join(", ")}` : "", ")"] })] })] }), multi(s) && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Interfaces" }), _jsx(Interfaces, { s: s })] })), _jsx("h4", { children: multi(s) ? "Adjacencies" : s.lan ? "LAN adjacencies" : "Adjacency" }), multi(s) || s.lan ? (_jsx(LANAdjacencies, { s: s })) : (_jsxs(_Fragment, { children: [_jsxs("div", { className: "flow-head", children: [_jsxs("div", { className: "flow-path mono", children: [_jsx("span", { className: pill, children: a.state }), " ", a.neighbor ? `${a.hostname ? a.hostname + " · " : ""}${a.neighbor}` : "no neighbor", a.levels && _jsxs("span", { className: "muted", children: [" \u00B7 L", a.levels.join("/L")] }), a.topologies && _jsxs("span", { className: "muted", children: [" \u00B7 ", a.topologies.join(", ")] }), _jsx(AdjExtras, { a: a })] }), _jsxs("div", { className: "muted", children: [a.hold_remaining_s !== undefined && `hold ${Math.max(0, a.hold_remaining_s).toFixed(0)} s · `, a.since && `since ${new Date(a.since).toLocaleTimeString()} · `, a.three_way ? "3-way" : "2-way", " \u00B7 ", a.transitions, " transitions", s.flapping && _jsx("span", { className: "warn-text", children: " \u00B7 flapping" })] })] }), a.addresses && a.addresses.length > 0 && _jsxs("div", { className: "muted mono small-note", children: ["neighbor addresses: ", a.addresses.join(", ")] })] })), s.topology && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Emulated topology" }), _jsxs("div", { className: "flow-path mono small-note", children: [s.topology.kind, " \u00B7 ", s.topology.nodes, " routers \u00B7 ", s.topology.links, " links \u00B7 ", s.topology.prefixes, " prefixes \u00B7 attached to us: ", s.topology.attached.join(", ") || "none"] }), (s.topology.down_nodes?.length || s.topology.down_links?.length || s.topology.overloaded?.length) ? (_jsxs("div", { className: "warn-box", children: [s.topology.down_nodes?.length ? _jsxs("div", { children: ["routers down: ", s.topology.down_nodes.join(", ")] }) : null, s.topology.down_links?.length ? _jsxs("div", { children: ["links down: ", s.topology.down_links.join(", ")] }) : null, s.topology.overloaded?.length ? _jsxs("div", { children: ["overloaded: ", s.topology.overloaded.join(", ")] }) : null] })) : (_jsx("div", { className: "muted small-note", children: "All simulated routers and links are up. Use the topology-link and topology-node actions to fail them." }))] })), _jsx("h4", { children: "Link-state database" }), s.lsdb_total !== undefined && s.lsdb_total > s.lsdb.length && (_jsxs("div", { className: "muted small-note", children: ["Showing ", s.lsdb.length, " of ", s.lsdb_total, " LSPs."] })), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense hover", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Level" }), _jsx("th", { children: "LSP" }), _jsx("th", { className: "num", children: "Seq" }), _jsx("th", { className: "num", children: "Lifetime" }), _jsx("th", { children: "Checksum" }), _jsx("th", { className: "num", children: "Neighbors" }), _jsx("th", { className: "num", children: "Prefixes" }), _jsx("th", { children: "Flags" })] }) }), _jsx("tbody", { children: s.lsdb.map((l) => {
                                const key = `${l.level}/${l.lsp_id}`;
                                const purged = l.lifetime === 0;
                                return (_jsxs(Fragment, { children: [_jsxs("tr", { onClick: () => setOpen(open === key ? undefined : key), className: purged ? "muted" : "", children: [_jsxs("td", { children: ["L", l.level] }), _jsxs("td", { className: "mono", children: [open === key ? "▾ " : "▸ ", name(l.lsp_id), l.own && _jsx("span", { className: "tag", children: "ours" })] }), _jsx("td", { className: "num mono", children: `0x${l.seq.toString(16)}` }), _jsx("td", { className: "num mono", children: purged ? "purged" : `${l.lifetime} s` }), _jsx("td", { className: "mono", children: l.checksum }), _jsx("td", { className: "num mono", children: l.neighbors }), _jsx("td", { className: "num mono", children: l.prefixes }), _jsx("td", { children: l.overload && _jsx("span", { className: "pill pill-warn", children: "OL" }) })] }), open === key && (_jsx("tr", { className: "lsp-detail", children: _jsx("td", { colSpan: 8, children: _jsx(LSPDetailRow, { agentId: agentId, name: inst, row: l, nameOf: name }) }) }))] }, key));
                            }) })] }) }), _jsx("h4", { children: "Counters" }), _jsx("div", { className: "counters", children: counters.map(([k, v]) => (_jsxs("div", { className: "counter", children: [_jsx("span", { className: "counter-k", children: k.replace(/_/g, " ") }), _jsx("span", { className: /error|mismatch/.test(k) && v ? "counter-v warn-text" : "counter-v", children: v })] }, k))) })] }));
}
// What the agent applies in place (protocols/isis/reconfig.go).
const isisNote = inPlaceNote(["hostname", "overload", "metric", "te_metric", "delay_us", "admin_groups", "prefixes", "loopbacks", "topology", "segment_routing", "flex_algos", "srv6",
    "attached_bit", "summaries", "leak_to_l1", "lsp_refresh", "lsp_lifetime", "lsp_mtu", "csnp_interval"], { interfaces: ["metric", "te_metric", "delay_us", "admin_groups"] }, "Applies in place: adjacencies stay up, and the new LSPs are flooded.");
registerView("isis", { summary, Status, applyNote: isisNote });
