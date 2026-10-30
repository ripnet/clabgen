import { jsx as _jsx, jsxs as _jsxs, Fragment as _Fragment } from "react/jsx-runtime";
import { useEffect, useRef, useState } from "react";
import { client, usePoll } from "../api";
import { agentLabel, useAgents } from "../agents";
import { ErrorBox, useBusy } from "../ui";
export function ScenarioTab({ agentId }) {
    const c = client(agentId);
    const sc = usePoll(() => c.getScenario({}), [agentId]);
    const agentInfo = usePoll(() => c.getInfo({}), [agentId], 5000);
    const cfg = agentInfo.data?.persistence;
    const [text, setText] = useState("");
    const [dirty, setDirty] = useState(false);
    const { busy, run } = useBusy();
    const file = useRef(null);
    const { agents, info } = useAgents();
    const label = agentLabel(agents.find((a) => a.id === agentId), info[agentId]);
    useEffect(() => {
        if (sc.data && !dirty)
            setText(sc.data.yaml);
    }, [sc.data, dirty]);
    const apply = (replace) => {
        if (replace && !confirm("Replace removes every existing device and protocol on this agent before applying. Continue?"))
            return;
        run(() => c.applyScenario({ yaml: text, replace }), replace ? "Scenario applied (replaced)" : "Scenario applied").then((r) => {
            if (r) {
                setDirty(false);
                sc.refresh();
            }
        });
    };
    return (_jsxs("div", { className: "card", children: [_jsxs("div", { className: "card-head", children: [_jsx("h2", { children: "Scenario" }), _jsxs("div", { className: "btn-row", children: [_jsx("button", { className: "btn", onClick: () => { setDirty(false); sc.refresh(); }, disabled: busy, children: dirty ? "Discard edits" : "Reload" }), _jsx("button", { className: "btn", onClick: () => file.current?.click(), children: "Open file\u2026" }), _jsx("input", { ref: file, type: "file", accept: ".yml,.yaml,text/yaml", hidden: true, onChange: (e) => {
                                    const f = e.target.files?.[0];
                                    if (f)
                                        f.text().then((t) => { setText(t); setDirty(true); });
                                    e.target.value = "";
                                } }), _jsx("button", { className: "btn", onClick: () => {
                                    const a = document.createElement("a");
                                    a.href = URL.createObjectURL(new Blob([text], { type: "text/yaml" }));
                                    a.download = `${label}.scenario.yml`;
                                    a.click();
                                    URL.revokeObjectURL(a.href);
                                }, children: "Download" }), _jsx("button", { className: "btn", onClick: () => apply(false), disabled: busy || !text.trim(), children: "Apply (merge)" }), _jsx("button", { className: "btn primary", onClick: () => apply(true), disabled: busy || !text.trim(), children: "Apply & replace" })] })] }), _jsxs("p", { className: "muted", children: ["The agent's whole configuration as YAML: the same format that ", _jsx("code", { children: "CLABGEN_SCENARIO" }), " loads at boot. Merge adds to what's there and reports conflicts; replace starts clean."] }), cfg && _jsx(SavedConfig, { cfg: cfg, busy: busy, reset: () => run(() => c.resetConfig({}), "Config reset").then((r) => r && (setDirty(false), sc.refresh(), agentInfo.refresh())) }), _jsx(ErrorBox, { error: sc.error }), _jsx("textarea", { className: "input mono code scenario", spellCheck: false, value: text, onChange: (e) => {
                    setText(e.target.value);
                    setDirty(true);
                } }), dirty && _jsx("div", { className: "hint", children: "Edited, not applied yet." })] }));
}
// SavedConfig says where the agent keeps its config across lab restarts,
// and resets it to the topology's scenario.
function SavedConfig({ cfg, busy, reset }) {
    if (!cfg.file)
        return (_jsxs("div", { className: "muted small-note saved-config", children: ["Not saved across lab restarts: ", cfg.notSaved || "no file", ". See ", _jsx("a", { href: "https://clabgen.dev/configuration/#saved-config", target: "_blank", rel: "noreferrer", children: "saved config" }), "."] }));
    const from = cfg.source === "saved" ? "restored from it" : cfg.source === "scenario" ? _jsxs(_Fragment, { children: ["started from the topology's scenario (", _jsx("code", { children: cfg.scenario }), ")"] }) : "started empty";
    const to = cfg.scenario ? "the topology's scenario" : "an empty config";
    return (_jsxs("div", { className: "saved-config card-lite", children: [_jsxs("div", { children: ["Saved to ", _jsx("code", { children: cfg.file }), " after every change, and restored when the lab restarts (this start: ", from, ").", cfg.source === "saved" && cfg.scenario && _jsx("span", { className: "muted", children: " Edits to the scenario file apply after a reset." })] }), cfg.error && _jsxs("div", { className: "error-box", children: ["Saving failed: ", cfg.error] }), _jsx("div", { className: "btn-row end", children: _jsxs("button", { className: "btn", disabled: busy, onClick: () => confirm(`Reset replaces everything on this agent with ${to}, and saves that instead. Continue?`) && reset(), title: `Delete the saved file and load ${to}`, children: ["Reset to ", to] }) })] }));
}
