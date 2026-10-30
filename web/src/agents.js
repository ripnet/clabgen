import { jsx as _jsx } from "react/jsx-runtime";
import { createContext, useContext, useEffect, useState } from "react";
import { agentsApi, client, uiInfoApi, usePoll } from "./api";
const AgentsCtx = createContext({ agents: [], allowManual: false, info: {}, refresh: () => { } });
export function AgentsProvider({ children }) {
    const list = usePoll(() => agentsApi.list(), [], 5000);
    // Servers from before /api/info answer with the page: no version.
    const ui = usePoll(() => uiInfoApi().catch(() => undefined), [], 30 * 60 * 1000);
    const agents = list.data?.agents ?? [];
    const ids = agents.map((a) => a.id).join("|");
    const [info, setInfo] = useState({});
    useEffect(() => {
        let cancelled = false;
        const probe = () => {
            for (const a of agents) {
                client(a.id)
                    .getInfo({}, { timeoutMs: 3000 })
                    .then((r) => ({ reachable: true, hostname: r.hostname, version: r.version, started: r.startTime ? new Date(Number(r.startTime.seconds) * 1000) : undefined }), (e) => ({ reachable: false, error: String(e?.message ?? e) }))
                    .then((i) => !cancelled && setInfo((m) => ({ ...m, [a.id]: i })));
            }
        };
        probe();
        const t = setInterval(probe, 5000);
        return () => {
            cancelled = true;
            clearInterval(t);
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [ids]);
    return (_jsx(AgentsCtx.Provider, { value: { agents, allowManual: list.data?.allow_manual ?? false, info, ui: ui.data, error: list.error, refresh: list.refresh }, children: children }));
}
export const useAgents = () => useContext(AgentsCtx);
export function agentLabel(a, info) {
    if (!a)
        return "?";
    if (a.source === "self" && info?.hostname)
        return info.hostname;
    return a.name;
}
