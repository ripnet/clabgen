import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useEffect, useState } from "react";
import { client, errorText } from "../api";
import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { PacketEditor } from "../components/PacketEditor";
import { PacketHex } from "../components/PacketHex";
import { registerView } from "./registry";
// The packet a running flow sends: its preview action, packet by packet.
function NextPacket({ agentId, name }) {
    const [index, setIndex] = useState();
    const [p, setP] = useState();
    const [err, setErr] = useState();
    useEffect(() => {
        client(agentId)
            .runProtocolAction({ name, action: "preview", args: (index === undefined ? {} : { index }) })
            .then((r) => {
            setP(r.result);
            setErr(undefined);
        })
            .catch((e) => setErr(errorText(e)));
    }, [agentId, name, index]);
    const at = p?.index ?? 0;
    return (_jsxs("details", { className: "pkt-next", children: [_jsx("summary", { children: _jsx("h4", { children: "Packet" }) }), err && _jsx("div", { className: "error-box", children: err }), p && (_jsxs(_Fragment, { children: [_jsxs("div", { className: "btn-row", children: [_jsx("button", { type: "button", className: "icon-btn", disabled: at === 0, onClick: () => setIndex(at - 1), "aria-label": "Previous packet", children: "\u2039" }), _jsxs("span", { className: "mono", children: ["packet ", at] }), _jsx("button", { type: "button", className: "icon-btn", onClick: () => setIndex(at + 1), "aria-label": "Next packet", children: "\u203A" }), _jsx("span", { className: "mono muted", children: p.summary })] }), p.note && _jsx("div", { className: p.note.startsWith("next hop unresolved") ? "warn-box small-note" : "muted small-note", children: p.note }), _jsx(PacketHex, { preview: p })] }))] }));
}
function PacketStatus(p) {
    return (_jsxs(_Fragment, { children: [_jsx(IpFlowView, { ...p }), p.agentId && p.name && _jsx(NextPacket, { agentId: p.agentId, name: p.name })] }));
}
registerView("packet", { summary: flowSummary, Status: PacketStatus, ConfigEditor: PacketEditor });
