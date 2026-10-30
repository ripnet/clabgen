import { jsxs as _jsxs, jsx as _jsx, Fragment as _Fragment } from "react/jsx-runtime";
import { useEffect, useMemo, useState } from "react";
import { parse as parseYAML, stringify as toYAML } from "yaml";
import { client, errorText, usePoll } from "../api";
import { navigate } from "../router";
import { SchemaForm, defaults, normalize, parseSchema } from "../components/SchemaForm";
import { StepInput, fmtStep, isStep } from "../components/Stepped";
import { Modal, useToast } from "../ui";
const blank = (port) => ({ name: "", count: 10, device: { port }, protocols: [] });
const placeholders = "{i} (1-based), {i0}, {group}; in protocol configs also the member's {device}, {ipv4}, {ipv6}, {gateway4}, {gateway6}, {mac}, {vlan}, {inner_vlan}";
// initial (with note, shown above the form) starts a new group from
// something else, e.g. an existing device and its protocols.
export function GroupEditor({ agentId, group, initial, note, onClose, onDone, }) {
    const c = client(agentId);
    const ports = usePoll(() => c.listPorts({}), [agentId]);
    const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
    const portNames = (ports.data?.ports ?? []).map((p) => p.name);
    const [spec, setSpec] = useState(() => group?.spec ?? initial ?? blank(""));
    // A new group starts on the first port, once the ports are known.
    useEffect(() => {
        if (!group && !spec.device?.port && portNames.length)
            setSpec((s) => ({ ...s, device: { ...s.device, port: portNames[0] } }));
    }, [portNames.join()]); // eslint-disable-line react-hooks/exhaustive-deps
    const [sel, setSel] = useState("device");
    const [yaml, setYaml] = useState();
    const [yamlErr, setYamlErr] = useState();
    const [preview, setPreview] = useState();
    const [previewErr, setPreviewErr] = useState();
    // Members to preview besides the first two and the last.
    const [extra, setExtra] = useState([]);
    const [error, setError] = useState();
    const [busy, setBusy] = useState(false);
    const toast = useToast();
    // The agent renders a few members after each pause in editing.
    const text = JSON.stringify(spec);
    useEffect(() => {
        if (!spec.name || !spec.count) {
            setPreview(undefined);
            setPreviewErr(!spec.name ? "Name the group to see its members." : "Set how many members.");
            return;
        }
        let live = true;
        const t = setTimeout(() => {
            const members = [...new Set([1, 2, spec.count, ...extra])].filter((i) => i >= 1 && i <= spec.count).sort((a, b) => a - b);
            c.previewDeviceGroup({ group: { name: spec.name, spec: spec }, members })
                .then((r) => {
                if (!live)
                    return;
                setPreview(r.result);
                setPreviewErr(undefined);
            })
                .catch((e) => {
                if (!live)
                    return;
                const t = errorText(e);
                // Agents before the group editor don't have the call.
                setPreviewErr(/404|unimplemented/i.test(t) ? "This agent can't preview groups (it needs a newer clabgen); creating and editing still work." : t);
            });
        }, 350);
        return () => {
            live = false;
            clearTimeout(t);
        };
    }, [agentId, text, extra.join()]); // eslint-disable-line react-hooks/exhaustive-deps
    // clean drops protocol values equal to their defaults, so a spec built
    // here is as short as one written by hand.
    const clean = (s) => ({
        ...s,
        protocols: s.protocols?.map((p) => {
            const t = types.data?.types.find((x) => x.name === p.type);
            return t && p.config ? { ...p, config: normalize(parseSchema(t.configSchema), p.config) } : p;
        }),
    });
    const protos = spec.protocols ?? [];
    const protoKey = (p, i) => `p${i}:${p.name || p.type}`;
    const setProto = (i, p) => {
        const next = [...protos];
        if (p)
            next[i] = p;
        else
            next.splice(i, 1);
        setSpec({ ...spec, protocols: next });
    };
    const addProto = (type) => {
        const t = types.data?.types.find((x) => x.name === type);
        const p = { type, config: t ? defaults(parseSchema(t.configSchema)) : {} };
        // A second instance of a type needs its own name.
        if (protos.some((x) => (x.name || x.type) === type))
            p.name = `${type}-${protos.length + 1}`;
        setSpec({ ...spec, protocols: [...protos, p] });
        setSel(protoKey(p, protos.length));
    };
    // A rejected spec stays in the editor with the agent's error.
    const save = async () => {
        setError(undefined);
        setBusy(true);
        const req = { group: { name: spec.name ?? "", spec: clean(spec) } };
        try {
            const r = group ? (await c.updateDeviceGroup(req)).group : (await c.createDeviceGroup(req)).group;
            toast("ok", group ? `Updated ${group.name}` : "Group created");
            onDone();
            onClose();
            if (!group && r)
                navigate({ page: "group", agent: agentId, group: r.name });
        }
        catch (e) {
            setError(errorText(e));
            setStep("edit");
        }
        finally {
            setBusy(false);
        }
    };
    const selIdx = protos.findIndex((p, i) => protoKey(p, i) === sel);
    const plan = group ? preview?.plan : undefined;
    // Editing ends in a review of the template's changes and the plan.
    const [step, setStep] = useState("edit");
    const original = useMemo(() => group?.spec ?? {}, [group]);
    const changes = useMemo(() => (group ? specChanges(clean(original), clean(spec)) : []), [group, original, text, types.data]); // eslint-disable-line react-hooks/exhaustive-deps
    return (_jsxs(Modal, { title: group ? `Edit group ${group.name}` : "New device group", onClose: onClose, wide: "x", children: [error && (_jsxs("div", { className: "error-box", role: "alert", children: ["The agent rejected the group: ", error] })), _jsxs("div", { hidden: step === "review", children: [note && _jsx("div", { className: "info-box", children: note }), _jsxs("div", { className: "group-head", children: [_jsxs("label", { className: "field", children: [_jsx("span", { className: "field-label", children: "name" }), _jsx("input", { className: "input mono", "aria-label": "Group name", value: spec.name ?? "", disabled: !!group, placeholder: "e.g. rmep", onChange: (e) => setSpec({ ...spec, name: e.target.value }) })] }), _jsxs("label", { className: "field", children: [_jsx("span", { className: "field-label", children: "members" }), _jsx("input", { className: "input mono", "aria-label": "Member count", type: "number", min: 1, max: 4096, value: spec.count ?? "", onChange: (e) => setSpec({ ...spec, count: e.target.value === "" ? undefined : Number(e.target.value) }) })] }), _jsx("button", { type: "button", className: "link group-yaml-toggle", onClick: () => {
                                    if (yaml === undefined) {
                                        setYaml(toYAML(clean(spec)));
                                        setYamlErr(undefined);
                                    }
                                    else if (!yamlErr)
                                        setYaml(undefined);
                                }, children: yaml === undefined ? "edit as YAML" : "form view" })] }), yaml !== undefined ? (_jsxs(_Fragment, { children: [_jsx("textarea", { className: "input mono code scenario", "aria-label": "Group as YAML", spellCheck: false, value: yaml, onChange: (e) => {
                                    setYaml(e.target.value);
                                    try {
                                        const v = parseYAML(e.target.value) ?? {};
                                        if (typeof v !== "object" || Array.isArray(v))
                                            throw new Error("must be a mapping of fields");
                                        setYamlErr(undefined);
                                        setSpec(v);
                                    }
                                    catch (x) {
                                        setYamlErr(x.message);
                                    }
                                } }), yamlErr && _jsx("div", { className: "hint error-text", children: yamlErr })] })) : (_jsxs("div", { className: "sectioned group-sections", children: [_jsxs("nav", { className: "section-nav", "aria-label": "Group parts", children: [_jsx("button", { type: "button", className: sel === "device" ? "active" : "", onClick: () => setSel("device"), children: _jsx("span", { children: "device" }) }), protos.map((p, i) => (_jsx("button", { type: "button", className: sel === protoKey(p, i) ? "active" : "", onClick: () => setSel(protoKey(p, i)), children: _jsx("span", { className: "mono", children: p.name || p.type }) }, i))), _jsxs("select", { className: "input add-proto", "aria-label": "Add a protocol", value: "", onChange: (e) => e.target.value && addProto(e.target.value), children: [_jsx("option", { value: "", children: "+ protocol\u2026" }), (types.data?.types ?? []).map((t) => (_jsx("option", { value: t.name, children: t.name }, t.name)))] })] }), _jsxs("div", { className: "section-pane", children: [_jsxs("p", { className: "muted small-note", children: [_jsx("b", { children: "\u00B1" }), " steps a value per member: start, plus a step (1, or an address/MAC form like ", _jsx("code", { children: "0.0.0.4" }), ") for each member. Text takes", " ", _jsx("code", { children: placeholders }), "."] }), selIdx < 0 ? (_jsx(DeviceTemplate, { value: spec.device ?? {}, ports: portNames, onChange: (d) => setSpec({ ...spec, device: d }) })) : (_jsx(ProtoTemplate, { proto: protos[selIdx], schemaText: types.data?.types.find((t) => t.name === protos[selIdx].type)?.configSchema, onChange: (p) => setProto(selIdx, p), onRemove: () => {
                                            setProto(selIdx, undefined);
                                            setSel("device");
                                        } }, sel))] })] })), _jsx(MemberPreview, { preview: preview, error: previewErr, protos: protos, extra: extra, onShow: (i) => setExtra([...new Set([...extra, i])]), onHide: (i) => setExtra(extra.filter((x) => x !== i)) })] }), step === "review" && (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table diff-table", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Field" }), _jsx("th", { children: "Was" }), _jsx("th", { children: "Now" })] }) }), _jsx("tbody", { children: changes.map((ch) => (_jsxs("tr", { children: [_jsx("td", { className: "mono", children: ch.path }), _jsx("td", { className: "mono diff-from", children: ch.from ?? _jsx("span", { className: "muted", children: "\u2014" }) }), _jsx("td", { className: "mono diff-to", children: ch.to ?? _jsx("span", { className: "muted", children: "removed" }) })] }, ch.path))) })] }) })), _jsxs("div", { className: "modal-foot", children: [plan && (_jsxs("span", { className: `muted small group-plan${plan.changed + plan.removed ? " warn-text" : ""}`, children: ["Applying: ", planText(plan)] })), step === "review" ? (_jsx("button", { type: "button", className: "btn", onClick: () => setStep("edit"), children: "Back" })) : (_jsx("button", { type: "button", className: "btn", onClick: onClose, children: "Cancel" })), group && step === "edit" ? (_jsx("button", { type: "button", className: "btn primary", disabled: !!yamlErr || !changes.length, onClick: () => setStep("review"), children: changes.length ? `Review ${changes.length} change${changes.length === 1 ? "" : "s"}` : "No changes" })) : (_jsx("button", { type: "button", className: "btn primary", disabled: busy || !!yamlErr || !spec.name, onClick: save, children: group ? "Apply" : "Create" }))] })] }));
}
// specLeaves lists a spec's values by path, a stepped value as one
// ("protocols[0].config.mep_id": "101 +1 per member").
function specLeaves(v, at = "", out = {}) {
    if (isStep(v))
        out[at] = fmtStep(v);
    else if (Array.isArray(v))
        v.forEach((x, i) => specLeaves(x, `${at}[${i}]`, out));
    else if (v && typeof v === "object")
        for (const [k, x] of Object.entries(v))
            specLeaves(x, at ? `${at}.${k}` : k, out);
    else if (v !== undefined && v !== null)
        out[at] = typeof v === "string" ? v : JSON.stringify(v);
    return out;
}
// specChanges lists what differs between two group specs, by path.
function specChanges(a, b) {
    const x = specLeaves(a);
    const y = specLeaves(b);
    const out = [];
    for (const k of [...new Set([...Object.keys(x), ...Object.keys(y)])].sort())
        if (x[k] !== y[k])
            out.push({ path: k, from: x[k], to: y[k] });
    return out;
}
export function planText(p) {
    const parts = [
        p.changed && `${p.changed} changed`,
        p.added && `${p.added} added`,
        p.removed && `${p.removed} removed`,
        p.unchanged && `${p.unchanged} unchanged (keep running)`,
    ].filter(Boolean);
    return !p.changed && !p.added && !p.removed ? "nothing changes" : parts.join(", ");
}
const tpids = [
    { v: undefined, label: "802.1Q" },
    { v: 0x88a8, label: "802.1ad (0x88a8)" },
    { v: 0x9100, label: "0x9100" },
];
// DeviceTemplate edits the member device: port, VLAN tags, addresses,
// gateways and MAC, each value steppable.
function DeviceTemplate({ value, ports, onChange }) {
    const set = (k, v) => {
        const next = { ...value };
        if (v === undefined || v === "" || (Array.isArray(v) && !v.length))
            delete next[k];
        else
            next[k] = v;
        onChange(next);
    };
    const vlans = value.vlans ?? [];
    const addrs = value.addresses ?? [];
    const textStep = (k, label, ph) => (_jsxs("div", { className: "field", children: [_jsx("span", { className: "field-label", children: label }), _jsx(StepInput, { value: value[k], numeric: false, label: label, onChange: (v) => set(k, v), children: _jsx("input", { className: "input mono", "aria-label": label, placeholder: ph, value: value[k] ?? "", onChange: (e) => set(k, e.target.value) }) })] }));
    return (_jsxs("div", { className: "fieldset", children: [_jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsxs("span", { className: "field-label", children: ["port", _jsx("span", { className: "req", children: "*" })] }), _jsxs("select", { className: "input", "aria-label": "port", value: value.port ?? "", onChange: (e) => set("port", e.target.value), children: [!ports.includes(value.port) && _jsx("option", { value: value.port ?? "", children: value.port || "—" }), ports.map((p) => (_jsx("option", { children: p }, p)))] })] }), _jsxs("div", { className: "field", children: [_jsx("span", { className: "field-label", children: "name" }), _jsx("input", { className: "input mono", "aria-label": "device name", placeholder: "{group}-{i}", value: value.name ?? "", onChange: (e) => set("name", e.target.value) })] }), textStep("mac", "MAC", "derived from the name")] }), _jsxs("div", { className: "field-block", children: [_jsx("span", { className: "field-label", children: "VLAN tags (outer first)" }), vlans.map((t, i) => (_jsxs("div", { className: "form-grid tag-row", children: [_jsxs("div", { className: "field", children: [_jsx("span", { className: "field-label", children: "VLAN ID" }), _jsx(StepInput, { value: t.vid, numeric: true, label: `VLAN ${i + 1}`, onChange: (v) => set("vlans", vlans.map((x, j) => (j === i ? { ...x, vid: v } : x))), children: _jsx("input", { className: "input mono", type: "number", min: 0, max: 4095, "aria-label": `VLAN ${i + 1}`, value: t.vid ?? "", onChange: (e) => set("vlans", vlans.map((x, j) => (j === i ? { ...x, vid: e.target.value === "" ? undefined : Number(e.target.value) } : x))) }) })] }), _jsxs("div", { className: "field", children: [_jsx("span", { className: "field-label", children: "type" }), _jsx("select", { className: "input", "aria-label": `VLAN ${i + 1} type`, value: String(t.tpid ?? ""), onChange: (e) => set("vlans", vlans.map((x, j) => {
                                            if (j !== i)
                                                return x;
                                            const { tpid: _, ...rest } = x;
                                            return e.target.value ? { ...rest, tpid: Number(e.target.value) } : rest;
                                        })), children: tpids.map((o) => (_jsx("option", { value: o.v ?? "", children: o.label }, o.label))) })] }), _jsx("button", { type: "button", className: "icon-btn tag-remove", "aria-label": `Remove VLAN ${i + 1}`, onClick: () => set("vlans", vlans.filter((_, j) => j !== i)), children: "\u00D7" })] }, i))), vlans.length < 4 && (_jsx("button", { type: "button", className: "btn small", onClick: () => set("vlans", [...vlans, { vid: vlans.length ? 1 : 100 }]), children: "Add VLAN tag" }))] }), _jsxs("div", { className: "field-block", children: [_jsx("span", { className: "field-label", children: "addresses" }), addrs.map((a, i) => (_jsxs("div", { className: "addr-row", children: [_jsx(StepInput, { value: a, numeric: false, label: `address ${i + 1}`, onChange: (v) => set("addresses", addrs.map((x, j) => (j === i ? v : x))), children: _jsx("input", { className: "input mono", "aria-label": `address ${i + 1}`, placeholder: "10.1.0.2/30 or 2001:db8::2/64", value: a ?? "", onChange: (e) => set("addresses", addrs.map((x, j) => (j === i ? e.target.value : x))) }) }), _jsx("button", { type: "button", className: "icon-btn", "aria-label": `Remove address ${i + 1}`, onClick: () => set("addresses", addrs.filter((_, j) => j !== i)), children: "\u00D7" })] }, i))), _jsx("button", { type: "button", className: "btn small", onClick: () => set("addresses", [...addrs, ""]), children: "Add address" })] }), _jsxs("div", { className: "form-grid", children: [textStep("gateway4", "IPv4 gateway", "10.1.0.1"), textStep("gateway6", "IPv6 gateway", "2001:db8::1")] })] }));
}
// ProtoTemplate edits one group protocol: its name, type and config.
function ProtoTemplate({ proto, schemaText, onChange, onRemove }) {
    const schema = useMemo(() => parseSchema(schemaText), [schemaText]);
    return (_jsxs("div", { className: "fieldset", children: [_jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsx("span", { className: "field-label", children: "type" }), _jsx("input", { className: "input mono", value: proto.type, disabled: true, "aria-label": "protocol type" })] }), _jsxs("div", { className: "field", children: [_jsx("span", { className: "field-label", children: "name in the group" }), _jsx("input", { className: "input mono", "aria-label": "protocol name", placeholder: proto.type, value: proto.name ?? "", onChange: (e) => {
                                    const { name: _, ...rest } = proto;
                                    onChange(e.target.value ? { ...rest, name: e.target.value } : rest);
                                } })] }), _jsx("div", { className: "field proto-remove", children: _jsx("button", { type: "button", className: "btn small danger-ghost", onClick: onRemove, children: "Remove from group" }) })] }), _jsx(SchemaForm, { schema: schema, value: proto.config ?? {}, mode: "config", stepping: true, expanded: true, onChange: (cfg) => onChange({ ...proto, config: cfg }) })] }));
}
// flat lists a config's scalar values by path ("peers[0].address").
function flat(v, at = "", out = {}) {
    if (Array.isArray(v))
        v.forEach((x, i) => flat(x, `${at}[${i}]`, out));
    else if (v && typeof v === "object")
        for (const [k, x] of Object.entries(v))
            flat(x, at ? `${at}.${k}` : k, out);
    else if (v !== undefined)
        out[at] = String(v);
    return out;
}
// MemberPreview shows the members the agent rendered: the device, and the
// protocol values that differ between them (what the template steps). A
// muted row stands for the members in between; any member can be added to
// the view (extra) and taken out again.
function MemberPreview({ preview, error, protos, extra, onShow, onHide, }) {
    const [want, setWant] = useState("");
    const ms = preview?.members ?? [];
    const protoOf = (m, short) => (m.protocols ?? []).find((x) => x.name === `${m.device.name}-${short}`);
    const varying = useMemo(() => {
        const out = {};
        for (const p of ms[0]?.protocols ?? []) {
            const short = p.name.slice(ms[0].device.name.length + 1);
            const vals = ms.map((m) => flat(protoOf(m, short)?.config));
            out[short] = Object.keys(vals[0] ?? {}).filter((k) => vals.some((v) => v[k] !== vals[0][k]));
        }
        return out;
    }, [preview]); // eslint-disable-line react-hooks/exhaustive-deps
    const stepped = protos.some((p) => JSON.stringify(p.config ?? {}).includes('"start"'));
    const cols = 5 + Object.keys(varying).length;
    const n = Number(want);
    const valid = !!preview && Number.isInteger(n) && n >= 1 && n <= preview.count;
    const rows = [];
    ms.forEach((m, k) => {
        const gap = m.index - (k ? ms[k - 1].index : 0) - 1;
        if (gap > 0)
            rows.push(_jsx("tr", { className: "gap-row", children: _jsxs("td", { colSpan: cols, className: "muted", children: ["\u22EF ", gap === 1 ? `member ${m.index - 1}` : `${gap} more (${m.index - gap}–${m.index - 1})`] }) }, `gap${m.index}`));
        rows.push(_jsxs("tr", { children: [_jsxs("td", { children: [m.index, extra.includes(m.index) && (_jsx("button", { type: "button", className: "icon-btn hide-member", "aria-label": `Stop showing member ${m.index}`, onClick: () => onHide(m.index), children: "\u00D7" }))] }), _jsx("td", { className: "mono", children: m.device.name }), _jsx("td", { className: "mono", children: (m.device.vlans ?? []).map((v) => v.vid).join(".") || "—" }), _jsx("td", { className: "mono", children: [...(m.device.addresses ?? []), m.device.gateway4 && `gw ${m.device.gateway4}`, m.device.gateway6 && `gw ${m.device.gateway6}`].filter(Boolean).join(", ") || "—" }), _jsx("td", { className: "mono", children: m.mac }), Object.entries(varying).map(([p, keys]) => {
                    const cfg = flat(protoOf(m, p)?.config);
                    return (_jsx("td", { className: "mono", children: keys.length ? keys.map((k) => `${k} ${cfg[k]}`).join(", ") : stepped ? "—" : "same on every member" }, p));
                })] }, m.index));
    });
    return (_jsxs("div", { className: "group-preview", children: [_jsxs("div", { className: "preview-head", children: [_jsxs("span", { className: "field-label", children: ["preview", preview && _jsxs("span", { className: "muted", children: [" \u00B7 ", preview.count, " members"] })] }), preview && preview.count > 3 && (_jsxs("form", { className: "show-member", onSubmit: (e) => {
                            e.preventDefault();
                            if (!valid)
                                return;
                            onShow(n);
                            setWant("");
                        }, children: [_jsx("input", { className: "input mono", "aria-label": "Member to show", type: "number", min: 1, max: preview.count, placeholder: "member #", value: want, onChange: (e) => setWant(e.target.value) }), _jsx("button", { className: "btn small", disabled: !valid, children: "Show" })] }))] }), error ? (_jsx("div", { className: error.startsWith("Name the") || error.startsWith("Set how") || error.startsWith("This agent can") ? "muted small-note" : "error-text small", role: "status", children: error })) : !preview ? (_jsx("div", { className: "muted small-note", children: "Rendering\u2026" })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "#" }), _jsx("th", { children: "device" }), _jsx("th", { children: "VLANs" }), _jsx("th", { children: "addresses" }), _jsx("th", { children: "MAC" }), Object.keys(varying).map((p) => (_jsx("th", { className: "mono", children: p }, p)))] }) }), _jsx("tbody", { children: rows })] }) }))] }));
}
