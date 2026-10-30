import { jsxs as _jsxs, jsx as _jsx, Fragment as _Fragment } from "react/jsx-runtime";
import { registerView, restartNote } from "./registry";
import { ago } from "../components/ValueView";
const ms = (us) => (us === undefined ? "—" : us >= 1000 ? `${(us / 1000).toFixed(2)} ms` : `${us.toFixed(0)} µs`);
const hhmmss = (t) => (t ? new Date(t).toLocaleTimeString() : "");
// BinRow is one measurement bin of a session.
function BinRow({ type, b, current }) {
    return (_jsxs("tr", { className: current ? "muted" : "", children: [_jsxs("td", { className: "mono small", children: [hhmmss(b.start), current ? " (now)" : `–${hhmmss(b.end)}`] }), _jsxs("td", { className: "num mono", children: [b.received, "/", b.sent] }), type === "delay" ? (_jsxs(_Fragment, { children: [_jsx("td", { className: "num mono", children: ms(b.delay?.min_us) }), _jsx("td", { className: "num mono", children: ms(b.delay?.avg_us) }), _jsx("td", { className: "num mono", children: ms(b.delay?.max_us) }), _jsx("td", { className: "num mono", children: ms(b.avg_fdv_us) })] })) : (_jsxs(_Fragment, { children: [_jsxs("td", { className: `num mono ${b.far_end_loss > 0 ? "warn-text" : ""}`, children: [b.far_end_loss, " (", b.far_end_loss_pct.toFixed(1), "%)"] }), _jsxs("td", { className: `num mono ${b.near_end_loss > 0 ? "warn-text" : ""}`, children: [b.near_end_loss, " (", b.near_end_loss_pct.toFixed(1), "%)"] })] }))] }));
}
function Sessions({ ss }) {
    return (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Measurement sessions" }), ss.map((x) => (_jsxs("div", { className: "cfm-session", children: [_jsxs("div", { className: "small-note", children: [_jsx("b", { className: "mono", children: x.name }), " \u00B7 ", x.type === "delay" ? "DMM" : x.type === "loss" ? "LMM (data frames)" : "SLM", " \u2192 ", _jsx("span", { className: "mono", children: x.target || "?" }), x.error && _jsxs("span", { className: "warn-text", children: [" \u00B7 ", x.error] })] }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Bin" }), _jsx("th", { className: "num", children: "Replies" }), x.type === "delay" ? (_jsxs(_Fragment, { children: [_jsx("th", { className: "num", children: "Min" }), _jsx("th", { className: "num", children: "Avg" }), _jsx("th", { className: "num", children: "Max" }), _jsx("th", { className: "num", children: "FDV" })] })) : (_jsxs(_Fragment, { children: [_jsx("th", { className: "num", children: "Far-end loss" }), _jsx("th", { className: "num", children: "Near-end loss" })] }))] }) }), _jsxs("tbody", { children: [_jsx(BinRow, { type: x.type, b: x.current, current: true }), x.history.map((b) => (_jsx(BinRow, { type: x.type, b: b }, b.start)))] })] }) })] }, x.name)))] }));
}
const ALL_DEFECTS = [
    ["DefRDICCM", "RDI"],
    ["DefMACstatus", "MAC status"],
    ["DefRemoteCCM", "LOC"],
    ["DefErrorCCM", "Error CCM"],
    ["DefXconCCM", "Xcon"],
];
function health(s) {
    if (!s.highest_defect || s.highest_defect === "none")
        return s.ais_received || s.lck_received || s.csf_received ? "warn" : "ok";
    return s.highest_defect === "DefRDICCM" ? "warn" : "bad";
}
const short = (d) => ALL_DEFECTS.find(([k]) => k === d)?.[1] ?? d;
function rmepHealth(r) {
    if (r.state === "failed")
        return "bad";
    if (r.state !== "ok")
        return "idle";
    const macBad = (r.port_status && r.port_status !== "up") || (r.interface_status && r.interface_status !== "up");
    return r.rdi || macBad ? "warn" : "ok";
}
function Counters({ title, c }) {
    const keys = Object.keys(c).sort();
    return (_jsxs("div", { className: "counter-block", children: [_jsx("h4", { children: title }), keys.length === 0 ? (_jsx("div", { className: "muted", children: "none" })) : (_jsx("div", { className: "counters", children: keys.map((k) => (_jsxs("div", { className: "counter", children: [_jsx("span", { className: "counter-k", children: k }), _jsx("span", { className: "counter-v mono", children: c[k].toLocaleString() })] }, k))) }))] }));
}
function CFMStatus({ status }) {
    const s = status;
    const active = new Set(s.defects ?? []);
    return (_jsxs("div", { className: "cfm", children: [_jsxs("div", { className: "facts", children: [_jsx(Fact, { k: "MAID", v: s.maid, mono: true }), _jsx(Fact, { k: "Level", v: String(s.level) }), _jsx(Fact, { k: "MEP ID", v: String(s.mep_id) }), _jsx(Fact, { k: "Interval", v: s.ccm_interval }), _jsx(Fact, { k: "MAC", v: s.mac, mono: true })] }), _jsxs("div", { className: "defects", children: [ALL_DEFECTS.map(([k, name]) => (_jsx("span", { className: `defect ${active.has(k) ? (k === "DefRDICCM" ? "warn" : "bad") : ""}`, title: k, children: name }, k))), _jsx("span", { className: "sep" }), _jsx("span", { className: `defect ${s.ccm_enabled ? "" : "warn"}`, children: s.ccm_enabled ? "CCM tx on" : "CCM tx off" }), _jsx("span", { className: `defect ${s.present_rdi ? "warn" : ""}`, children: s.present_rdi ? "sending RDI" : "no RDI tx" }), s.ais_received && _jsx("span", { className: "defect warn", children: "AIS rx" }), s.lck_received && _jsx("span", { className: "defect warn", children: "LCK rx" }), s.csf_received && (_jsxs("span", { className: "defect warn", title: `from ${s.csf_received.from}`, children: ["CSF ", s.csf_received.type, " rx"] })), s.ais_sending && _jsx("span", { className: "defect warn", children: "sending AIS" }), s.lck_sending && _jsx("span", { className: "defect warn", children: "sending LCK (locked)" }), s.csf_sending && _jsxs("span", { className: "defect warn", children: ["sending CSF ", s.csf_sending] })] }), (s.error_reason || s.xcon_reason) && (_jsxs("div", { className: "reasons", children: [s.error_reason && _jsxs("div", { children: [_jsx("b", { children: "Error CCM:" }), " ", s.error_reason] }), s.xcon_reason && _jsxs("div", { children: [_jsx("b", { children: "Xcon:" }), " ", s.xcon_reason] })] })), s.faults && Object.keys(s.faults).length > 0 && (_jsxs("div", { className: "reasons warn-box", children: [_jsx("b", { children: "Injected faults:" }), " ", Object.entries(s.faults)
                        .map(([k, v]) => `${k.replace(/_/g, " ")}=${String(v)}`)
                        .join(", ")] })), _jsx("h4", { children: "Remote MEPs" }), (s.remote_meps ?? []).length === 0 ? (_jsx("div", { className: "muted", children: "None yet. With remote_meps empty, peers are learned from their CCMs." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "MEP" }), _jsx("th", { children: "State" }), _jsx("th", { children: "MAC" }), _jsx("th", { children: "RDI" }), _jsx("th", { title: "Port Status / Interface Status TLVs", children: "Port/Intf" }), _jsx("th", { children: "Interval" }), _jsx("th", { className: "num", children: "CCM rx" }), _jsx("th", { className: "num", children: "Seq err" }), _jsx("th", { children: "Last CCM / state since" })] }) }), _jsx("tbody", { children: s.remote_meps.map((r) => (_jsxs("tr", { children: [_jsxs("td", { className: "mono", children: [r.mep_id, !r.configured && _jsx("span", { className: "tag", title: "auto-discovered", children: "AD" }), (r.sender_id || r.org_tlv || r.ccm_padding) && (_jsx("div", { className: "muted small-note", children: [r.sender_id && `sender ${r.sender_id}`, r.org_tlv && `org ${r.org_tlv}`, r.ccm_padding && `pad ${r.ccm_padding} B`].filter(Boolean).join(" · ") }))] }), _jsxs("td", { children: [_jsx("span", { className: `dot ${rmepHealth(r)}` }), " ", r.state] }), _jsx("td", { className: "mono", children: r.mac ?? "—" }), _jsx("td", { children: r.rdi ? _jsx("span", { className: "pill pill-warn", children: "RDI" }) : "—" }), _jsxs("td", { className: "mono", children: [r.port_status ?? "—", " / ", r.interface_status ?? "—"] }), _jsx("td", { className: "mono", children: r.ccm_interval ?? "—" }), _jsx("td", { className: "num mono", children: (r.ccm_rx ?? 0).toLocaleString() }), _jsx("td", { className: `num mono ${r.seq_errors ? "warn-text" : ""}`, children: r.seq_errors ?? 0 }), _jsxs("td", { className: "small", children: [r.last_rx && !r.last_rx.startsWith("0001") ? ago(new Date(r.last_rx)) : "—", _jsxs("span", { className: "muted", children: [" / ", r.state_since ? ago(new Date(r.state_since)) : "—"] })] })] }, r.mep_id))) })] }) })), (s.one_way_delay ?? []).length > 0 && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "One-way delay received (1DM)" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "From" }), _jsx("th", { className: "num", children: "Count" }), _jsx("th", { className: "num", children: "Last" }), _jsx("th", { className: "num", children: "Min" }), _jsx("th", { className: "num", children: "Avg" }), _jsx("th", { className: "num", children: "Max" }), _jsx("th", { className: "num", children: "FDV" })] }) }), _jsx("tbody", { children: s.one_way_delay.map((o) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: o.from }), _jsx("td", { className: "num mono", children: o.count }), _jsx("td", { className: "num mono", children: ms(o.last_us) }), _jsx("td", { className: "num mono", children: ms(o.min_us) }), _jsx("td", { className: "num mono", children: ms(o.avg_us) }), _jsx("td", { className: "num mono", children: ms(o.max_us) }), _jsx("td", { className: "num mono", children: ms(o.avg_fdv_us) })] }, o.from))) })] }) })] })), (s.test_signal ?? []).length > 0 && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Test signal received (TST)" }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "From" }), _jsx("th", { className: "num", children: "Received" }), _jsx("th", { className: "num", children: "Bytes" }), _jsx("th", { className: "num", children: "Lost" }), _jsx("th", { className: "num", children: "Out of order" }), _jsx("th", { className: "num", children: "Bit errors" })] }) }), _jsx("tbody", { children: s.test_signal.map((x) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: x.from }), _jsx("td", { className: "num mono", children: x.received }), _jsx("td", { className: "num mono", children: x.pattern_bytes.toLocaleString() }), _jsx("td", { className: `num mono ${x.lost ? "warn-text" : ""}`, children: x.lost }), _jsx("td", { className: "num mono", children: x.out_of_order }), _jsxs("td", { className: `num mono ${x.bit_errors ? "warn-text" : ""}`, children: [x.bit_errors, x.unchecked ? _jsxs("span", { className: "muted", children: [" (", x.unchecked, " unchecked)"] }) : null] })] }, x.from))) })] }) })] })), (s.sessions ?? []).length > 0 && _jsx(Sessions, { ss: s.sessions }), _jsxs("div", { className: "counter-row", children: [_jsx(Counters, { title: "Transmitted", c: s.tx ?? {} }), _jsx(Counters, { title: "Received", c: { ...(s.rx ?? {}), ...(s.rx_errors ? { errors: s.rx_errors } : {}) } })] })] }));
}
function Fact({ k, v, mono }) {
    return (_jsxs("div", { className: "fact", children: [_jsx("div", { className: "fact-k", children: k }), _jsx("div", { className: `fact-v ${mono ? "mono" : ""}`, children: v })] }));
}
registerView("cfm-mep", {
    summary: (st) => {
        const s = st;
        const rm = (s.remote_meps ?? []).map((r) => `${r.mep_id}:${r.state}`).join(" ");
        const def = (s.defects ?? []).map(short).join(", ");
        return {
            health: health(s),
            text: `L${s.level} MEP ${s.mep_id} · ${s.maid} · rmeps [${rm || "none"}]${def ? " · " + def : ""}`,
        };
    },
    Status: CFMStatus,
    // Only a new identity restarts the MEP (protocols/cfm/reconfig.go).
    applyNote: (was, now) => ["level", "mep_id", "md_format", "md_name", "ma_format", "ma_name", "ccm_interval"].every((k) => was[k] === now[k])
        ? { restart: false, text: "Applies in place: the MEP keeps its remote MEPs, counters and sessions (a changed session list restarts the sessions)." }
        : restartNote,
});
