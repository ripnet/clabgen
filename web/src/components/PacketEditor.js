import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useEffect, useMemo, useRef, useState } from "react";
import { client, errorText } from "../api";
import { SchemaFields, SchemaForm } from "./SchemaForm";
import { PacketHex } from "./PacketHex";
// The packet builder's editor: the layers as cards, the rest of the config
// in the usual form, and a live preview from the device (bytes by layer).
const KINDS = ["ethernet", "vlan", "mpls", "arp", "ipv4", "ipv6", "udp", "tcp", "icmp", "icmpv6", "vxlan", "raw", "payload"];
const TITLES = {
    ethernet: "Ethernet", vlan: "VLAN", mpls: "MPLS", arp: "ARP", ipv4: "IPv4", ipv6: "IPv6", udp: "UDP", tcp: "TCP",
    icmp: "ICMP", icmpv6: "ICMPv6", vxlan: "VXLAN", raw: "Raw bytes", payload: "Payload",
};
// Starting points: each replaces the layers.
const TEMPLATES = [
    { name: "UDP over IPv4", layers: [{ ipv4: { dst: "" } }, { udp: { dst_port: 5001 } }, { payload: { size: 64 } }] },
    { name: "UDP over IPv6", layers: [{ ipv6: { dst: "" } }, { udp: { dst_port: 5001 } }, { payload: { size: 64 } }] },
    { name: "TCP SYN", layers: [{ ipv4: { dst: "" } }, { tcp: { dst_port: 80, flags: "S" } }] },
    { name: "ICMP echo request", layers: [{ ipv4: { dst: "" } }, { icmp: { id: 1 } }, { payload: { size: 56 } }] },
    { name: "ARP request", layers: [{ arp: { target_ip: "" } }] },
    { name: "Raw L2 (no IP)", layers: [{ ethernet: { dst: "ff:ff:ff:ff:ff:ff" } }, { payload: { size: 46 } }] },
    { name: "QinQ + IPv4", layers: [{ vlan: { tpid: 0x88a8, vid: 100 } }, { vlan: { vid: 10 } }, { ipv4: { dst: "" } }, { udp: { dst_port: 5001 } }, { payload: { size: 64 } }] },
    { name: "MPLS + IPv4", layers: [{ mpls: { label: 16001 } }, { ipv4: { dst: "" } }, { udp: { dst_port: 5001 } }, { payload: { size: 64 } }] },
    { name: "VXLAN", layers: [{ ipv4: { dst: "" } }, { udp: {} }, { vxlan: { vni: 1000 } }, { ethernet: { dst: "02:00:00:00:00:02" } }, { payload: { size: 46 } }] },
];
const kindOf = (l) => Object.keys(l).find((k) => KINDS.includes(k)) ?? "";
function without(s, drop) {
    const props = { ...(s.properties ?? {}) };
    for (const k of drop)
        delete props[k];
    return { ...s, properties: props, "x-order": s["x-order"]?.filter((k) => !drop.includes(k)) };
}
// One line per layer, from the preview's values (or the config's).
function layerLine(kind, cfg, v) {
    const g = (k) => v[k] ?? (cfg[k] !== undefined && cfg[k] !== "" ? String(cfg[k]) : "");
    switch (kind) {
        case "ethernet":
            return `${g("src") || "device"} → ${g("dst") || "next hop"}`;
        case "vlan":
            return `VID ${g("vid")}${g("pcp") && g("pcp") !== "0" ? ` · PCP ${g("pcp")}` : ""}`;
        case "mpls":
            return `label ${g("label")}`;
        case "arp":
            return `${g("op") === "2" ? "reply" : "who has"} ${g("target_ip")}`;
        case "ipv4":
        case "ipv6":
            return `${g("src") || "device"} → ${g("dst") || "?"}`;
        case "udp":
        case "tcp":
            return `${g("src_port")} → ${g("dst_port")}`;
        case "icmp":
        case "icmpv6":
            return `type ${g("type")} code ${g("code") || 0}`;
        case "vxlan":
            return `VNI ${g("vni")}`;
        case "raw":
            return `${Math.floor(String(cfg.hex ?? "").replace(/[^0-9a-f]/gi, "").length / 2)} bytes`;
        case "payload":
            return `${cfg.size ?? 0} bytes${cfg.signature === false ? "" : " · signed"}`;
    }
    return "";
}
const size = (n) => (n < 1024 ? `${n} bytes` : n < 1 << 20 ? `${(n / 1024).toFixed(0)} KiB` : `${(n / (1 << 20)).toFixed(1)} MiB`);
function toBase64(buf) {
    const b = new Uint8Array(buf);
    let s = "";
    for (let i = 0; i < b.length; i += 0x8000)
        s += String.fromCharCode(...b.subarray(i, i + 0x8000));
    return btoa(s);
}
// usePreview asks the agent what the config sends (debounced, newest wins).
function usePreview(agentId, device, config, index) {
    const [state, setState] = useState({ loading: true });
    const seq = useRef(0);
    useEffect(() => {
        if (!device) {
            setState({ loading: false, error: "Pick a device to preview the packet from." });
            return;
        }
        const my = ++seq.current;
        setState((s) => ({ ...s, loading: true }));
        const t = setTimeout(async () => {
            try {
                const r = await client(agentId).previewProtocol({ type: "packet", device, config: config, args: { index } }, { timeoutMs: 8000 });
                if (my === seq.current)
                    setState({ data: r.result, loading: false });
            }
            catch (e) {
                if (my === seq.current)
                    setState((s) => ({ data: s.data, error: errorText(e), loading: false }));
            }
        }, 300);
        return () => clearTimeout(t);
    }, [agentId, device, config, index]);
    return state;
}
export function PacketEditor({ schema, value, onChange, agentId, device, errorText: rejected }) {
    const layerSchema = schema.properties?.layers?.items ?? {};
    const pcapSchema = schema.properties?.pcap ?? {};
    const rest = useMemo(() => without(schema, ["layers", "pcap"]), [schema]);
    const layers = value.layers ?? [];
    const pcap = value.pcap;
    const replaying = !!pcap?.data;
    const [index, setIndex] = useState(0);
    const [open, setOpen] = useState(null);
    const [hot, setHot] = useState();
    const [imported, setImported] = useState();
    const fileRef = useRef(null);
    const preview = usePreview(agentId, device, value, index);
    const kinds = useMemo(() => layers.map(kindOf), [layers]);
    const setLayers = (ls) => onChange({ ...value, layers: ls });
    const setLayer = (i, l) => setLayers(layers.map((x, j) => (j === i ? l : x)));
    const move = (i, d) => {
        const ls = [...layers];
        [ls[i], ls[i + d]] = [ls[i + d], ls[i]];
        setLayers(ls);
        setOpen(i + d);
    };
    const addLayer = (kind) => {
        const ls = [...layers];
        const at = ls.length && kindOf(ls[ls.length - 1]) === "payload" && kind !== "payload" ? ls.length - 1 : ls.length;
        ls.splice(at, 0, { [kind]: kind === "payload" ? { size: 46 } : kind === "raw" ? { hex: "" } : {} });
        setLayers(ls);
        setOpen(at);
    };
    const addModifier = (field) => {
        const ms = [...(value.modifiers ?? []), { field, mode: "increment" }];
        onChange({ ...value, modifiers: ms });
    };
    // Values per config layer from the preview, for the cards' summary lines.
    const shown = useMemo(() => {
        const m = new Map();
        for (const l of preview.data?.layers ?? []) {
            const v = m.get(l.layer) ?? {};
            for (const f of l.fields ?? [])
                v[f.name] = f.value;
            m.set(l.layer, v);
        }
        return m;
    }, [preview.data]);
    const importFile = async (f) => {
        const data = toBase64(await f.arrayBuffer());
        setImported({ name: f.name, data });
        try {
            const r = await client(agentId).previewProtocol({ type: "packet", device, config: { ...value, pcap: { data } }, args: { index: 0 } }, { timeoutMs: 20000 });
            setImported({ name: f.name, data, preview: r.result });
        }
        catch (e) {
            setImported({ name: f.name, data, error: errorText(e) });
        }
    };
    const frameAsLayers = async (i) => {
        if (!imported)
            return;
        try {
            const r = await client(agentId).previewProtocol({ type: "packet", device, config: { ...value, pcap: { data: imported.data } }, args: { index: i } });
            const p = r.result;
            const next = { ...value, layers: p.decoded ?? [] };
            delete next.pcap;
            onChange(next);
            setImported(undefined);
            setIndex(0);
        }
        catch (e) {
            setImported({ ...imported, error: errorText(e) });
        }
    };
    const p = preview.data;
    const frames = replaying ? p?.frames?.length : undefined;
    return (_jsxs("div", { className: "pkt-editor", children: [_jsxs("div", { className: "pkt-build", children: [replaying ? (_jsxs("section", { className: "pkt-pcap card-lite", children: [_jsxs("div", { className: "pkt-sec-head", children: [_jsx("h4", { children: "Replaying a capture" }), _jsx("button", { type: "button", className: "link", onClick: () => onChange({ ...value, pcap: undefined }), children: "build from layers instead" })] }), _jsxs("div", { className: "muted small-note", children: [frames ? `${frames}${frames === 1000 ? "+" : ""} frames` : "…", " \u00B7 ", size(Math.round((String(pcap?.data).length * 3) / 4)), ", kept in this flow's config"] }), _jsx(SchemaFields, { schema: without(pcapSchema, ["data"]), value: pcap ?? {}, onChange: (v) => onChange({ ...value, pcap: { ...v, data: pcap?.data } }) })] })) : (_jsxs("section", { children: [_jsxs("div", { className: "pkt-sec-head", children: [_jsx("h4", { children: "Layers" }), _jsxs("select", { className: "input small", "aria-label": "Start from a template", value: "", onChange: (e) => {
                                            const t = TEMPLATES.find((x) => x.name === e.target.value);
                                            if (t) {
                                                setLayers(structuredClone(t.layers));
                                                setOpen(0);
                                            }
                                        }, children: [_jsx("option", { value: "", children: "Start from\u2026" }), TEMPLATES.map((t) => (_jsx("option", { children: t.name }, t.name)))] })] }), layers.length > 0 && kindOf(layers[0]) !== "ethernet" && (_jsxs("div", { className: "pkt-implicit muted", onMouseEnter: () => setHot(-1), onMouseLeave: () => setHot(undefined), children: ["Ethernet ", _jsx("span", { className: "mono", children: layerLine("ethernet", {}, shown.get(-1) ?? {}) }), " \u00B7 from the device, with its VLAN tags"] })), _jsx("ol", { className: "pkt-layers", children: layers.map((l, i) => {
                                    const k = kindOf(l);
                                    const sub = l[k] ?? {};
                                    const isOpen = open === i;
                                    return (_jsxs("li", { className: `pkt-layer t${i % 6}${hot === i ? " hot" : ""}`, onMouseEnter: () => setHot(i), onMouseLeave: () => setHot(undefined), children: [_jsxs("div", { className: "pkt-layer-head", children: [_jsxs("button", { type: "button", className: "pkt-layer-title", "aria-expanded": isOpen, onClick: () => setOpen(isOpen ? null : i), children: [_jsx("span", { className: "caret", children: isOpen ? "▾" : "▸" }), " ", _jsx("b", { children: TITLES[k] ?? "?" }), " ", _jsx("span", { className: "mono muted", children: layerLine(k, sub, shown.get(i) ?? {}) })] }), _jsxs("span", { className: "pkt-layer-tools", children: [_jsx("button", { type: "button", className: "icon-btn", disabled: i === 0, onClick: () => move(i, -1), "aria-label": "Move up", title: "Move up", children: "\u2191" }), _jsx("button", { type: "button", className: "icon-btn", disabled: i === layers.length - 1, onClick: () => move(i, 1), "aria-label": "Move down", title: "Move down", children: "\u2193" }), _jsx("button", { type: "button", className: "icon-btn", onClick: () => setLayers(layers.filter((_, j) => j !== i)), "aria-label": "Remove layer", title: "Remove", children: "\u00D7" })] })] }), isOpen && (_jsx("div", { className: "pkt-layer-body", children: _jsx(SchemaFields, { schema: layerSchema.properties?.[k] ?? {}, value: sub, onChange: (v) => setLayer(i, { [k]: v }) }) }))] }, i));
                                }) }), _jsxs("div", { className: "btn-row", children: [_jsxs("select", { className: "input small", "aria-label": "Add a layer", value: "", onChange: (e) => e.target.value && addLayer(e.target.value), children: [_jsx("option", { value: "", children: "+ Add layer\u2026" }), KINDS.map((k) => (_jsx("option", { value: k, children: TITLES[k] }, k)))] }), _jsx("button", { type: "button", className: "btn small", onClick: () => fileRef.current?.click(), disabled: !device, children: "Import pcap\u2026" }), _jsx("input", { ref: fileRef, type: "file", accept: ".pcap,.pcapng,.cap", hidden: true, onChange: (e) => {
                                            const f = e.target.files?.[0];
                                            e.target.value = "";
                                            if (f)
                                                importFile(f);
                                        } })] }), imported && (_jsxs("div", { className: "pkt-import card-lite", children: [_jsxs("div", { className: "pkt-sec-head", children: [_jsx("h4", { children: imported.name }), _jsx("button", { type: "button", className: "icon-btn", onClick: () => setImported(undefined), "aria-label": "Close import", children: "\u00D7" })] }), imported.error && _jsx("div", { className: "error-box", children: imported.error }), !imported.preview && !imported.error && _jsx("div", { className: "muted", children: "Reading\u2026" }), imported.preview && (_jsxs(_Fragment, { children: [_jsxs("div", { className: "btn-row", children: [_jsxs("button", { type: "button", className: "btn primary small", onClick: () => {
                                                            onChange({ ...value, pcap: { data: imported.data } });
                                                            setImported(undefined);
                                                            setIndex(0);
                                                        }, children: ["Replay all ", imported.preview.frames?.length ?? 0, " frames"] }), _jsx("span", { className: "muted small-note", children: "or edit one frame as layers:" })] }), _jsx("div", { className: "pkt-frames", children: (imported.preview.frames ?? []).map((f) => (_jsxs("button", { type: "button", className: "pkt-frame", onClick: () => frameAsLayers(f.index), children: [_jsx("span", { className: "mono muted", children: f.index + 1 }), " ", _jsx("span", { className: "mono", children: f.summary })] }, f.index))) })] }))] }))] })), _jsxs("section", { children: [_jsx("h4", { children: "Sending" }), _jsx(SchemaForm, { schema: rest, value: value, onChange: onChange, mode: "config", errorText: rejected })] })] }), _jsxs("aside", { className: "pkt-preview", "aria-live": "polite", children: [_jsxs("div", { className: "pkt-sec-head", children: [_jsx("h4", { children: "Preview" }), _jsxs("span", { className: "pkt-index", children: [_jsx("button", { type: "button", className: "icon-btn", disabled: index === 0, onClick: () => setIndex(index - 1), "aria-label": "Previous packet", children: "\u2039" }), _jsxs("span", { className: "mono", children: [replaying ? "frame" : "packet", " ", (p?.index ?? index) + (replaying ? 1 : 0), frames ? ` of ${frames}` : ""] }), _jsx("button", { type: "button", className: "icon-btn", disabled: !!frames && index >= frames - 1, onClick: () => setIndex(index + 1), "aria-label": "Next packet", children: "\u203A" })] })] }), preview.error && _jsx("div", { className: "error-box", children: preview.error }), p && (_jsxs("div", { className: preview.error || preview.loading ? "stale" : "", children: [_jsx("div", { className: "mono pkt-summary", children: p.summary }), p.note && _jsx("div", { className: p.note.startsWith("next hop unresolved") ? "warn-box small-note" : "muted small-note", children: p.note }), _jsx(PacketHex, { preview: p, kinds: replaying ? undefined : kinds, activeLayer: hot, onLayer: (l) => setHot(l === -2 ? undefined : l), onAddModifier: replaying ? undefined : addModifier })] })), !p && !preview.error && _jsx("div", { className: "muted", children: "Building\u2026" })] })] }));
}
