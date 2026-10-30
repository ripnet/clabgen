import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useMemo, useState } from "react";
import { client, errorText, usePoll } from "../api";
import { href, navigate } from "../router";
import { summarize, viewFor } from "../protocols/registry";
import { SchemaForm, defaults, parseSchema } from "../components/SchemaForm";
import { Empty, ErrorBox, HealthText, Modal, useBusy } from "../ui";
export function ProtocolsTab({ agentId }) {
    const c = client(agentId);
    const list = usePoll(() => c.listProtocols({ withStatus: true, brief: true, noGroupMembers: true }), [agentId], 1500);
    const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
    const [creating, setCreating] = useState(false);
    const { busy, run } = useBusy();
    // Traffic flows have their own tab.
    const traffic = new Set(types.data?.types.filter((t) => t.category === "traffic").map((t) => t.name));
    const protos = (list.data?.protocols ?? []).filter((p) => !traffic.has(p.type));
    const toggleAll = (disabled) => {
        const names = protos.filter((p) => p.disabled !== disabled).map((p) => p.name);
        if (disabled && !confirm(`Disable ${names.length} protocol${names.length > 1 ? "s" : ""}? They stop but keep their config.`))
            return;
        run(async () => {
            const res = await Promise.allSettled(names.map((name) => c.setProtocolDisabled({ name, disabled })));
            const failed = names.filter((_, i) => res[i].status === "rejected");
            if (failed.length)
                throw new Error(`Could not ${disabled ? "disable" : "enable"} ${failed.join(", ")}: ${errorText(res.find((r) => r.status === "rejected").reason)}`);
        }, `${disabled ? "Disabled" : "Enabled"} ${names.length} protocol${names.length > 1 ? "s" : ""}`).then(list.refresh);
    };
    const nOn = protos.filter((p) => !p.disabled).length;
    const nOff = protos.length - nOn;
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Protocols" }), _jsxs("div", { className: "btn-row", children: [nOff > 0 && (_jsxs("button", { className: "btn", disabled: busy, onClick: () => toggleAll(false), title: "Start every disabled protocol on this agent", children: ["Enable all (", nOff, ")"] })), nOn > 0 && (_jsxs("button", { className: "btn", disabled: busy, onClick: () => toggleAll(true), title: "Stop every protocol on this agent, keeping their config", children: ["Disable all (", nOn, ")"] })), _jsx("button", { className: "btn primary", onClick: () => setCreating(true), children: "New protocol" })] })] }), _jsx(ErrorBox, { error: list.error }), !list.loading && protos.length === 0 ? (_jsx(Empty, { title: "No protocols yet", children: "Create one, or apply a scenario from the Scenario tab." })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table hover", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Name" }), _jsx("th", { title: "Disabled protocols keep their config but don't run", children: "Enabled" }), _jsx("th", { children: "Type" }), _jsx("th", { children: "Device" }), _jsx("th", { children: "State" }), _jsx("th", {})] }) }), _jsx("tbody", { children: protos.map((p) => {
                                const s = summarize(p.type, p.state, p.error, p.status);
                                return (_jsxs("tr", { onClick: () => navigate({ page: "protocol", agent: agentId, protocol: p.name }), children: [_jsx("td", { className: "mono strong", children: _jsx("a", { href: href({ page: "protocol", agent: agentId, protocol: p.name }), onClick: (e) => e.stopPropagation(), children: p.name }) }), _jsx("td", { children: _jsx(EnableToggle, { disabled: p.disabled, busy: busy, onToggle: () => run(() => c.setProtocolDisabled({ name: p.name, disabled: !p.disabled }), `${p.disabled ? "Enabled" : "Disabled"} ${p.name}`).then(list.refresh) }) }), _jsx("td", { className: "mono", children: p.type }), _jsx("td", { className: "mono", children: p.device }), _jsx("td", { children: _jsx(HealthText, { health: s.health, children: s.text }) }), _jsx("td", { className: "actions", children: _jsx("button", { className: "btn small danger-ghost", disabled: busy, onClick: (e) => {
                                                    e.stopPropagation();
                                                    if (confirm(`Delete protocol ${p.name}?`))
                                                        run(() => c.deleteProtocol({ name: p.name }), `Deleted ${p.name}`).then(list.refresh);
                                                }, children: "Delete" }) })] }, p.name));
                            }) })] }) })), creating && _jsx(CreateProtocol, { agentId: agentId, category: "protocol", onClose: () => setCreating(false), onDone: list.refresh })] }));
}
// EnableToggle switches a protocol instance on (running) or off (disabled,
// config kept).
export function EnableToggle({ disabled, busy, onToggle, title }) {
    return (_jsxs("button", { className: `toggle ${disabled ? "" : "on"}`, disabled: busy, "aria-pressed": !disabled, title: title ?? (disabled ? "Enable: start it again" : "Disable: stop it, keeping its config"), onClick: (e) => {
            e.stopPropagation();
            onToggle();
        }, children: [_jsx("span", { className: "toggle-knob" }), _jsx("span", { children: disabled ? "off" : "on" })] }));
}
// CreateProtocol creates an instance of a type in one category ("protocol"
// or "traffic"). A preset fills in the type, name, device and config fields
// (e.g. the Ping… shortcut on devices).
export function CreateProtocol({ agentId, category, preset, onClose, onDone, }) {
    const c = client(agentId);
    const allTypes = usePoll(() => c.listProtocolTypes({}), [agentId]);
    const devices = usePoll(() => c.listDevices({}), [agentId]);
    const [type, setType] = useState(preset?.type ?? "");
    const [name, setName] = useState(preset?.name ?? "");
    const [device, setDevice] = useState(preset?.device ?? "");
    const [cfg, setCfg] = useState({});
    const { busy, run } = useBusy();
    const types = (allTypes.data?.types ?? []).filter((x) => (x.category || "protocol") === category);
    const t = types.find((x) => x.name === (type || types[0]?.name));
    const schema = useMemo(() => parseSchema(t?.configSchema), [t?.configSchema]);
    const [cfgFor, setCfgFor] = useState("");
    if (t && cfgFor !== t.name) {
        setCfgFor(t.name);
        setCfg({ ...defaults(schema), ...(t.name === preset?.type ? preset?.config : {}) });
    }
    const dev = device || devices.data?.devices[0]?.name || "";
    const Editor = t ? viewFor(t.name).ConfigEditor : undefined;
    return (_jsxs(Modal, { title: preset?.title ?? (category === "traffic" ? "New flow" : "New protocol"), onClose: onClose, wide: Editor ? "x" : true, children: [_jsx(ErrorBox, { error: allTypes.error || devices.error }), devices.data && devices.data.devices.length === 0 && (_jsx("div", { className: "warn-box", children: "This agent has no devices yet. Create one in the Devices tab first." })), _jsxs("form", { onSubmit: (e) => {
                    e.preventDefault();
                    if (!t)
                        return;
                    run(() => c.createProtocol({ protocol: { name, type: t.name, device: dev, config: cfg } }), `Created ${name}`).then((r) => {
                        if (!r)
                            return;
                        onDone();
                        onClose();
                        navigate({ page: "protocol", agent: agentId, protocol: name });
                    });
                }, children: [_jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsxs("label", { className: "field-label", children: ["name", _jsx("span", { className: "req", children: "*" })] }), _jsx("input", { className: "input mono", value: name, onChange: (e) => setName(e.target.value), required: true, autoFocus: true, pattern: "[A-Za-z0-9][A-Za-z0-9_.:\\-]{0,62}" })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "type" }), _jsx("select", { className: "input", value: t?.name ?? "", onChange: (e) => setType(e.target.value), children: types.map((x) => (_jsx("option", { value: x.name, children: x.name }, x.name))) }), t && _jsx("div", { className: "hint", children: t.description })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "device" }), _jsx("select", { className: "input", value: dev, onChange: (e) => setDevice(e.target.value), children: devices.data?.devices.map((d) => (_jsxs("option", { value: d.name, children: [d.name, " (", d.port, d.vlans.length ? " · " + d.vlans.map((v) => v.vid).join(".") : "", ")"] }, d.name))) })] })] }), _jsx("h4", { children: "Configuration" }), Editor && t ? (_jsx(Editor, { schema: schema, value: cfg, onChange: setCfg, agentId: agentId, device: dev })) : (_jsx(SchemaForm, { schema: schema, value: cfg, onChange: setCfg, mode: "config" })), _jsxs("div", { className: "modal-foot", children: [_jsx("button", { type: "button", className: "btn", onClick: onClose, children: "Cancel" }), _jsx("button", { className: "btn primary", disabled: busy || !t || !dev || !name, children: "Create" })] })] })] }));
}
