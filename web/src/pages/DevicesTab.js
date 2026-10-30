import { jsx as _jsx, jsxs as _jsxs } from "react/jsx-runtime";
import { useState } from "react";
import { client, usePoll } from "../api";
import { GroupEditor } from "./GroupEditor";
import { DeviceIP } from "./DeviceIP";
import { CreateProtocol } from "./ProtocolsTab";
import { Empty, ErrorBox, Modal, useBusy } from "../ui";
const TPIDS = [0x8100, 0x88a8, 0x9100];
const hex = (n) => "0x" + n.toString(16).padStart(4, "0");
export function vlanText(v) {
    if (!v.length)
        return "untagged";
    return v.map((t) => `${t.tpid && t.tpid !== 0x8100 ? hex(t.tpid) + ":" : ""}${t.vid}${t.pcp ? "." + t.pcp : ""}`).join(" / ");
}
export function DevicesTab({ agentId }) {
    const c = client(agentId);
    const devs = usePoll(() => c.listDevices({}), [agentId], 5000);
    const protos = usePoll(() => c.listProtocols({ withStatus: false }), [agentId], 5000);
    const [editing, setEditing] = useState();
    const [ipOf, setIpOf] = useState();
    const [pingFrom, setPingFrom] = useState();
    const [groupFrom, setGroupFrom] = useState();
    const { busy, run } = useBusy();
    const all = devs.data?.devices ?? [];
    // Group members are listed in the Groups tab.
    const list = all.filter((d) => !d.group);
    const members = all.length - list.length;
    const using = (d) => (protos.data?.protocols ?? []).filter((p) => p.device === d).map((p) => p.name);
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Devices" }), _jsx("button", { className: "btn primary", onClick: () => setEditing("new"), children: "New device" })] }), _jsx("p", { className: "muted", children: "A device is an emulated endpoint on a port: a MAC address, a VLAN stack and optionally IP addresses (it answers ARP, ND and ping). Protocols run on devices." }), _jsx(ErrorBox, { error: devs.error }), members > 0 && (_jsxs("p", { className: "muted small-note", children: [members, " more devices belong to ", _jsx("a", { href: `#/a/${encodeURIComponent(agentId)}/groups`, children: "device groups" }), "."] })), !devs.loading && list.length === 0 ? (_jsx(Empty, { title: "No devices yet" })) : (_jsx("div", { className: "table-wrap", children: _jsxs("table", { className: "table", children: [_jsx("thead", { children: _jsxs("tr", { children: [_jsx("th", { children: "Name" }), _jsx("th", { children: "Port" }), _jsx("th", { children: "MAC" }), _jsx("th", { children: "VLANs (outer first)" }), _jsx("th", { children: "Addresses" }), _jsx("th", { children: "Protocols" }), _jsx("th", {})] }) }), _jsx("tbody", { children: list.map((d) => {
                                const u = using(d.name);
                                const hasIP = d.addresses.length > 0 || d.dynamicAddresses.length > 0;
                                const gws = [d.gateway4 || d.dynamicGateway4, d.gateway6 || d.dynamicGateway6].filter(Boolean);
                                return (_jsxs("tr", { children: [_jsx("td", { className: "mono strong", children: d.name }), _jsx("td", { className: "mono", children: d.port }), _jsxs("td", { className: "mono", children: [d.mac, d.macDerived && (_jsx("span", { className: "tag", title: "Derived from host and device name", children: "auto" }))] }), _jsx("td", { className: "mono", children: vlanText(d.vlans) }), _jsxs("td", { className: "mono addr-cell", children: [d.addresses.map((a) => (_jsx("div", { children: a }, a))), d.dynamicAddresses.map((a) => (_jsxs("div", { children: [a, _jsx("span", { className: "tag", title: "Leased by a DHCP client on this device", children: "dhcp" })] }, a))), !hasIP && "—", gws.length > 0 && _jsxs("div", { className: "muted", children: ["gw ", gws.join(", ")] })] }), _jsx("td", { className: "mono", children: u.join(", ") || "—" }), _jsxs("td", { className: "actions", children: [_jsx("button", { className: "btn small", disabled: !hasIP, title: hasIP ? "Neighbors, counters and resolve" : "No IP addresses", onClick: () => setIpOf(d.name), children: "IP" }), _jsx("button", { className: "btn small", disabled: !hasIP, title: hasIP ? "Start an ICMP flow from this device" : "No IP addresses", onClick: () => setPingFrom(d), children: "Ping\u2026" }), _jsx("button", { className: "btn small", title: "A device group of devices like this one, with its protocols", onClick: () => setGroupFrom(d), children: "Make group\u2026" }), _jsx("button", { className: "btn small", disabled: busy, onClick: () => setEditing(d), children: "Edit" }), _jsx("button", { className: "btn small danger-ghost", disabled: busy, onClick: () => {
                                                        const extra = u.length ? `\n\nIts protocols (${u.join(", ")}) will stop and wait for a device with this name.` : "";
                                                        if (confirm(`Delete device ${d.name}?${extra}`))
                                                            run(() => c.deleteDevice({ name: d.name }), `Deleted ${d.name}`).then(devs.refresh);
                                                    }, children: "Delete" })] })] }, d.name));
                            }) })] }) })), pingFrom && (_jsx(CreateProtocol, { agentId: agentId, category: "traffic", preset: {
                    type: "icmp",
                    title: `Ping from ${pingFrom.name}`,
                    device: pingFrom.name,
                    name: uniqueName(`ping-${pingFrom.name}`, protos.data?.protocols.map((p) => p.name) ?? []),
                    config: { auto_start: true, destination: pingFrom.gateway4 || pingFrom.gateway6 || pingFrom.dynamicGateway4 || pingFrom.dynamicGateway6 || "" },
                }, onClose: () => setPingFrom(undefined), onDone: protos.refresh })), groupFrom && (_jsx(GroupEditor, { agentId: agentId, initial: groupSpecFrom(groupFrom, (protos.data?.protocols ?? []).filter((p) => p.device === groupFrom.name && !p.group)), note: `Started from ${groupFrom.name} and its protocols. Name the group, set how many members, and step (±) what must differ per member: addresses, VLANs, IDs.${groupFrom.macDerived ? "" : " Its fixed MAC is left out: each member gets its own."}`, onClose: () => setGroupFrom(undefined), onDone: () => undefined })), ipOf && _jsx(DeviceIP, { agentId: agentId, device: ipOf, onClose: () => setIpOf(undefined) }), editing && (_jsx(DeviceForm, { agentId: agentId, device: editing === "new" ? undefined : editing, protocols: editing === "new" ? [] : using(editing.name), onClose: () => setEditing(undefined), onDone: () => {
                    devs.refresh();
                    protos.refresh();
                } }))] }));
}
const toTags = (v) => v.map((t) => ({ tpid: t.tpid || 0x8100, vid: String(t.vid), pcp: t.pcp ? String(t.pcp) : "" }));
// DeviceForm creates a device, or edits one when `device` is set. The name
// is the device's key and can't change.
function DeviceForm({ agentId, device, protocols, onClose, onDone, }) {
    const c = client(agentId);
    const editing = !!device;
    const ports = usePoll(() => c.listPorts({}), [agentId]);
    const [name, setName] = useState(device?.name ?? "");
    const [port, setPort] = useState(device?.port ?? "");
    const [derived, setDerived] = useState(device ? device.macDerived : true);
    const [mac, setMac] = useState(device && !device.macDerived ? device.mac : "");
    const [tags, setTags] = useState(device ? toTags(device.vlans) : []);
    const [addrs, setAddrs] = useState(device?.addresses.length ? [...device.addresses] : [""]);
    const [gw4, setGw4] = useState(device?.gateway4 ?? "");
    const [gw6, setGw6] = useState(device?.gateway6 ?? "");
    const { busy, run } = useBusy();
    const p = port || ports.data?.ports[0]?.name || "eth1";
    const set = (i, t) => setTags((x) => x.map((y, j) => (j === i ? { ...y, ...t } : y)));
    const vlans = tags.map((t) => ({ tpid: t.tpid, vid: Number(t.vid), pcp: Number(t.pcp || 0) }));
    // Only port, MAC and VLAN changes restart protocols; IP changes apply in place.
    const l2Changed = !!device &&
        (p !== device.port ||
            (derived ? !device.macDerived : device.macDerived || mac.toLowerCase() !== device.mac.toLowerCase()) ||
            vlanText(vlans) !== vlanText(device.vlans));
    const submit = () => {
        const d = {
            name,
            port: p,
            mac: derived ? "" : mac,
            vlans,
            addresses: addrs.map((a) => a.trim()).filter(Boolean),
            gateway4: gw4.trim(),
            gateway6: gw6.trim(),
        };
        const call = () => (editing ? c.updateDevice({ device: d }) : c.createDevice({ device: d }));
        run(call, editing ? `Updated ${name}` : `Created ${name}`).then((r) => {
            if (r) {
                onDone();
                onClose();
            }
        });
    };
    return (_jsxs(Modal, { title: editing ? `Edit device ${device.name}` : "New device", onClose: onClose, children: [editing && l2Changed && protocols.length > 0 && (_jsxs("div", { className: "warn-box", children: ["Saving restarts the protocols on this device (", protocols.join(", "), "): their counters and remote state are reset."] })), _jsxs("form", { onSubmit: (e) => {
                    e.preventDefault();
                    submit();
                }, children: [_jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsxs("label", { className: "field-label", children: ["name", _jsx("span", { className: "req", children: "*" })] }), _jsx("input", { className: "input mono", value: name, onChange: (e) => setName(e.target.value), required: true, autoFocus: !editing, readOnly: editing, title: editing ? "The name identifies the device; delete and recreate to rename" : undefined, pattern: "[A-Za-z0-9][A-Za-z0-9_.:\\-]{0,62}" })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "port" }), _jsx("input", { className: "input mono", list: "ports", value: p, onChange: (e) => setPort(e.target.value) }), _jsx("datalist", { id: "ports", children: ports.data?.ports.map((x) => (_jsx("option", { value: x.name }, x.name))) }), _jsx("div", { className: "hint", children: "Ports that don't exist yet are fine: the device starts working when the port appears." })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "mac" }), _jsx("input", { className: "input mono", value: derived ? "" : mac, disabled: derived, onChange: (e) => setMac(e.target.value), placeholder: derived ? (device?.macDerived ? device.mac : "derived from host + name") : "02:00:00:00:00:01", required: !derived, pattern: "([0-9A-Fa-f]{2}[:\\-]){5}[0-9A-Fa-f]{2}", "aria-label": "MAC address" }), _jsxs("label", { className: "switch small", children: [_jsx("input", { type: "checkbox", checked: derived, onChange: (e) => {
                                                    setDerived(e.target.checked);
                                                    if (!e.target.checked && !mac && device)
                                                        setMac(device.mac);
                                                } }), _jsx("span", { children: "use derived MAC" })] })] })] }), _jsx("h4", { children: "VLAN stack" }), tags.length === 0 && _jsx("div", { className: "muted", children: "Untagged." }), tags.map((t, i) => (_jsxs("div", { className: "vlan-row", children: [_jsx("span", { className: "muted", children: tags.length === 1 ? "tag" : i === 0 ? "outer" : i === tags.length - 1 ? "inner" : `tag ${i + 1}` }), _jsx("select", { className: "input", value: t.tpid, onChange: (e) => set(i, { tpid: Number(e.target.value) }), "aria-label": "TPID", children: TPIDS.map((x) => (_jsx("option", { value: x, children: hex(x) }, x))) }), _jsx("input", { className: "input mono", type: "number", min: 0, max: 4095, required: true, placeholder: "VID", value: t.vid, onChange: (e) => set(i, { vid: e.target.value }), "aria-label": "VLAN ID" }), _jsx("input", { className: "input mono", type: "number", min: 0, max: 7, placeholder: "PCP", value: t.pcp, onChange: (e) => set(i, { pcp: e.target.value }), "aria-label": "PCP" }), _jsx("button", { type: "button", className: "icon-btn", onClick: () => setTags((x) => x.filter((_, j) => j !== i)), "aria-label": "Remove tag", children: "\u00D7" })] }, i))), _jsxs("div", { className: "btn-row", children: [tags.length < 4 && (_jsx("button", { type: "button", className: "btn small", onClick: () => setTags((x) => [...x, { tpid: 0x8100, vid: "", pcp: "" }]), children: "Add tag" })), tags.length === 1 && (_jsx("button", { type: "button", className: "link", onClick: () => setTags((x) => [{ ...x[0], tpid: 0x88a8 }, { tpid: 0x8100, vid: "", pcp: "" }]), children: "make it QinQ (0x88a8 outer)" })), tags.length > 0 && (_jsx("button", { type: "button", className: "link", onClick: () => setTags([]), children: "remove all tags" }))] }), _jsx("h4", { children: "IP" }), addrs.map((a, i) => (_jsxs("div", { className: "vlan-row ip-row", children: [_jsx("span", { className: "muted", children: i === 0 ? "address" : "" }), _jsx("input", { className: "input mono", value: a, placeholder: "10.0.0.2/24 or 2001:db8::2/64", "aria-label": "IP address", onChange: (e) => setAddrs((x) => x.map((y, j) => (j === i ? e.target.value : y))) }), _jsx("button", { type: "button", className: "icon-btn", onClick: () => setAddrs((x) => (x.length > 1 ? x.filter((_, j) => j !== i) : [""])), "aria-label": "Remove address", children: "\u00D7" })] }, i))), _jsx("div", { className: "btn-row", children: _jsx("button", { type: "button", className: "btn small", onClick: () => setAddrs((x) => [...x, ""]), children: "Add address" }) }), _jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "gateway4" }), _jsx("input", { className: "input mono", value: gw4, onChange: (e) => setGw4(e.target.value), placeholder: "optional, e.g. 10.0.0.1" })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "gateway6" }), _jsx("input", { className: "input mono", value: gw6, onChange: (e) => setGw6(e.target.value), placeholder: "optional, e.g. fe80::1" })] })] }), _jsx("div", { className: "hint", children: "The device answers ARP, ND and ping on these addresses. With any IPv6 address an EUI-64 link-local is added automatically." }), _jsxs("div", { className: "modal-foot", children: [_jsx("button", { type: "button", className: "btn", onClick: onClose, children: "Cancel" }), _jsx("button", { className: "btn primary", disabled: busy || !name, children: editing ? "Save" : "Create" })] })] })] }));
}
function uniqueName(base, taken) {
    if (!taken.includes(base))
        return base;
    for (let i = 2;; i++)
        if (!taken.includes(`${base}-${i}`))
            return `${base}-${i}`;
}
// groupSpecFrom starts a group spec from a device and its protocols: the
// device's port, VLANs, addresses and gateways (not a fixed MAC, which
// members can't share) and each protocol's config.
function groupSpecFrom(d, ps) {
    const device = { port: d.port };
    if (d.vlans.length)
        device.vlans = d.vlans.map((t) => ({ vid: t.vid, ...(t.tpid && t.tpid !== 0x8100 ? { tpid: t.tpid } : {}), ...(t.pcp ? { pcp: t.pcp } : {}) }));
    if (d.addresses.length)
        device.addresses = [...d.addresses];
    if (d.gateway4)
        device.gateway4 = d.gateway4;
    if (d.gateway6)
        device.gateway6 = d.gateway6;
    return { name: "", count: 10, device, protocols: ps.map((p) => ({ name: p.name, type: p.type, config: (p.config ?? {}) })) };
}
