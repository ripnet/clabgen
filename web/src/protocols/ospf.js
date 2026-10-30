import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { Fragment, useState } from "react";
import { client, usePoll } from "../api";
import { ValueView } from "../components/ValueView";
import { inPlaceNote, registerView, restartNote } from "./registry";
const as = (s) => s;
const active = (s) => s.interfaces.filter((i) => !i.passive);
function summary(st) {
    const s = as(st);
    const ifs = active(s);
    const full = s.neighbors.filter((n) => n.state === "full").length;
    const live = ifs.filter((i) => i.full > 0).length;
    const tail = `${s.lsdb_total} LSAs${s.max_metric ? " · max-metric" : ""}`;
    if (ifs.length === 0)
        return { health: "ok", text: `passive · ${tail}` };
    if (ifs.some((i) => i.flapping) && !full)
        return { health: "bad", text: "down (flap)" };
    if (!full) {
        const best = s.neighbors.map((n) => n.state).sort()[0];
        return {
            health: s.neighbors.length ? "warn" : "bad",
            text: s.neighbors.length ? `no full adjacency (${best})` : "no neighbors",
        };
    }
    const roles = ifs
        .filter((i) => i.network === "broadcast")
        .map((i) => i.state)
        .filter((x) => x === "dr" || x === "backup");
    const who = full === 1
        ? s.neighbors.find((n) => n.state === "full").router_id
        : `${full} neighbors`;
    return {
        health: live < ifs.length || s.max_metric ? "warn" : "ok",
        text: `full with ${who}${ifs.length > 1 ? ` on ${live}/${ifs.length} interfaces` : ""}${roles.length ? ` · ${roles.join("/").toUpperCase().replace("BACKUP", "BDR")}` : ""} · ${tail}`,
    };
}
const pill = (state) => state === "full"
    ? "pill pill-on"
    : state === "2-way"
        ? "pill"
        : "pill pill-warn";
function LSADetail({ agentId, name, row, }) {
    const d = usePoll(async () => {
        if (!agentId || !name)
            return undefined;
        const r = await client(agentId).runProtocolAction({
            name,
            action: "lsa",
            args: {
                type: row.type,
                id: row.id,
                adv_router: row.adv_router,
                area: row.area ?? (row.link ? "" : "as"),
            },
        });
        return r.result;
    }, [agentId, name, row.type, row.id, row.adv_router], 3000);
    if (d.error)
        return _jsx("div", { className: "error-text", children: d.error });
    if (!d.data)
        return _jsx("div", { className: "muted", children: "Loading\u2026" });
    const { header: _h, ...rest } = d.data;
    return _jsx(ValueView, { value: rest });
}
function Status({ status, agentId, name: inst, }) {
    const s = as(status);
    const [open, setOpen] = useState();
    const multi = s.interfaces.length > 1;
    const multiArea = (s.areas?.length ?? 0) > 1;
    const anyBFD = s.neighbors.some((n) => n.bfd);
    const counters = Object.entries(s.counters ?? {}).sort(([x], [y]) => x.localeCompare(y));
    const types = Object.entries(s.lsdb_types ?? {})
        .map(([k, v]) => `${v} ${k}`)
        .join(" · ");
    return (_jsxs("div", { className: "isis", children: [_jsxs("dl", { className: "kv", children: [_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "router ID \u00B7 area" }), _jsxs("dd", { className: "mono", children: [s.router_id, " \u00B7 ", s.area] })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "roles" }), _jsx("dd", { className: "mono", children: [s.abr && "ABR", s.asbr && "ASBR", s.max_metric && "max-metric"]
                                    .filter(Boolean)
                                    .join(" · ") || "internal router" })] }), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "authentication" }), _jsxs("dd", { className: "mono", children: [s.auth, (s.counters?.rx_auth_failures ?? 0) > 0 && (_jsxs("span", { className: "warn-text", children: [" ", "\u00B7 ", s.counters.rx_auth_failures, " failures"] }))] })] }), s.graceful_restart && (_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "graceful restart" }), _jsxs("dd", { className: "mono", children: [s.graceful_restart.restart ? (_jsxs("span", { className: "warn-text", children: ["restarting (", s.graceful_restart.restart, s.graceful_restart.waiting_for
                                                ? `, waiting for ${s.graceful_restart.waiting_for} adjacencies`
                                                : "", ")"] })) : (`helper ${s.graceful_restart.helper ? "on" : "off"}`), s.graceful_restart.helping?.length
                                        ? ` · helping ${s.graceful_restart.helping.join(", ")}`
                                        : ""] })] })), _jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: "advertised prefixes" }), _jsxs("dd", { className: "mono", children: [s.advertised_prefixes, " injected (+ interfaces", s.loopbacks?.length ? `, loopbacks ${s.loopbacks.join(", ")}` : "", ")"] })] })] }), _jsx("h4", { children: "Interfaces" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Interface" }), _jsx("th", { children: "Network" }), multiArea && _jsx("th", { children: "Area" }), _jsx("th", { children: "State" }), _jsx("th", { className: "num", children: "Cost" }), _jsx("th", { children: "DR \u00B7 BDR" }), _jsx("th", { className: "num", children: "Full" }), _jsx("th", { children: "Address" }), _jsx("th", { children: "Auth" })] }) }), _jsx("tbody", { children: s.interfaces.map((i) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: i.name }), _jsxs("td", { className: "mono", children: [i.network, (i.network === "broadcast" ||
                                                i.network === "non-broadcast") && (_jsxs("span", { className: "muted", children: [" \u00B7 prio ", i.priority ?? 0] }))] }), multiArea && _jsx("td", { className: "mono", children: i.area }), _jsxs("td", { children: [_jsx("span", { className: i.state === "down" ? "pill pill-warn" : "pill pill-on", children: i.passive ? "passive" : i.state }), i.flapping && _jsx("span", { className: "tag tag-warn", children: "flapping" })] }), _jsx("td", { className: "num mono", children: i.cost }), _jsx("td", { className: "mono", children: i.network === "broadcast" || i.network === "non-broadcast"
                                            ? `${i.dr ?? "—"} · ${i.bdr ?? "—"}`
                                            : "—" }), _jsxs("td", { className: "num mono", children: [i.full, "/", i.neighbors] }), _jsx("td", { className: "mono", children: i.address ?? i.error }), _jsxs("td", { className: "mono", children: [i.auth, i.last_error && (_jsx("div", { className: "warn-text small", title: "Last rejected packet", children: i.last_error }))] })] }, i.name))) })] }) }), _jsx("h4", { children: "Neighbors" }), s.neighbors.length === 0 ? (_jsx("div", { className: "muted", children: "No neighbors heard." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [multi && _jsx("th", { children: "Interface" }), _jsx("th", { children: "Router ID" }), _jsx("th", { children: "Address" }), _jsx("th", { children: "State" }), _jsx("th", { className: "num", children: "Priority" }), _jsx("th", { className: "num", children: "Dead" }), _jsx("th", { children: "Since" }), anyBFD && _jsx("th", { children: "BFD" }), _jsx("th", { children: "Last change" })] }) }), _jsx("tbody", { children: s.neighbors.map((n) => (_jsxs("tr", { children: [multi && _jsx("td", { className: "mono", children: n.interface }), _jsxs("td", { className: "mono", children: [n.router_id, n.role && (_jsx("span", { className: "tag", children: n.role.toUpperCase() }))] }), _jsx("td", { className: "mono", children: n.address }), _jsxs("td", { children: [_jsx("span", { className: pill(n.state), children: n.state }), (n.requests ?? 0) > 0 && (_jsxs("span", { className: "muted small", children: [" \u00B7 ", n.requests, " req"] })), (n.retransmit ?? 0) > 0 && (_jsxs("span", { className: "muted small", children: [" ", "\u00B7 ", n.retransmit, " rxmt"] }))] }), _jsx("td", { className: "num mono", children: n.priority }), _jsxs("td", { className: "num mono", children: [Math.max(0, n.dead_in_s).toFixed(0), " s"] }), _jsx("td", { className: "mono", children: new Date(n.since).toLocaleTimeString() }), anyBFD && (_jsx("td", { children: n.bfd ? (_jsxs("span", { className: n.bfd.state === "up"
                                                ? "pill pill-on"
                                                : "pill pill-warn", title: n.bfd.last_down
                                                ? `last down: ${n.bfd.last_down}`
                                                : undefined, children: [n.bfd.state, n.bfd.detect_ms ? ` · ${n.bfd.detect_ms} ms` : "", n.bfd.silent ? " · silent" : ""] })) : ("—") })), _jsx("td", { className: "muted", children: n.reason })] }, `${n.interface}/${n.router_id}`))) })] }) })), multiArea && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Areas" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Area" }), _jsx("th", { children: "Type" }), _jsx("th", { children: "Interfaces" }), _jsx("th", { className: "num", children: "LSAs" }), _jsx("th", { children: "Ranges" }), _jsx("th", { className: "num", children: "Routers" }), _jsx("th", { className: "num", children: "Prefixes" })] }) }), _jsx("tbody", { children: s.areas.map((a) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: a.id }), _jsxs("td", { children: [a.type, a.no_summary && _jsx("span", { className: "tag", children: "no summary" }), a.nssa_translator && (_jsx("span", { className: "tag", children: "translator" }))] }), _jsx("td", { className: "mono", children: [...a.interfaces, a.topology ? "emulated topology" : ""]
                                                    .filter(Boolean)
                                                    .join(", ") || "—" }), _jsx("td", { className: "num mono", children: a.lsas }), _jsx("td", { className: "mono", children: a.ranges?.join(", ") ?? "—" }), _jsx("td", { className: "num mono", children: a.reachable_routers ?? "—" }), _jsx("td", { className: "num mono", children: a.intra_area_prefixes ?? "—" })] }, a.id))) })] }) })] })), s.segment_routing && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Segment routing" }), _jsx("div", { className: "flow-path mono small-note", children: [
                            s.segment_routing.srgb && `SRGB ${s.segment_routing.srgb}`,
                            s.segment_routing.srlb && `SRLB ${s.segment_routing.srlb}`,
                            `algorithms ${(s.segment_routing.algorithms ?? []).join(", ")}`,
                            s.segment_routing.traffic_engineering && "TE LSAs",
                        ]
                            .filter(Boolean)
                            .join(" · ") }), (s.segment_routing.node_sids?.length ?? 0) > 0 && (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Prefix" }), _jsx("th", { className: "num", children: "Index" }), _jsx("th", { className: "num", children: "Label" }), _jsx("th", { className: "num", children: "Algorithm" }), _jsx("th", { children: "Router" })] }) }), _jsx("tbody", { children: s.segment_routing.node_sids.map((x) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: x.prefix }), _jsx("td", { className: "num mono", children: x.index }), _jsx("td", { className: "num mono", children: x.label }), _jsx("td", { className: "num mono", children: x.algorithm ?? 0 }), _jsx("td", { className: "mono", children: x.router ?? "us" })] }, `${x.prefix}/${x.algorithm ?? 0}`))) })] }) })), (s.segment_routing.adj_sids?.length ?? 0) > 0 && (_jsxs("div", { className: "muted small-note", children: ["Adjacency SIDs:", " ", s.segment_routing
                                .adj_sids.map((x) => `${x.label} → ${x.neighbor} (${x.interface}${x.lan ? ", LAN" : ""})`)
                                .join(" · ")] }))] })), s.topology && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Emulated topology" }), _jsxs("div", { className: "flow-path mono small-note", children: [s.topology.kind, " \u00B7 ", s.topology.nodes, " routers \u00B7 ", s.topology.links, " ", "links \u00B7 ", s.topology.prefixes, " prefixes \u00B7 attached to us:", " ", s.topology.attached.join(", ") || "none"] }), s.topology.down_nodes?.length ||
                        s.topology.down_links?.length ||
                        s.topology.overloaded?.length ? (_jsxs("div", { className: "warn-box", children: [s.topology.down_nodes?.length ? (_jsxs("div", { children: ["routers down: ", s.topology.down_nodes.join(", ")] })) : null, s.topology.down_links?.length ? (_jsxs("div", { children: ["links down: ", s.topology.down_links.join(", ")] })) : null, s.topology.overloaded?.length ? (_jsxs("div", { children: ["max-metric: ", s.topology.overloaded.join(", ")] })) : null] })) : (_jsx("div", { className: "muted small-note", children: "All simulated routers and links are up. Use the topology-link and topology-node actions to fail them." }))] })), _jsx("h4", { children: "Link-state database" }), _jsxs("div", { className: "muted small-note", children: [types || "empty", s.lsdb_total > s.lsdb.length &&
                        ` · showing ${s.lsdb.length} of ${s.lsdb_total}`] }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense hover", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Type" }), multiArea && _jsx("th", { children: "Area" }), _jsx("th", { children: "Link state ID" }), _jsx("th", { children: "Advertising router" }), _jsx("th", { className: "num", children: "Seq" }), _jsx("th", { className: "num", children: "Age" }), _jsx("th", { children: "Checksum" }), _jsx("th", { children: "Content" })] }) }), _jsx("tbody", { children: s.lsdb.map((l) => {
                                const key = `${l.type}/${l.id}/${l.adv_router}/${l.link ?? ""}`;
                                const flushed = l.age >= 3600;
                                return (_jsxs(Fragment, { children: [_jsxs("tr", { onClick: () => setOpen(open === key ? undefined : key), className: flushed ? "muted" : "", children: [_jsxs("td", { children: [l.type, l.link && (_jsx("span", { className: "tag", title: "Link scope: flooded only on this interface", children: l.link }))] }), multiArea && _jsx("td", { className: "mono", children: l.area ?? "AS" }), _jsxs("td", { className: "mono", children: [open === key ? "▾ " : "▸ ", l.id] }), _jsxs("td", { className: "mono", children: [l.adv_router, l.own && _jsx("span", { className: "tag", children: "ours" })] }), _jsx("td", { className: "num mono", children: l.seq }), _jsx("td", { className: "num mono", children: flushed ? "MaxAge" : `${l.age} s` }), _jsx("td", { className: "mono", children: l.checksum }), _jsx("td", { className: "mono", children: l.summary })] }), open === key && (_jsx("tr", { className: "lsp-detail", children: _jsx("td", { colSpan: multiArea ? 8 : 7, children: _jsx(LSADetail, { agentId: agentId, name: inst, row: l }) }) }))] }, key));
                            }) })] }) }), _jsx("h4", { children: "Counters" }), _jsx("div", { className: "counters", children: counters.map(([k, v]) => (_jsxs("div", { className: "counter", children: [_jsx("span", { className: "counter-k", children: k.replace(/_/g, " ") }), _jsx("span", { className: /error|mismatch|failure/.test(k) && v
                                ? "counter-v warn-text"
                                : "counter-v", children: v })] }, k))) })] }));
}
// What the agent applies in place (protocols/ospf/reconfig.go): the
// simulated topology's area and, without a router_id, OSPFv2's first
// loopback (the router ID) restart it.
const ospfInPlace = inPlaceNote(["topology", "loopbacks", "prefixes", "max_metric", "cost", "te_metric", "delay_us", "admin_groups", "bandwidth_mbps", "segment_routing", "flex_algos",
    "traffic_engineering", "lsa_refresh"], { interfaces: ["cost", "te_metric", "delay_us", "admin_groups", "bandwidth_mbps"] }, "Applies in place: adjacencies stay up, and the new LSAs are flooded (ones no longer wanted are flushed).");
const ospfNote = (v3) => (was, now) => {
    const area = (c) => (c.topology ?? {}).area ?? "";
    const lb = (c) => (c.loopbacks ?? [])[0] ?? "";
    if (area(was) !== area(now) || (!v3 && !now.router_id && lb(was) !== lb(now)))
        return restartNote;
    return ospfInPlace(was, now);
};
registerView("ospf", { summary, Status, applyNote: ospfNote(false) });
registerView("ospfv3", { summary, Status, applyNote: ospfNote(true) });
