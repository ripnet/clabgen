import { createClient, ConnectError } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import { useCallback, useEffect, useRef, useState } from "react";
import { AgentService } from "./gen/clabgen/v1/agent_pb";
const clients = new Map();
// Every call goes to our own origin; the server proxies /agents/<id>/ to the agent.
export function client(agentId) {
    let c = clients.get(agentId);
    if (!c) {
        const base = new URL(`agents/${encodeURIComponent(agentId)}`, document.baseURI).toString();
        c = createClient(AgentService, createConnectTransport({ baseUrl: base }));
        clients.set(agentId, c);
    }
    return c;
}
async function json(r) {
    if (!r.ok) {
        let msg = `${r.status} ${r.statusText}`;
        try {
            msg = (await r.json()).error ?? msg;
        }
        catch {
            /* not JSON */
        }
        throw new Error(msg);
    }
    return r.json();
}
export const uiInfoApi = () => fetch("api/info").then((r) => json(r));
export const agentsApi = {
    list: () => fetch("api/agents").then((r) => json(r)),
    add: (a) => fetch("api/agents", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(a) }).then((r) => json(r)),
    remove: (id) => fetch(`api/agents/${encodeURIComponent(id)}`, { method: "DELETE" }).then((r) => {
        if (!r.ok && r.status !== 204)
            throw new Error(`${r.status}`);
    }),
};
export function errorText(e) {
    if (e instanceof ConnectError)
        return e.rawMessage || e.message;
    if (e instanceof Error)
        return e.message;
    return String(e);
}
// usePoll runs fn now, whenever deps change, and every intervalMs while the
// page is visible. Results from stale deps are discarded.
export function usePoll(fn, deps, intervalMs = 0) {
    const [data, setData] = useState();
    const [error, setError] = useState();
    const [loading, setLoading] = useState(true);
    const gen = useRef(0);
    const fnRef = useRef(fn);
    fnRef.current = fn;
    const run = useCallback(() => {
        const my = gen.current;
        fnRef.current().then((d) => {
            if (gen.current !== my)
                return;
            setData(d);
            setError(undefined);
            setLoading(false);
        }, (e) => {
            if (gen.current !== my)
                return;
            setError(errorText(e));
            setLoading(false);
        });
    }, []);
    useEffect(() => {
        gen.current++;
        setData(undefined);
        setError(undefined);
        setLoading(true);
        run();
        if (!intervalMs)
            return;
        const t = setInterval(() => {
            if (document.visibilityState === "visible")
                run();
        }, intervalMs);
        return () => clearInterval(t);
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, deps);
    return { data, error, loading, refresh: run };
}
// useStream keeps a server stream open, reconnecting after failures.
export function useStream(open, onMessage, deps) {
    const [connected, setConnected] = useState(false);
    const [error, setError] = useState();
    const onRef = useRef(onMessage);
    onRef.current = onMessage;
    useEffect(() => {
        const ctl = new AbortController();
        let retry;
        const loop = async () => {
            try {
                setConnected(true);
                setError(undefined);
                for await (const m of open(ctl.signal))
                    onRef.current(m);
            }
            catch (e) {
                if (ctl.signal.aborted)
                    return;
                setError(errorText(e));
            }
            setConnected(false);
            if (!ctl.signal.aborted)
                retry = setTimeout(loop, 2000);
        };
        loop();
        return () => {
            ctl.abort();
            if (retry)
                clearTimeout(retry);
        };
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, deps);
    return { connected, error };
}
