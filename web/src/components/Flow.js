import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useState } from "react";
const checkNames = {
    loss_pct: "loss",
    outage_ms: "longest outage",
    rtt_avg_ms: "RTT average",
    rtt_ms: "RTT maximum",
    jitter_ms: "jitter",
};
const checkUnit = (c) => (c === "loss_pct" ? "%" : " ms");
const fmtCheck = (c, v) => `${v < 10 ? v.toFixed(2).replace(/\.?0+$/, "") : Math.round(v)}${checkUnit(c)}`;
// VerdictBadge is a flow's checks result in a word.
export function VerdictBadge({ v }) {
    if (!v)
        return _jsx("span", { className: "muted", children: "\u2014" });
    const cls = v.result === "pass" ? "verdict pass" : v.result === "fail" ? "verdict fail" : "verdict pending";
    const title = v.failed ? `failed: ${v.failed}` : v.result === "pending" ? "nothing measured yet" : v.checks.map((c) => `${checkNames[c.check] ?? c.check} ≤ ${fmtCheck(c.check, c.limit)}`).join(", ");
    return (_jsx("span", { className: cls, title: title, children: v.result }));
}
// VerdictPanel lists a flow's checks with what it measured.
export function VerdictPanel({ v }) {
    return (_jsxs("div", { className: "verdict-panel", children: [_jsxs("h4", { children: ["Checks ", _jsx(VerdictBadge, { v: v })] }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Check" }), _jsx("th", { className: "num", children: "Limit" }), _jsx("th", { className: "num", children: "Measured" }), _jsx("th", { children: "Result" })] }) }), _jsx("tbody", { children: v.checks.map((c) => (_jsxs("tr", { children: [_jsx("td", { children: checkNames[c.check] ?? c.check }), _jsxs("td", { className: "num mono", children: ["\u2264 ", fmtCheck(c.check, c.limit)] }), _jsx("td", { className: "num mono", children: c.unavailable ? "—" : fmtCheck(c.check, c.value) }), _jsx("td", { children: c.unavailable ? (_jsx("span", { className: "muted", title: "One-way flows are measured at their receivers", children: "not measured here" })) : c.pass ? (_jsx("span", { className: "verdict pass", children: "pass" })) : (_jsx("span", { className: "verdict fail", children: "fail" })) })] }, c.check))) })] }) })] }));
}
export const asFlow = (s) => s;
export const ms = (us) => (us === undefined ? "—" : us < 1000 ? `${(us / 1000).toFixed(3)} ms` : `${(us / 1000).toFixed(2)} ms`);
export const dur = (m) => (m < 1000 ? `${Math.round(m)} ms` : `${(m / 1000).toFixed(m < 10000 ? 2 : 1)} s`);
const n = (v) => v.toLocaleString();
export function flowSummary(st) {
    const s = asFlow(st);
    // A failed check outranks everything else; it stays until cleared.
    if (s.verdict?.result === "fail")
        return { health: "bad", text: `checks failed: ${s.verdict.failed}` };
    if (s.state === "resolving")
        return { health: "warn", text: s.error ? `resolving: ${s.error}` : "resolving" };
    if (s.state !== "running") {
        const tail = s.tx ? ` · ${n(s.rx)}/${n(s.tx)} · ${s.loss_pct.toFixed(1)}% lost` : "";
        return { health: "idle", text: s.state + tail };
    }
    if (s.outage_current_ms > 0)
        return { health: "bad", text: `outage ${dur(s.outage_current_ms)}` };
    const rtt = s.rtt ? ` · ${ms(s.rtt.avg_us)}` : "";
    if (!s.tracks_replies)
        return { health: "ok", text: `sending · ${n(s.tx)} tx` };
    const checks = s.verdict?.result === "pass" ? " · checks pass" : "";
    return { health: s.lost || s.errors ? "warn" : "ok", text: `${n(s.rx)}/${n(s.tx)} · ${s.loss_pct.toFixed(2)}% lost${rtt}${checks}` };
}
function Tile({ k, v, tone }) {
    return (_jsxs("div", { className: "stat", children: [_jsx("div", { className: tone ? `stat-n ${tone}-text` : "stat-n", children: v }), _jsx("div", { className: "stat-l", children: k })] }));
}
export function FlowTiles({ s }) {
    return (_jsxs("div", { className: "flow-tiles", children: [_jsx(Tile, { k: "sent", v: n(s.tx) }), s.tracks_replies !== false && (_jsxs(_Fragment, { children: [_jsx(Tile, { k: "received", v: n(s.rx) }), _jsx(Tile, { k: `lost (${s.loss_pct.toFixed(2)}%)`, v: n(s.lost), tone: s.lost ? "bad" : undefined }), _jsx(Tile, { k: "outage now", v: s.outage_current_ms ? dur(s.outage_current_ms) : "—", tone: s.outage_current_ms ? "bad" : undefined }), _jsx(Tile, { k: `longest outage (${s.outages})`, v: s.outage_max_ms ? dur(s.outage_max_ms) : "—", tone: s.outage_max_ms ? "warn" : undefined }), _jsx(Tile, { k: "rtt avg", v: ms(s.rtt?.avg_us) }), _jsx(Tile, { k: "rtt min / max", v: s.rtt ? `${ms(s.rtt.min_us)} / ${ms(s.rtt.max_us)}` : "—" }), _jsx(Tile, { k: "jitter", v: ms(s.rtt?.jitter_us) })] })), (s.errors > 0 || s.tx_errors > 0 || s.late > 0 || s.duplicate > 0) && (_jsx(Tile, { k: "errors \u00B7 tx errors \u00B7 late \u00B7 dup", v: `${s.errors} · ${s.tx_errors} · ${s.late} · ${s.duplicate}`, tone: "warn" }))] }));
}
const W = 720;
const H = 120;
const PAD_L = 44;
const PAD_B = 18;
// Timeline: one stacked bar per second (by send time): received at the
// bottom, lost on top. Lost is red, received the accent blue (a validated
// colour-blind-safe pair); the legend names both.
export function FlowTimeline({ s, seconds = 120 }) {
    const [hover, setHover] = useState();
    const byT = new Map(s.timeline.map((b) => [b.t, b]));
    const last = s.timeline.length ? Math.max(...s.timeline.map((b) => b.t)) : Math.floor(Date.now() / 1000);
    const ts = Array.from({ length: seconds }, (_, i) => last - seconds + 1 + i);
    const max = Math.max(1, ...s.timeline.map((b) => b.tx));
    const bw = (W - PAD_L) / seconds;
    const y = (v) => (H - PAD_B) * (1 - v / max);
    const hb = hover !== undefined ? byT.get(ts[hover]) : undefined;
    const replies = s.tracks_replies !== false;
    const rttMax = Math.max(1, ...s.timeline.map((b) => b.rtt_avg_us ?? 0));
    return (_jsxs("div", { className: "flow-chart", children: [_jsxs("div", { className: "chart-head", children: [_jsxs("span", { className: "chart-title", children: ["Packets per second, by send time \u00B7 last ", seconds, "s"] }), _jsxs("span", { className: "legend", children: [_jsx("span", { className: "sw sw-rx" }), " ", replies ? "received" : "sent", replies && (_jsxs(_Fragment, { children: [_jsx("span", { className: "sw sw-lost" }), " lost"] }))] })] }), _jsxs("div", { className: "chart-wrap", onMouseLeave: () => setHover(undefined), children: [_jsxs("svg", { viewBox: `0 0 ${W} ${H}`, className: "chart", role: "img", "aria-label": "Packets per second timeline", children: [_jsx("line", { x1: PAD_L, x2: W, y1: H - PAD_B, y2: H - PAD_B, className: "axis" }), _jsx("line", { x1: PAD_L, x2: W, y1: 0.5, y2: 0.5, className: "grid" }), _jsx("text", { x: PAD_L - 6, y: 10, className: "tick", textAnchor: "end", children: n(max) }), _jsx("text", { x: PAD_L - 6, y: H - PAD_B, className: "tick", textAnchor: "end", children: "0" }), [seconds, seconds / 2, 0].map((ago, i) => (_jsx("text", { x: PAD_L + (seconds - ago) * bw - (i === 2 ? 0 : 0), y: H - 4, className: "tick", textAnchor: i === 0 ? "start" : i === 2 ? "end" : "middle", children: ago ? `-${ago}s` : "now" }, i))), ts.map((t, i) => {
                                const b = byT.get(t);
                                if (!b)
                                    return null;
                                const x = PAD_L + i * bw + 0.5;
                                const w = Math.max(1, bw - 2);
                                const ok = replies ? b.rx : b.tx;
                                const lostTop = y(ok + b.lost);
                                return (_jsxs("g", { children: [ok > 0 && _jsx("rect", { x: x, y: y(ok), width: w, height: H - PAD_B - y(ok), rx: Math.min(2, w / 2), className: "bar-rx" }), replies && b.lost > 0 && (_jsx("rect", { x: x, y: lostTop, width: w, height: Math.max(1, y(ok) - lostTop - (ok > 0 ? 2 : 0)), rx: Math.min(2, w / 2), className: "bar-lost" }))] }, t));
                            }), hover !== undefined && _jsx("rect", { x: PAD_L + hover * bw, y: 0, width: bw, height: H - PAD_B, className: "hover-col" }), ts.map((t, i) => (_jsx("rect", { x: PAD_L + i * bw, y: 0, width: bw, height: H, fill: "transparent", onMouseEnter: () => setHover(i) }, "h" + t)))] }), hover !== undefined && (_jsxs("div", { className: "chart-tip", style: { left: `${Math.min(80, Math.max(0, ((PAD_L + hover * bw) / W) * 100))}%` }, children: [_jsx("div", { className: "mono", children: new Date(ts[hover] * 1000).toLocaleTimeString() }), hb ? (_jsxs(_Fragment, { children: [_jsxs("div", { children: ["sent ", _jsx("b", { children: n(hb.tx) }), replies && (_jsxs(_Fragment, { children: [" ", "\u00B7 received ", _jsx("b", { children: n(hb.rx) }), " \u00B7 lost ", _jsx("b", { children: n(hb.lost) })] }))] }), hb.rtt_avg_us !== undefined && (_jsxs("div", { children: ["rtt avg ", ms(hb.rtt_avg_us), " \u00B7 max ", ms(hb.rtt_max_us)] }))] })) : (_jsx("div", { className: "muted", children: "nothing sent" }))] }))] }), replies && s.rtt && (_jsxs(_Fragment, { children: [_jsx("div", { className: "chart-head", children: _jsx("span", { className: "chart-title", children: "Round-trip time, per second (avg)" }) }), _jsxs("svg", { viewBox: `0 0 ${W} 60`, className: "chart", role: "img", "aria-label": "Round-trip time", children: [_jsx("line", { x1: PAD_L, x2: W, y1: 59.5, y2: 59.5, className: "axis" }), _jsx("text", { x: PAD_L - 6, y: 10, className: "tick", textAnchor: "end", children: ms(rttMax) }), _jsx("text", { x: PAD_L - 6, y: 58, className: "tick", textAnchor: "end", children: "0" }), _jsx("line", { x1: PAD_L, x2: W, y1: 6.5, y2: 6.5, className: "grid" }), _jsx("polyline", { className: "rtt-line", points: ts
                                    .map((t, i) => {
                                    const b = byT.get(t);
                                    return b?.rtt_avg_us !== undefined ? `${PAD_L + i * bw + bw / 2},${59 - (52 * b.rtt_avg_us) / rttMax}` : null;
                                })
                                    .filter(Boolean)
                                    .join(" ") })] })] }))] }));
}
// FlowLog shows per-packet results like ping output, newest last.
export function FlowLog({ s }) {
    if (!s.recent.length)
        return _jsx("div", { className: "muted", children: "No packets yet." });
    return (_jsx("pre", { className: "flow-log mono", children: s.recent.map((l, i) => (_jsxs("div", { className: l.result === "reply" ? "" : l.result === "timeout" || l.result === "error" || l.result === "tx-error" ? "bad-text" : "warn-text", children: [l.time, " seq=", l.seq, " ", l.result === "reply" ? `${l.detail ? l.detail + " " : ""}time=${ms(l.rtt_us)}` : `${l.result}${l.detail ? " " + l.detail : ""}`] }, i))) }));
}
