import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useState } from "react";
const stepKeys = new Set(["start", "step", "repeat", "cycle"]);
export function isStep(v) {
    if (!v || typeof v !== "object" || Array.isArray(v))
        return false;
    const ks = Object.keys(v);
    return ks.includes("start") && ks.every((k) => stepKeys.has(k));
}
// fmtStep is a one-line description, e.g. "101 +1 per member".
export function fmtStep(s) {
    let out = `${s.start} +${s.step ?? 1}`;
    out += s.repeat && s.repeat > 1 ? ` every ${s.repeat} members` : " per member";
    if (s.cycle)
        out += `, ${s.cycle} values`;
    return out;
}
// A step typed as text: an integer is a number, anything else (0.0.0.4,
// ::1:0, 00:00:00:00:01:00) a string.
const parseStep = (t) => {
    const s = t.trim();
    if (s === "")
        return undefined;
    return /^-?\d+$/.test(s) ? Number(s) : s;
};
const posInt = (t) => {
    const n = Number(t);
    return t.trim() !== "" && Number.isInteger(n) && n >= 1 ? n : undefined;
};
// StepInput edits a value that may be stepped per member. A plain value
// shows its own input (children) and a "per member" switch; a stepped one
// shows start and step, with repeat and cycle behind "more".
export function StepInput({ value, onChange, numeric, label, children, }) {
    const stepped = isStep(value);
    const [more, setMore] = useState(stepped && (!!value.repeat || !!value.cycle));
    const toggle = (_jsx("button", { type: "button", className: `step-toggle${stepped ? " on" : ""}`, title: stepped ? "Same value for every member" : "Step per member", "aria-label": stepped ? `${label}: same for every member` : `${label}: step per member`, "aria-pressed": stepped, onClick: () => {
            if (stepped)
                return onChange(value.start === "" ? undefined : value.start);
            const start = value === undefined || value === null || value === "" ? (numeric ? 1 : "") : value;
            onChange({ start });
        }, children: "\u00B1" }));
    if (!stepped)
        return (_jsxs("div", { className: "step-field", children: [children, toggle] }));
    const set = (k, v) => {
        const next = { ...value };
        if (v === undefined)
            delete next[k];
        else
            next[k] = v;
        onChange(next);
    };
    return (_jsxs("div", { className: "step-field stepped", children: [_jsxs("div", { className: "step-row", children: [_jsx("input", { className: "input mono", "aria-label": `${label} start`, placeholder: "start", type: numeric ? "number" : "text", value: String(value.start ?? ""), onChange: (e) => set("start", numeric ? (e.target.value === "" ? "" : Number(e.target.value)) : e.target.value) }), _jsx("span", { className: "step-plus", children: "+" }), _jsx("input", { className: "input mono step-step", "aria-label": `${label} step`, placeholder: "1", value: value.step === undefined ? "" : String(value.step), onChange: (e) => set("step", parseStep(e.target.value)) }), toggle] }), more ? (_jsxs("div", { className: "step-row step-more", children: [_jsxs("label", { children: ["every", _jsx("input", { className: "input mono", "aria-label": `${label} repeat`, placeholder: "1", value: value.repeat ?? "", onChange: (e) => set("repeat", posInt(e.target.value)) }), "members"] }), _jsxs("label", { children: ["wrap after", _jsx("input", { className: "input mono", "aria-label": `${label} cycle`, placeholder: "never", value: value.cycle ?? "", onChange: (e) => set("cycle", posInt(e.target.value)) }), "values"] })] })) : (_jsx("button", { type: "button", className: "link small", onClick: () => setMore(true), children: "repeat / wrap\u2026" }))] }));
}
