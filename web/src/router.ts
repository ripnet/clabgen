import { useEffect, useState } from "react";

// Routes (hash based so any static server or proxy path works):
//   #/                         lab overview
//   #/flows                    every agent's traffic flows
//   #/a/<agent>/<tab>          agent tab: ports | devices | groups | protocols | traffic | events | scenario
//                              (#/a/<agent>: the tab it was last on)
//   #/a/<agent>/p/<protocol>   protocol detail
//   #/a/<agent>/g/<group>      device group detail
export type Route =
  | { page: "overview" }
  | { page: "flows" }
  | { page: "agent"; agent: string; tab: AgentTab }
  | { page: "protocol"; agent: string; protocol: string }
  | { page: "group"; agent: string; group: string };

export const agentTabs = ["ports", "devices", "groups", "protocols", "traffic", "capture", "events", "scenario"] as const;
export type AgentTab = (typeof agentTabs)[number];

export function parse(hash: string): Route {
  const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  if (parts[0] === "a" && parts[1]) {
    if (parts[2] === "p" && parts[3]) return { page: "protocol", agent: parts[1], protocol: parts[3] };
    if (parts[2] === "g" && parts[3]) return { page: "group", agent: parts[1], group: parts[3] };
    const tab = (agentTabs as readonly string[]).includes(parts[2]) ? (parts[2] as AgentTab) : agentTab(parts[1]);
    return { page: "agent", agent: parts[1], tab };
  }
  if (parts[0] === "flows") return { page: "flows" };
  return { page: "overview" };
}

export function href(r: Route): string {
  const e = encodeURIComponent;
  switch (r.page) {
    case "overview":
      return "#/";
    case "flows":
      return "#/flows";
    case "agent":
      return `#/a/${e(r.agent)}/${r.tab}`;
    case "protocol":
      return `#/a/${e(r.agent)}/p/${e(r.protocol)}`;
    case "group":
      return `#/a/${e(r.agent)}/g/${e(r.group)}`;
  }
}

// The tab each agent was last on, and the last tab on any agent, so going
// back to an agent (or to one not visited yet) opens the tab you were using.
// Kept in the browser's storage when it can be; else for this page only.
const tabsKey = "clabgen.agentTabs";
type TabMemory = { agents: Record<string, AgentTab>; last?: AgentTab };
let tabs: TabMemory = (() => {
  try {
    const t = JSON.parse(localStorage.getItem(tabsKey) ?? "") as TabMemory;
    if (t && typeof t.agents === "object") return t;
  } catch {
    /* none kept, or storage blocked */
  }
  return { agents: {} };
})();

function rememberTab(agent: string, tab: AgentTab) {
  if (tabs.agents[agent] === tab && tabs.last === tab) return;
  tabs = { agents: { ...tabs.agents, [agent]: tab }, last: tab };
  try {
    localStorage.setItem(tabsKey, JSON.stringify(tabs));
  } catch {
    /* storage blocked: remembered for this page only */
  }
}

// agentTab is the tab to open an agent on.
export function agentTab(agent: string): AgentTab {
  return tabs.agents[agent] ?? tabs.last ?? "protocols";
}

// agentHref links to an agent on the tab it was last on.
export const agentHref = (agent: string) => href({ page: "agent", agent, tab: agentTab(agent) });

export function navigate(r: Route) {
  window.location.hash = href(r);
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parse(window.location.hash));
  useEffect(() => {
    if (route.page === "agent") rememberTab(route.agent, route.tab);
  }, [route]);
  useEffect(() => {
    const f = () => setRoute(parse(window.location.hash));
    window.addEventListener("hashchange", f);
    return () => window.removeEventListener("hashchange", f);
  }, []);
  return route;
}
