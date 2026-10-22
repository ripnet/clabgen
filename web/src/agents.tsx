import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { agentsApi, client, uiInfoApi, usePoll, type AgentRef, type UIInfo } from "./api";

export type AgentInfo = { reachable: boolean; hostname?: string; version?: string; started?: Date; error?: string };

type Ctx = {
  agents: AgentRef[];
  allowManual: boolean;
  info: Record<string, AgentInfo>;
  ui?: UIInfo; // the UI's own version (and a newer release)
  error?: string;
  refresh: () => void;
};

const AgentsCtx = createContext<Ctx>({ agents: [], allowManual: false, info: {}, refresh: () => {} });

export function AgentsProvider({ children }: { children: ReactNode }) {
  const list = usePoll(() => agentsApi.list(), [], 5000);
  // Servers from before /api/info answer with the page: no version.
  const ui = usePoll(() => uiInfoApi().catch(() => undefined), [], 30 * 60 * 1000);
  const agents = list.data?.agents ?? [];
  const ids = agents.map((a) => a.id).join("|");
  const [info, setInfo] = useState<Record<string, AgentInfo>>({});

  useEffect(() => {
    let cancelled = false;
    const probe = () => {
      for (const a of agents) {
        client(a.id)
          .getInfo({}, { timeoutMs: 3000 })
          .then(
            (r) => ({ reachable: true, hostname: r.hostname, version: r.version, started: r.startTime ? new Date(Number(r.startTime.seconds) * 1000) : undefined }),
            (e) => ({ reachable: false, error: String(e?.message ?? e) }),
          )
          .then((i: AgentInfo) => !cancelled && setInfo((m) => ({ ...m, [a.id]: i })));
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

  return (
    <AgentsCtx.Provider value={{ agents, allowManual: list.data?.allow_manual ?? false, info, ui: ui.data, error: list.error, refresh: list.refresh }}>
      {children}
    </AgentsCtx.Provider>
  );
}

export const useAgents = () => useContext(AgentsCtx);

export function agentLabel(a: AgentRef | undefined, info?: AgentInfo): string {
  if (!a) return "?";
  if (a.source === "self" && info?.hostname) return info.hostname;
  return a.name;
}
