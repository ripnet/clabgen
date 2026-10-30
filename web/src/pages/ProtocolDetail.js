import { jsx as _jsx, Fragment as _Fragment, jsxs as _jsxs } from "react/jsx-runtime";
import { useMemo, useState } from "react";
import { client, errorText, usePoll, useStream } from "../api";
import { href, navigate } from "../router";
import { restartNote, summarize, viewFor } from "../protocols/registry";
import { SchemaForm, defaults, diff, parseSchema } from "../components/SchemaForm";
import { ValueView } from "../components/ValueView";
import { Empty, ErrorBox, HealthText, Modal, useBusy, useToast } from "../ui";
import { AgentHeader, Crumbs } from "./AgentPage";
import { EventRow } from "./EventsTab";
import { EnableToggle } from "./ProtocolsTab";
export function ProtocolDetail({ agentId, name }) {
    const c = client(agentId);
    const p = usePoll(() => c.getProtocol({ name }), [agentId, name], 1000);
    const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
    const proto = p.data?.protocol;
    const type = types.data?.types.find((t) => t.name === proto?.type);
    const section = type?.category === "traffic" ? "traffic" : "protocols";
    const [editing, setEditing] = useState(false);
    const { busy, run } = useBusy();
    const toast = useToast();
    const view = viewFor(proto?.type ?? "");
    const s = proto ? summarize(proto.type, proto.state, proto.error, proto.status) : undefined;
    return (_jsxs("div", { className: "page", children: [_jsx(AgentHeader, { agentId: agentId, children: _jsx(Crumbs, { agentId: agentId, current: name, trail: proto?.group
                        ? [
                            { label: "Groups", href: href({ page: "agent", agent: agentId, tab: "groups" }) },
                            { label: proto.group, href: href({ page: "group", agent: agentId, group: proto.group }) },
                        ]
                        : [{ label: section === "traffic" ? "Traffic" : "Protocols", href: href({ page: "agent", agent: agentId, tab: section }) }] }) }), p.error && !proto && _jsx(ErrorBox, { error: p.error }), proto && (_jsxs(_Fragment, { children: [_jsxs("div", { className: "proto-head", children: [_jsxs("div", { children: [_jsx("h2", { className: "mono", children: proto.name }), _jsxs("div", { className: "muted", children: [_jsx("span", { className: "mono", children: proto.type }), " on device ", _jsx("span", { className: "mono", children: proto.device }), proto.group && (_jsxs(_Fragment, { children: [" ", "\u00B7 member of group ", _jsx("a", { href: href({ page: "group", agent: agentId, group: proto.group }), className: "mono", children: proto.group })] }))] })] }), _jsx("div", { className: "proto-state", children: s && _jsx(HealthText, { health: s.health, children: proto.state === "running" ? s.text : proto.error ? `${proto.state}: ${proto.error}` : proto.state }) }), _jsxs("div", { className: "btn-row", hidden: !!proto.group, title: "Group members change through their group", children: [_jsx(EnableToggle, { disabled: proto.disabled, busy: busy, onToggle: () => run(() => c.setProtocolDisabled({ name: proto.name, disabled: !proto.disabled }), `${proto.disabled ? "Enabled" : "Disabled"} ${proto.name}`).then(p.refresh) }), _jsx("button", { className: "btn", onClick: () => setEditing(true), disabled: !type, children: "Edit config" }), _jsx("button", { className: "btn danger-ghost", disabled: busy, onClick: () => {
                                            if (confirm(`Delete protocol ${proto.name}?`))
                                                run(() => c.deleteProtocol({ name: proto.name }), `Deleted ${proto.name}`).then((r) => r && navigate({ page: "agent", agent: agentId, tab: type?.category === "traffic" ? "traffic" : "protocols" }));
                                        }, children: "Delete" })] })] }), _jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Status" }), _jsx("span", { className: "live on", children: "live" })] }), proto.status ? view.Status ? _jsx(view.Status, { status: proto.status, agentId: agentId, name: proto.name }) : _jsx(ValueView, { value: proto.status }) : _jsx("div", { className: "muted", children: proto.disabled ? "Disabled: the configuration is kept; turn it on to start it again." : "Not running." })] }), _jsxs("div", { className: "detail-grid", children: [type && type.actions.length > 0 ? (_jsx(Actions, { agentId: agentId, name: proto.name, actions: type.actions, disabled: proto.state !== "running" })) : (_jsx("div", {})), _jsx(ProtocolEvents, { agentId: agentId, name: proto.name })] }), _jsxs("details", { className: "card config-card", children: [_jsx("summary", { children: _jsx("h2", { children: "Configuration" }) }), _jsx(ValueView, { value: proto.config ?? {} })] }), editing && type && (_jsx(EditConfig, { schemaText: type.configSchema, type: proto.type, agentId: agentId, device: proto.device, value: (proto.config ?? {}), onClose: () => setEditing(false), onSave: async (cfg) => {
                            await c.updateProtocol({ name: proto.name, config: cfg });
                            toast("ok", "Configuration applied");
                            setEditing(false);
                            p.refresh();
                        } }))] }))] }));
}
function Note({ note }) {
    return _jsx("div", { className: note.restart ? "warn-box" : "info-box", children: note.text });
}
// EditConfig edits a running instance's config: the form, then a review of
// the changes before applying. A rejected config comes back to the form
// with the fields the error names marked.
function EditConfig({ schemaText, type, agentId, device, value, onClose, onSave, }) {
    const Editor = viewFor(type).ConfigEditor;
    const schema = useMemo(() => parseSchema(schemaText), [schemaText]);
    const [original] = useState(value);
    const [cfg, setCfg] = useState(value);
    const [step, setStep] = useState("edit");
    const [error, setError] = useState();
    const [busy, setBusy] = useState(false);
    const changes = useMemo(() => diff(schema, original, cfg), [schema, original, cfg]);
    const note = (viewFor(type).applyNote ?? (() => restartNote))(original, cfg);
    const apply = async () => {
        setBusy(true);
        try {
            await onSave(cfg);
        }
        catch (e) {
            setError(errorText(e));
            setStep("edit");
        }
        finally {
            setBusy(false);
        }
    };
    return (_jsxs(Modal, { title: step === "edit" ? "Edit configuration" : "Review changes", onClose: onClose, wide: Editor ? "x" : true, children: [_jsxs("div", { hidden: step !== "edit", children: [error ? _jsxs("div", { className: "error-box", role: "alert", children: ["The agent rejected the configuration: ", error] }) : _jsx(Note, { note: note }), Editor ? (_jsx(Editor, { schema: schema, value: cfg, onChange: setCfg, original: original, errorText: error, agentId: agentId, device: device })) : (_jsx(SchemaForm, { schema: schema, value: cfg, onChange: setCfg, mode: "config", original: original, errorText: error }))] }), step === "review" && (_jsxs(_Fragment, { children: [_jsx(Note, { note: note }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table diff-table", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Field" }), _jsx("th", { children: "Was" }), _jsx("th", { children: "Now" })] }) }), _jsx("tbody", { children: changes.map((ch) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: ch.path }), _jsx("td", { className: "mono diff-from", children: ch.from ?? _jsx("span", { className: "muted", children: /\]$/.test(ch.path) ? "new" : "default" }) }), _jsx("td", { className: "mono diff-to", children: ch.to ?? _jsx("span", { className: "muted", children: /\]$/.test(ch.path) ? "removed" : "default" }) })] }, ch.path))) })] }) })] })), _jsxs("div", { className: "modal-foot", children: [step === "review" && (_jsx("button", { className: "btn", onClick: () => setStep("edit"), children: "Back" })), _jsx("span", { className: "grow" }), _jsx("button", { className: "btn", onClick: onClose, children: "Cancel" }), step === "edit" ? (_jsxs("button", { className: "btn primary", disabled: changes.length === 0, title: changes.length === 0 ? "Nothing changed" : undefined, onClick: () => setStep("review"), children: ["Review ", changes.length > 0 ? `${changes.length} change${changes.length > 1 ? "s" : ""}` : "changes"] })) : (_jsx("button", { className: "btn primary", disabled: busy, onClick: apply, children: busy ? "Applying…" : "Apply" }))] })] }));
}
function Actions({ agentId, name, actions, disabled }) {
    const [sel, setSel] = useState(actions[0].name);
    const action = actions.find((a) => a.name === sel) ?? actions[0];
    const schema = useMemo(() => parseSchema(action.argsSchema), [action.argsSchema]);
    const [argsBy, setArgsBy] = useState({});
    const args = argsBy[action.name] ?? defaults(schema);
    const [running, setRunning] = useState(false);
    const [results, setResults] = useState([]);
    const runAction = async () => {
        setRunning(true);
        const t0 = performance.now();
        const base = { id: Date.now(), action: action.name, args, at: new Date() };
        try {
            const r = await client(agentId).runProtocolAction({ name, action: action.name, args: args });
            setResults((x) => [{ ...base, ms: performance.now() - t0, result: r.result }, ...x].slice(0, 10));
        }
        catch (e) {
            setResults((x) => [{ ...base, ms: performance.now() - t0, error: errorText(e) }, ...x].slice(0, 10));
        }
        finally {
            setRunning(false);
        }
    };
    return (_jsxs("div", { className: "card", children: [_jsx("div", { className: "card-head", children: _jsx("h2", { children: "Actions" }) }), _jsx("div", { className: "seg", role: "tablist", children: actions.map((a) => (_jsx("button", { role: "tab", "aria-selected": a.name === action.name, className: a.name === action.name ? "seg-btn active" : "seg-btn", onClick: () => setSel(a.name), children: a.name }, a.name))) }), _jsx("p", { className: "muted action-desc", children: action.description }), _jsxs("form", { onSubmit: (e) => {
                    e.preventDefault();
                    runAction();
                }, children: [_jsx(SchemaForm, { schema: schema, value: args, onChange: (v) => setArgsBy((m) => ({ ...m, [action.name]: v })), mode: "args" }), _jsxs("div", { className: "btn-row end", children: [_jsx("button", { type: "button", className: "btn", onClick: () => setArgsBy((m) => ({ ...m, [action.name]: defaults(schema) })), children: "Reset" }), _jsx("button", { className: "btn primary", disabled: running || disabled, children: running ? "Running…" : `Run ${action.name}` })] })] }), results.map((r, i) => (_jsxs("div", { className: `result ${i > 0 ? "old" : ""}`, children: [_jsxs("div", { className: "result-head", children: [_jsx("span", { className: "mono strong", children: r.action }), _jsxs("span", { className: "muted", children: [r.at.toLocaleTimeString(), " \u00B7 ", (r.ms / 1000).toFixed(2), " s"] })] }), r.error ? _jsx("div", { className: "error-box", children: r.error }) : i === 0 ? _jsx(ValueView, { value: r.result ?? {} }) : _jsx(ResultSummary, { r: r.result })] }, r.id)))] }));
}
function ResultSummary({ r }) {
    if (!r)
        return null;
    const parts = Object.entries(r)
        .filter(([, v]) => typeof v !== "object")
        .slice(0, 6)
        .map(([k, v]) => `${k.replace(/_/g, " ")}: ${String(v)}`);
    return _jsx("div", { className: "muted mono small", children: parts.join(" · ") });
}
function ProtocolEvents({ agentId, name }) {
    const [events, setEvents] = useState([]);
    const source = `protocol/${name}`;
    useStream((signal) => {
        setEvents([]); // the stream replays history on every (re)connect
        return client(agentId).watchEvents({ replay: true }, { signal });
    }, (e) => e.source === source && setEvents((x) => [e, ...x].slice(0, 50)), [agentId, name]);
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Recent events" }), _jsx("a", { className: "muted", href: href({ page: "agent", agent: agentId, tab: "events" }), children: "all events \u2192" })] }), events.length === 0 ? (_jsx(Empty, { title: "No events yet" })) : (_jsx("div", { className: "events compact", children: events.map((e, i) => (_jsx(EventRow, { e: e, hideSource: true }, i))) }))] }));
}
