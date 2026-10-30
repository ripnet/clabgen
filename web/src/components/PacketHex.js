import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { Fragment, useMemo, useState } from "react";
// fieldKeys names each preview layer's fields the way modifiers address
// them ("ipv4.src", "vlan[2].vid"), from the config's layer kinds.
export function fieldKeys(p, kinds) {
    const out = new Map();
    const count = {};
    const seen = new Map();
    for (const l of p.layers ?? []) {
        if (l.layer === -2)
            continue; // the device's tags
        let prefix = seen.get(l.layer);
        if (prefix === undefined) {
            const kind = l.layer < 0 ? "ethernet" : kinds[l.layer];
            if (!kind)
                continue;
            count[kind] = (count[kind] ?? 0) + 1;
            prefix = count[kind] === 1 ? kind : `${kind}[${count[kind]}]`;
            seen.set(l.layer, prefix);
        }
        out.set(l, prefix);
    }
    return out;
}
export function PacketHex({ preview, kinds, activeLayer, onLayer, onAddModifier, }) {
    const [hover, setHover] = useState({});
    const bytes = useMemo(() => {
        const h = preview.hex ?? "";
        const out = [];
        for (let i = 0; i + 1 < h.length; i += 2)
            out.push(parseInt(h.slice(i, i + 2), 16));
        return out;
    }, [preview.hex]);
    const layers = preview.layers ?? [];
    const keys = useMemo(() => (kinds ? fieldKeys(preview, kinds) : new Map()), [preview, kinds]);
    // Each byte's layer (index into layers), for its colour.
    const layerOf = useMemo(() => {
        const m = new Array(bytes.length).fill(-1);
        layers.forEach((l, i) => {
            for (let b = l.offset; b < l.offset + l.length && b < m.length; b++)
                m[b] = i;
        });
        return m;
    }, [layers, bytes.length]);
    const lit = (b) => {
        if (hover.field) {
            const [pos, bits] = hover.field;
            return b >= Math.floor(pos / 8) && b < Math.ceil((pos + bits) / 8);
        }
        const li = hover.layer ?? (activeLayer !== undefined ? layers.findIndex((l) => l.layer === activeLayer) : -1);
        return li >= 0 && layerOf[b] === li;
    };
    const enterLayer = (i, f) => {
        setHover({ layer: i, field: f ? [f.bit_offset, f.bits] : undefined });
        onLayer?.(i === undefined ? undefined : layers[i].layer);
    };
    // Colours follow the config's layers (as the editor's cards do); the
    // device's tags and an implied Ethernet header are neutral.
    const tint = (li) => (li < 0 || layers[li].layer < 0 ? "tn" : `t${layers[li].layer % 6}`);
    const rows = [];
    for (let off = 0; off < bytes.length; off += 16) {
        const cells = [];
        let ascii = "";
        for (let b = off; b < off + 16; b++) {
            if (b >= bytes.length) {
                cells.push(_jsx("span", { className: `hex-b hex-pad${b % 8 === 7 ? " gap" : ""}`, children: "\u00a0\u00a0" }, b));
                continue;
            }
            const li = layerOf[b];
            const c = bytes[b];
            ascii += c >= 0x20 && c < 0x7f ? String.fromCharCode(c) : ".";
            cells.push(_jsx("span", { className: `hex-b ${tint(li)}${lit(b) ? " lit" : ""}${b % 8 === 7 ? " gap" : ""}`, onMouseEnter: () => enterLayer(li < 0 ? undefined : li, fieldAt(layers[li], b)), onMouseLeave: () => enterLayer(undefined), children: c.toString(16).padStart(2, "0") }, b));
        }
        rows.push(_jsxs("div", { className: "hex-row", children: [_jsx("span", { className: "hex-off", children: off.toString(16).padStart(4, "0") }), _jsx("span", { className: "hex-bytes", children: cells }), _jsx("span", { className: "hex-ascii", children: ascii })] }, off));
    }
    return (_jsxs("div", { className: "pkt-hex", children: [_jsx("div", { className: "hex-dump mono", "aria-label": "Packet bytes", children: rows }), _jsx("table", { className: "table dense pkt-fields", children: _jsx("tbody", { children: layers.map((l, i) => (_jsxs(Fragment, { children: [_jsx("tr", { className: `pkt-layer-row ${tint(i)}`, onMouseEnter: () => enterLayer(i), onMouseLeave: () => enterLayer(undefined), children: _jsxs("th", { colSpan: 3, children: [l.title, " ", _jsxs("span", { className: "muted", children: ["\u00B7 ", l.offset, "\u2013", l.offset + l.length - 1] })] }) }), (l.fields ?? []).map((f) => {
                                const key = keys.get(l) ? `${keys.get(l)}.${f.name}` : "";
                                return (_jsxs("tr", { onMouseEnter: () => enterLayer(i, f), onMouseLeave: () => enterLayer(undefined), className: hover.field?.[0] === f.bit_offset ? "lit" : "", children: [_jsx("td", { className: "mono", children: f.name }), _jsx("td", { className: "mono", children: f.value }), _jsx("td", { className: "num", children: onAddModifier && key && (_jsx("button", { type: "button", className: "link small", title: `Change ${key} from packet to packet`, onClick: () => onAddModifier(key), children: "+ modifier" })) })] }, f.name));
                            })] }, i))) }) })] }));
}
function fieldAt(l, b) {
    return l?.fields?.find((f) => b >= Math.floor(f.bit_offset / 8) && b < Math.ceil((f.bit_offset + f.bits) / 8));
}
