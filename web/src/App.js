import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useState } from "react";
import { agentsApi } from "./api";
import { agentLabel, useAgents } from "./agents";
import { href, useRoute } from "./router";
import { Dot, ErrorBox, Modal, useBusy, useTheme } from "./ui";
import { Overview } from "./pages/Overview";
import { FlowsPage } from "./pages/FlowsPage";
import { displayVersion, skew } from "./version";
import { AgentPage } from "./pages/AgentPage";
import { ProtocolDetail } from "./pages/ProtocolDetail";
import { GroupDetail } from "./pages/GroupsTab";
export function App() {
    const route = useRoute();
    const [theme, setTheme] = useTheme();
    const [navOpen, setNavOpen] = useState(false);
    const current = route.page === "overview" || route.page === "flows" ? undefined : route.agent;
    return (_jsxs("div", { className: `shell ${navOpen ? "nav-open" : ""}`, children: [_jsxs("header", { className: "topbar", children: [_jsx("button", { className: "icon-btn nav-toggle", onClick: () => setNavOpen(!navOpen), "aria-label": "Toggle navigation", children: "\u2630" }), _jsxs("a", { className: "brand", href: "#/", children: [_jsxs("svg", { viewBox: "0 0 32 32", width: "22", height: "22", "aria-hidden": "true", children: [_jsx("rect", { width: "32", height: "32", rx: "7", fill: "var(--accent)" }), _jsx("path", { d: "M6 20h5l3-9 4 13 3-8h5", fill: "none", stroke: "white", strokeWidth: "2.6", strokeLinecap: "round", strokeLinejoin: "round" })] }), "clabgen"] }), _jsx(UIVersion, {}), _jsx("div", { className: "grow" }), _jsx(ThemeSwitch, { theme: theme, setTheme: setTheme })] }), _jsx("aside", { className: "sidebar", onClick: (e) => e.target.closest("a") && setNavOpen(false), children: _jsx(Sidebar, { current: current, page: route.page }) }), _jsxs("main", { className: "main", children: [route.page === "overview" && _jsx(Overview, {}), route.page === "flows" && _jsx(FlowsPage, {}), route.page === "agent" && _jsx(AgentPage, { agentId: route.agent, tab: route.tab }, route.agent), route.page === "protocol" && _jsx(ProtocolDetail, { agentId: route.agent, name: route.protocol }, route.agent + "/" + route.protocol), route.page === "group" && _jsx(GroupDetail, { agentId: route.agent, name: route.group }, route.agent + "/" + route.group)] })] }));
}
// UIVersion shows the version serving the UI, and a newer release.
function UIVersion() {
    const { ui } = useAgents();
    if (!ui?.version)
        return null;
    return (_jsxs(_Fragment, { children: [_jsx("a", { className: "brand-version muted", href: "https://github.com/ripnet/clabgen", target: "_blank", rel: "noreferrer", title: "clabgen by Tom Young and contributors", children: displayVersion(ui.version) }), ui.update_available && ui.latest && (_jsxs("a", { className: "update-link", href: ui.latest_url, target: "_blank", rel: "noreferrer", title: "Update: docker pull ghcr.io/ripnet/clabgen-ui and recreate the container; redeploy labs to update their agents", children: [displayVersion(ui.latest), " available"] }))] }));
}
function ThemeSwitch({ theme, setTheme }) {
    return (_jsx("div", { className: "seg small", role: "radiogroup", "aria-label": "Theme", children: ["auto", "light", "dark"].map((t) => (_jsx("button", { role: "radio", "aria-checked": theme === t, className: theme === t ? "seg-btn active" : "seg-btn", onClick: () => setTheme(t), children: t }, t))) }));
}
function Sidebar({ current, page }) {
    const { agents, info, ui, allowManual, error, refresh } = useAgents();
    const [adding, setAdding] = useState(false);
    const groups = new Map();
    for (const a of agents) {
        const g = a.source === "self" ? "this agent" : a.lab || "agents";
        groups.set(g, [...(groups.get(g) ?? []), a]);
    }
    return (_jsxs("nav", { "aria-label": "Agents", children: [_jsx("a", { className: `side-link ${page === "overview" ? "active" : ""}`, href: "#/", children: "Lab overview" }), _jsx("a", { className: `side-link ${page === "flows" ? "active" : ""}`, href: "#/flows", children: "Flows" }), error && _jsx(ErrorBox, { error: `Agent list: ${error}` }), [...groups.entries()].map(([g, list]) => (_jsxs("div", { className: "side-group", children: [_jsx("div", { className: "side-group-title", children: g }), list.map((a) => {
                        const i = info[a.id];
                        const k = i?.reachable ? skew(i.version, ui?.version) : undefined;
                        return (_jsxs("a", { className: `side-link ${current === a.id ? "active" : ""}`, href: href({ page: "agent", agent: a.id, tab: "protocols" }), title: i?.reachable ? `${i.hostname} · ${displayVersion(i.version)} · ${a.url ?? "local"}${k && k.kind !== "differs" ? `\n${k.text}` : ""}` : i?.error, children: [_jsx(Dot, { health: i?.reachable ? "ok" : i ? "bad" : "idle" }), _jsx("span", { className: "side-name", children: agentLabel(a, i) }), k?.kind === "older" && _jsx("span", { className: "side-ver warn", children: "update" }), k?.kind === "newer" && _jsx("span", { className: "side-ver", children: "newer" }), a.source === "manual" && _jsx(RemoveAgent, { id: a.id, onDone: refresh })] }, a.id));
                    })] }, g))), allowManual && (_jsx("button", { className: "btn small side-add", onClick: () => setAdding(true), children: "Add agent" })), adding && _jsx(AddAgent, { onClose: () => setAdding(false), onDone: refresh })] }));
}
function RemoveAgent({ id, onDone }) {
    const { run } = useBusy();
    return (_jsx("button", { className: "icon-btn side-x", "aria-label": `Remove ${id}`, onClick: (e) => {
            e.preventDefault();
            e.stopPropagation();
            run(() => agentsApi.remove(id)).then(onDone);
        }, children: "\u00D7" }));
}
function AddAgent({ onClose, onDone }) {
    const [url, setUrl] = useState("");
    const [name, setName] = useState("");
    const { busy, run } = useBusy();
    const id = (name || url).replace(/^https?:\/\//, "").replace(/[^A-Za-z0-9_.-]/g, "-").replace(/^-+/, "");
    return (_jsx(Modal, { title: "Add agent", onClose: onClose, children: _jsxs("form", { onSubmit: (e) => {
                e.preventDefault();
                run(() => agentsApi.add({ id, name: name || undefined, url }), `Added ${name || url}`).then((r) => {
                    if (r) {
                        onDone();
                        onClose();
                    }
                });
            }, children: [_jsxs("div", { className: "form-grid", children: [_jsxs("div", { className: "field", children: [_jsxs("label", { className: "field-label", children: ["address", _jsx("span", { className: "req", children: "*" })] }), _jsx("input", { className: "input mono", value: url, onChange: (e) => setUrl(e.target.value), placeholder: "172.20.20.5 or host:8080", required: true, autoFocus: true }), _jsx("div", { className: "hint", children: "Must be reachable from the server hosting this page; calls are proxied through it." })] }), _jsxs("div", { className: "field", children: [_jsx("label", { className: "field-label", children: "name" }), _jsx("input", { className: "input", value: name, onChange: (e) => setName(e.target.value), placeholder: "optional" })] })] }), _jsx("div", { className: "hint", children: "The agent is kept until the server restarts." }), _jsxs("div", { className: "modal-foot", children: [_jsx("button", { type: "button", className: "btn", onClick: onClose, children: "Cancel" }), _jsx("button", { className: "btn primary", disabled: busy || !url || !id, children: "Add" })] })] }) }));
}
