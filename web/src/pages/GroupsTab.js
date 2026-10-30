import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useMemo, useState } from "react";
import { client, errorText, usePoll } from "../api";
import { href, navigate } from "../router";
import { SchemaForm, defaults, parseSchema } from "../components/SchemaForm";
import { ValueView } from "../components/ValueView";
import { Empty, ErrorBox, HealthText, useBusy } from "../ui";
import { AgentHeader, Crumbs } from "./AgentPage";
import { GroupEditor } from "./GroupEditor";
import { EnableToggle } from "./ProtocolsTab";
// throwFailed turns per-member failures of a group change into an error.
function throwFailed(r) {
    const failed = r.results.filter((x) => x.error);
    if (failed.length)
        throw new Error(`${failed.length} of ${r.results.length} failed: ${failed.slice(0, 3).map((f) => `${f.protocol}: ${f.error}`).join("; ")}`);
    return r;
}
// Group-wide pseudo-actions, next to the protocol type's own.
const toggleActions = [
    { name: "disable", description: "Stop the protocol on the selected members, keeping their config. On every member it is kept in the group spec; on some, until they are enabled again.", argsSchema: "" },
    { name: "enable", description: "Start the protocol again on the selected members.", argsSchema: "" },
];
const goodWords = /^(established|up|ok|running|passive|\d+ neighbors?)$/;
const idleWords = /^(stopped|idle|no peers)$/;
export function wordHealth(state, health) {
    if (state === "error")
        return "bad";
    if (state !== "running")
        return "idle";
    if (!health || goodWords.test(health))
        return "ok";
    return idleWords.test(health) ? "idle" : "warn";
}
export function countsText(c) {
    return Object.entries(c ?? {})
        .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
        .map(([k, n]) => `${n} ${k}`)
        .join(", ");
}
const sum = (c) => Object.values(c ?? {}).reduce((a, b) => a + b, 0);
// groupHealth rolls a group protocol's counts into one health: ok when
// every member is running and healthy, bad when none is. Disabled members
// don't count against it.
export function groupHealth(p) {
    const total = sum(p.states) - (p.states.disabled ?? 0);
    const running = p.states.running ?? 0;
    if (total === 0)
        return { health: "idle", text: countsText(p.states) };
    if (p.states.error)
        return { health: "bad", text: `${p.states.error} failed: ${countsText(p.states)}` };
    if (!running)
        return { health: "idle", text: countsText(p.states) };
    if (!p.health)
        return { health: running === total ? "ok" : "warn", text: countsText(p.states) };
    const entries = Object.entries(p.health);
    const good = entries.filter(([k]) => goodWords.test(k)).reduce((a, [, n]) => a + n, 0);
    const idle = entries.filter(([k]) => idleWords.test(k)).reduce((a, [, n]) => a + n, 0);
    let text = countsText(p.health);
    if (running < sum(p.states))
        text += ` (${countsText(p.states)})`;
    if (good === total)
        return { health: "ok", text };
    if (idle === total)
        return { health: "idle", text };
    return { health: good > 0 ? "warn" : "bad", text };
}
export function groupStatus(g) {
    const st = (g.status ?? {});
    const count = Number(g.spec?.count ?? 0);
    return { devices: st.devices ?? 0, count, protocols: st.protocols ?? [], members: st.members ?? [] };
}
export function GroupsTab({ agentId }) {
    const c = client(agentId);
    const list = usePoll(() => c.listDeviceGroups({}), [agentId], 2000);
    const [editing, setEditing] = useState();
    const { busy, run } = useBusy();
    const groups = list.data?.groups ?? [];
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Device groups" }), _jsx("button", { className: "btn primary", onClick: () => setEditing("new"), children: "New group" })] }), _jsx("p", { className: "muted", children: "A group is many devices from one template (VLANs, MACs and addresses stepped per member), each running the same protocols. Members are shown here rather than in the Devices and Protocols tabs." }), _jsx(ErrorBox, { error: list.error }), !list.loading && groups.length === 0 ? (_jsx(Empty, { title: "No device groups", children: "Create one, or add device_groups to a scenario." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table hover", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Name" }), _jsx("th", { children: "Devices" }), _jsx("th", { children: "Protocols" }), _jsx("th", {})] }) }), _jsx("tbody", { children: groups.map((g) => {
                                const s = groupStatus(g);
                                return (_jsxs("tr", { onClick: () => navigate({ page: "group", agent: agentId, group: g.name }), children: [_jsx("td", { className: "mono strong", children: _jsx("a", { href: href({ page: "group", agent: agentId, group: g.name }), onClick: (e) => e.stopPropagation(), children: g.name }) }), _jsxs("td", { className: "mono", children: [s.devices, "/", s.count] }), _jsxs("td", { children: [s.protocols.map((p) => {
                                                    const h = groupHealth(p);
                                                    return (_jsx("div", { children: _jsxs(HealthText, { health: h.health, children: [_jsx("span", { className: "mono", children: p.name }), ": ", h.text] }) }, p.name));
                                                }), s.protocols.length === 0 && _jsx("span", { className: "muted", children: "\u2014" })] }), _jsxs("td", { className: "actions", children: [_jsx("button", { className: "btn small", disabled: busy, onClick: (e) => {
                                                        e.stopPropagation();
                                                        setEditing(g);
                                                    }, children: "Edit" }), _jsx("button", { className: "btn small danger-ghost", disabled: busy, onClick: (e) => {
                                                        e.stopPropagation();
                                                        if (confirm(`Delete group ${g.name} and its ${s.devices} devices?`))
                                                            run(() => c.deleteDeviceGroup({ name: g.name }), `Deleted ${g.name}`).then(list.refresh);
                                                    }, children: "Delete" })] })] }, g.name));
                            }) })] }) })), editing && _jsx(GroupEditor, { agentId: agentId, group: editing === "new" ? undefined : editing, onClose: () => setEditing(undefined), onDone: list.refresh })] }));
}
const pageSize = 100;
export function GroupDetail({ agentId, name }) {
    const c = client(agentId);
    const g = usePoll(() => c.getDeviceGroup({ name }), [agentId, name], 2000);
    const [editing, setEditing] = useState(false);
    const [filter, setFilter] = useState("");
    const [page, setPage] = useState(0);
    const { busy, run } = useBusy();
    const group = g.data?.group;
    const s = group ? groupStatus(group) : undefined;
    const members = useMemo(() => {
        const f = filter.trim().toLowerCase();
        if (!s || !f)
            return s?.members ?? [];
        return s.members.filter((m) => m.device.toLowerCase().includes(f) ||
            m.vlans.includes(f) ||
            (m.addresses ?? []).some((a) => a.includes(f)) ||
            (m.protocols ?? []).some((p) => (p.health || p.state).toLowerCase().includes(f)));
    }, [s, filter]);
    const pages = Math.max(1, Math.ceil(members.length / pageSize));
    const shown = members.slice(page * pageSize, (page + 1) * pageSize);
    return (_jsxs("div", { className: "page", children: [_jsx(AgentHeader, { agentId: agentId, children: _jsx(Crumbs, { agentId: agentId, current: name, trail: [{ label: "Groups", href: href({ page: "agent", agent: agentId, tab: "groups" }) }] }) }), g.error && !group && _jsx(ErrorBox, { error: g.error }), group && s && (_jsxs(_Fragment, { children: [_jsxs("div", { className: "proto-head", children: [_jsxs("div", { children: [_jsx("h2", { className: "mono", children: group.name }), _jsxs("div", { className: "muted", children: [s.devices, "/", s.count, " devices"] })] }), _jsx("div", { className: "proto-state", children: s.protocols.map((p) => {
                                    const h = groupHealth(p);
                                    return (_jsxs("div", { className: "group-proto", children: [_jsx(EnableToggle, { disabled: !!p.disabled, busy: busy, title: p.disabled ? `Enable ${p.name} on every member` : `Disable ${p.name} on every member, keeping its config`, onToggle: () => {
                                                    if (!p.disabled && !confirm(`Disable ${p.name} on all ${s.count} members of ${group.name}?`))
                                                        return;
                                                    run(() => c.setGroupDisabled({ group: group.name, protocol: p.name, disabled: !p.disabled }).then(throwFailed), `${p.disabled ? "Enabled" : "Disabled"} ${p.name} on ${group.name}`).then(g.refresh);
                                                } }), _jsxs(HealthText, { health: h.health, children: [_jsx("span", { className: "mono", children: p.name }), " (", p.type, "): ", h.text] })] }, p.name));
                                }) }), _jsxs("div", { className: "btn-row", children: [_jsx("button", { className: "btn", onClick: () => setEditing(true), children: "Edit" }), _jsx("button", { className: "btn danger-ghost", disabled: busy, onClick: () => {
                                            if (confirm(`Delete group ${group.name} and its ${s.devices} devices?`))
                                                run(() => c.deleteDeviceGroup({ name: group.name }), `Deleted ${group.name}`).then((r) => r && navigate({ page: "agent", agent: agentId, tab: "groups" }));
                                        }, children: "Delete" })] })] }), s.protocols.length > 0 && _jsx(GroupActions, { agentId: agentId, group: group.name, protocols: s.protocols, count: s.count }), _jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Members" }), _jsxs("div", { className: "btn-row", children: [_jsx("input", { className: "input", style: { width: 260 }, placeholder: "Filter: name, VLAN, address, state", value: filter, onChange: (e) => {
                                                    setFilter(e.target.value);
                                                    setPage(0);
                                                } }), pages > 1 && (_jsxs(_Fragment, { children: [_jsx("button", { className: "btn small", disabled: page === 0, onClick: () => setPage(page - 1), children: "\u2039" }), _jsxs("span", { className: "muted small", children: [page * pageSize + 1, "\u2013", Math.min(members.length, (page + 1) * pageSize), " of ", members.length] }), _jsx("button", { className: "btn small", disabled: page >= pages - 1, onClick: () => setPage(page + 1), children: "\u203A" })] }))] })] }), _jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table dense", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "#" }), _jsx("th", { children: "Device" }), _jsx("th", { children: "VLANs" }), _jsx("th", { children: "MAC" }), _jsx("th", { children: "Addresses" }), s.protocols.map((p) => (_jsx("th", { children: p.name }, p.name)))] }) }), _jsx("tbody", { children: shown.map((m) => (_jsxs("tr", { children: [_jsx("td", { className: "mono muted", children: m.index }), _jsx("td", { className: "mono strong", children: m.device }), _jsx("td", { className: "mono", children: m.vlans }), _jsx("td", { className: "mono", children: m.mac || "—" }), _jsx("td", { className: "mono", children: (m.addresses ?? []).join(", ") || "—" }), (m.protocols ?? []).map((p) => (_jsx("td", { children: _jsx("a", { href: href({ page: "protocol", agent: agentId, protocol: p.name }), title: p.error || p.name, children: _jsx(HealthText, { health: wordHealth(p.state, p.health), children: p.state === "running" ? p.health || "running" : p.state }) }) }, p.name)))] }, m.device))) })] }) }), members.length === 0 && _jsx("div", { className: "muted", children: "No member matches." })] }), editing && _jsx(GroupEditor, { agentId: agentId, group: group, onClose: () => setEditing(false), onDone: g.refresh })] }))] }));
}
function GroupActions({ agentId, group, protocols, count }) {
    const types = usePoll(() => client(agentId).listProtocolTypes({}), [agentId]);
    const [protoName, setProtoName] = useState(protocols[0].name);
    const proto = protocols.find((p) => p.name === protoName) ?? protocols[0];
    const typeActions = types.data?.types.find((t) => t.name === proto.type)?.actions;
    const actions = typeActions ? [...typeActions, ...toggleActions] : [];
    const [sel, setSel] = useState("");
    const action = actions.find((a) => a.name === sel) ?? actions[0];
    const schema = useMemo(() => parseSchema(action?.argsSchema), [action?.argsSchema]);
    const [argsBy, setArgsBy] = useState({});
    const key = proto.type + "/" + (action?.name ?? "");
    const args = argsBy[key] ?? defaults(schema);
    const [members, setMembers] = useState("");
    const [random, setRandom] = useState("");
    const [running, setRunning] = useState(false);
    const [results, setResults] = useState([]);
    if (!action)
        return null;
    const runAction = async () => {
        setRunning(true);
        const t0 = performance.now();
        const base = { id: Date.now(), action: `${proto.name} ${action.name}`, at: new Date() };
        try {
            const sel = { group, protocol: proto.name, members, random: Number(random) || 0 };
            const toggle = toggleActions.some((a) => a.name === action.name);
            const r = toggle
                ? await client(agentId).setGroupDisabled({ ...sel, disabled: action.name === "disable" })
                : await client(agentId).runGroupAction({ ...sel, action: action.name, args: args });
            setResults((x) => [{ ...base, ms: performance.now() - t0, results: r.results }, ...x].slice(0, 5));
        }
        catch (e) {
            setResults((x) => [{ ...base, ms: performance.now() - t0, error: errorText(e) }, ...x].slice(0, 5));
        }
        finally {
            setRunning(false);
        }
    };
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Actions" }), protocols.length > 1 && (_jsx("div", { className: "seg", children: protocols.map((p) => (_jsx("button", { className: p.name === proto.name ? "seg-btn active" : "seg-btn", onClick: () => setProtoName(p.name), children: p.name }, p.name))) }))] }), _jsx("div", { className: "seg", role: "tablist", children: actions.map((a) => (_jsx("button", { role: "tab", "aria-selected": a.name === action.name, className: a.name === action.name ? "seg-btn active" : "seg-btn", onClick: () => setSel(a.name), children: a.name }, a.name))) }), _jsx("p", { className: "muted action-desc", children: action.description }), _jsxs("form", { onSubmit: (e) => {
                    e.preventDefault();
                    runAction();
                }, children: [_jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "members" }), _jsx("input", { className: "input mono", placeholder: `all (1-${count})`, value: members, onChange: (e) => setMembers(e.target.value) }), _jsx("div", { className: "hint", children: "1-based indices and ranges, e.g. 1-10,15" })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "random" }), _jsx("input", { className: "input mono", type: "number", min: 0, placeholder: "0", value: random, onChange: (e) => setRandom(e.target.value) }), _jsx("div", { className: "hint", children: "Run on this many of the selected members, picked at random" })] })] }), action.argsSchema && (_jsxs(_Fragment, { children: [_jsx("h4", { children: "Arguments" }), _jsx(SchemaForm, { schema: schema, value: args, onChange: (v) => setArgsBy((m) => ({ ...m, [key]: v })), mode: "args" })] })), _jsx("div", { className: "btn-row end", children: _jsx("button", { className: "btn primary", disabled: running, children: running ? "Running…" : action.name === "disable" ? "Disable" : action.name === "enable" ? "Enable" : `Run ${action.name}` }) })] }), results.map((r, i) => {
                const failed = (r.results ?? []).filter((x) => x.error);
                return (_jsxs("div", { className: `result ${i > 0 ? "old" : ""}`, children: [_jsxs("div", { className: "result-head", children: [_jsx("span", { className: "mono strong", children: r.action }), _jsxs("span", { className: "muted", children: [r.at.toLocaleTimeString(), " \u00B7 ", (r.ms / 1000).toFixed(2), " s"] })] }), r.error ? (_jsx("div", { className: "error-box", children: r.error })) : (_jsxs(_Fragment, { children: [_jsxs(HealthText, { health: failed.length ? (failed.length === r.results?.length ? "bad" : "warn") : "ok", children: [r.results?.length, " members: ", (r.results?.length ?? 0) - failed.length, " ok, ", failed.length, " failed"] }), failed.slice(0, 20).map((f) => (_jsxs("div", { className: "mono small error-text", children: [f.protocol, ": ", f.error] }, f.index))), i === 0 && (r.results?.length ?? 0) > 0 && (_jsxs("details", { children: [_jsx("summary", { className: "muted small", children: "Results per member" }), _jsx(ValueView, { value: (r.results ?? []).map((x) => ({ member: x.index, protocol: x.protocol, error: x.error, ...(x.result ?? {}) })) })] }))] }))] }, r.id));
            })] }));
}
