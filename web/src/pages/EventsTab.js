import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useMemo, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { client, useStream } from "../api";
const MAX = 2000;
function kindClass(kind, data, msg = "") {
    if (kind === "port-oper" || kind === "port-admin")
        return msg.endsWith(" up") ? "ok" : "bad";
    if (kind === "rmep-down" || kind === "port-removed")
        return "bad";
    if (kind === "defects") {
        const h = data?.highest;
        if (h === "none")
            return "ok";
        return h === "DefRDICCM" ? "warn" : "bad";
    }
    if (kind.endsWith("-up") || kind.endsWith("-discovered") || kind.endsWith("-added"))
        return "ok";
    return "";
}
export function EventRow({ e, hideSource }) {
    const t = e.time ? timestampDate(e.time) : undefined;
    const data = e.data;
    const extra = data
        ? Object.entries(data)
            .filter(([k, v]) => v !== "" && v !== null && !(Array.isArray(v) && v.length === 0) && k !== "previous")
            .map(([k, v]) => `${k}=${Array.isArray(v) ? v.join(",") : String(v)}`)
            .join(" ")
        : "";
    return (_jsxs("div", { className: `event ${kindClass(e.kind, data, e.message)}`, children: [_jsx("span", { className: "ev-time mono", children: t ? t.toLocaleTimeString(undefined, { hour12: false }) + "." + String(t.getMilliseconds()).padStart(3, "0") : "" }), !hideSource && _jsx("span", { className: "ev-src mono", children: e.source }), _jsx("span", { className: "ev-kind mono", children: e.kind }), _jsxs("span", { className: "ev-msg", children: [e.message, extra && _jsxs("span", { className: "ev-data mono", children: [" ", extra] })] })] }));
}
export function EventsTab({ agentId }) {
    const [events, setEvents] = useState([]);
    const [filter, setFilter] = useState("");
    const [paused, setPaused] = useState(false);
    const [queued, setQueued] = useState([]);
    const pausedRef = useRef(paused);
    pausedRef.current = paused;
    const st = useStream((signal) => {
        setEvents([]);
        return client(agentId).watchEvents({ replay: true }, { signal });
    }, (e) => (pausedRef.current ? setQueued((q) => [e, ...q].slice(0, MAX)) : setEvents((x) => [e, ...x].slice(0, MAX))), [agentId]);
    const shown = useMemo(() => {
        const f = filter.trim().toLowerCase();
        if (!f)
            return events;
        return events.filter((e) => `${e.source} ${e.kind} ${e.message} ${JSON.stringify(e.data ?? {})}`.toLowerCase().includes(f));
    }, [events, filter]);
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Events" }), _jsxs("div", { className: "btn-row", children: [_jsx("span", { className: `live ${st.connected ? "on" : ""}`, children: st.connected ? "live" : st.error ? "reconnecting…" : "connecting…" }), _jsx("input", { className: "input search", placeholder: "Filter (source, kind, text)", value: filter, onChange: (e) => setFilter(e.target.value) }), _jsx("button", { className: "btn", onClick: () => {
                                    if (paused) {
                                        setEvents((x) => [...queued, ...x].slice(0, MAX));
                                        setQueued([]);
                                    }
                                    setPaused(!paused);
                                }, children: paused ? `Resume${queued.length ? ` (${queued.length})` : ""}` : "Pause" }), _jsx("button", { className: "btn", onClick: () => setEvents([]), children: "Clear" })] })] }), _jsx("div", { className: "events", children: shown.length === 0 ? _jsxs("div", { className: "muted pad", children: ["No events", filter ? " match the filter" : " yet", "."] }) : shown.map((e, i) => _jsx(EventRow, { e: e }, i)) })] }));
}
