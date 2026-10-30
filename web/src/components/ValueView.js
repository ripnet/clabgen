import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
const isObj = (v) => typeof v === "object" && v !== null && !Array.isArray(v);
const label = (k) => k.replace(/_/g, " ");
const tsRE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/;
// Identifiers read better without digit grouping.
const rawKey = /(^|_)(id|seq|vid|ttl|hop|level|index|test_id|trans_id|mep_id)$/;
// Struct fields arrive sorted by name; lead tables with identifying columns.
const leadCols = ["hop", "seq", "mep_id", "id", "name", "state"];
export function fmtScalar(v, key = "") {
    if (v === null || v === undefined)
        return "—";
    if (typeof v === "number" && rawKey.test(key))
        return String(v);
    if (typeof v === "number")
        return Number.isInteger(v) ? v.toLocaleString() : v.toFixed(v < 10 ? 3 : 1);
    if (typeof v === "string" && tsRE.test(v)) {
        const d = new Date(v);
        if (d.getFullYear() < 1971)
            return "—";
        return `${d.toLocaleTimeString()} (${ago(d)})`;
    }
    return String(v);
}
export function ago(d) {
    const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
    if (s < 1)
        return "now";
    if (s < 60)
        return `${Math.round(s)}s ago`;
    if (s < 3600)
        return `${Math.round(s / 60)}m ago`;
    return `${Math.round(s / 3600)}h ago`;
}
function Scalar({ v, k }) {
    if (typeof v === "boolean")
        return _jsx("span", { className: v ? "pill pill-on" : "pill", children: String(v) });
    return _jsx("span", { className: "mono", children: fmtScalar(v, k) });
}
export function Table({ rows }) {
    const cols = [];
    for (const r of rows)
        for (const k of Object.keys(r))
            if (!cols.includes(k))
                cols.push(k);
    cols.sort((a, b) => {
        const ia = leadCols.indexOf(a), ib = leadCols.indexOf(b);
        return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
    });
    return (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsx("tr", { children: cols.map((c) => (_jsx("th", { children: label(c) }, c))) }) }), _jsx("tbody", { children: rows.map((r, i) => (_jsx("tr", { children: cols.map((c) => (_jsx("td", { children: isObj(r[c]) || Array.isArray(r[c]) ? _jsx("code", { children: JSON.stringify(r[c]) }) : _jsx(Scalar, { v: r[c], k: c }) }, c))) }, i))) })] }) }));
}
export function ValueView({ value, depth = 0, k }) {
    if (!isObj(value)) {
        if (Array.isArray(value)) {
            if (value.length && value.every(isObj))
                return _jsx(Table, { rows: value });
            return _jsx("span", { className: "mono", children: value.length ? value.map((x) => fmtScalar(x)).join(", ") : "—" });
        }
        return _jsx(Scalar, { v: value, k: k });
    }
    const entries = Object.entries(value);
    const simple = entries.filter(([, v]) => !isObj(v) && !(Array.isArray(v) && v.some(isObj)));
    const complex = entries.filter(([, v]) => isObj(v) || (Array.isArray(v) && v.some(isObj)));
    return (_jsxs("div", { className: "value-view", children: [simple.length > 0 && (_jsx("dl", { className: "kv", children: simple.map(([k, v]) => (_jsxs("div", { className: "kv-row", children: [_jsx("dt", { children: label(k) }), _jsx("dd", { children: _jsx(ValueView, { value: v, depth: depth + 1, k: k }) })] }, k))) })), complex.map(([k, v]) => (_jsxs("section", { className: "sub", children: [_jsx("h4", { children: label(k) }), _jsx(ValueView, { value: v, depth: depth + 1 })] }, k)))] }));
}
