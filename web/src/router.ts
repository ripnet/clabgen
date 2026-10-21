import { useEffect, useState } from "react";

// Routes (hash based so any static server or proxy path works):
//   #/                         lab overview
//   #/a/<agent>/<tab>          agent tab: ports | devices | protocols | traffic | events | scenario
//   #/a/<agent>/p/<protocol>   protocol detail
export type Route =
  | { page: "overview" }
  | { page: "agent"; agent: string; tab: AgentTab }
  | { page: "protocol"; agent: string; protocol: string };

export const agentTabs = ["ports", "devices", "protocols", "traffic", "events", "scenario"] as const;
export type AgentTab = (typeof agentTabs)[number];

export function parse(hash: string): Route {
  const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  if (parts[0] === "a" && parts[1]) {
    if (parts[2] === "p" && parts[3]) return { page: "protocol", agent: parts[1], protocol: parts[3] };
    const tab = (agentTabs as readonly string[]).includes(parts[2]) ? (parts[2] as AgentTab) : "protocols";
    return { page: "agent", agent: parts[1], tab };
  }
  return { page: "overview" };
}

export function href(r: Route): string {
  const e = encodeURIComponent;
  switch (r.page) {
    case "overview":
      return "#/";
    case "agent":
      return `#/a/${e(r.agent)}/${r.tab}`;
    case "protocol":
      return `#/a/${e(r.agent)}/p/${e(r.protocol)}`;
  }
}

export function navigate(r: Route) {
  window.location.hash = href(r);
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parse(window.location.hash));
  useEffect(() => {
    const f = () => setRoute(parse(window.location.hash));
    window.addEventListener("hashchange", f);
    return () => window.removeEventListener("hashchange", f);
  }, []);
  return route;
}
