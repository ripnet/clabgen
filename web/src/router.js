import { useEffect, useState } from "react";
export const agentTabs = ["ports", "devices", "groups", "protocols", "traffic", "capture", "events", "scenario"];
export function parse(hash) {
    const parts = hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
    if (parts[0] === "a" && parts[1]) {
        if (parts[2] === "p" && parts[3])
            return { page: "protocol", agent: parts[1], protocol: parts[3] };
        if (parts[2] === "g" && parts[3])
            return { page: "group", agent: parts[1], group: parts[3] };
        const tab = agentTabs.includes(parts[2]) ? parts[2] : "protocols";
        return { page: "agent", agent: parts[1], tab };
    }
    if (parts[0] === "flows")
        return { page: "flows" };
    return { page: "overview" };
}
export function href(r) {
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
export function navigate(r) {
    window.location.hash = href(r);
}
export function useRoute() {
    const [route, setRoute] = useState(() => parse(window.location.hash));
    useEffect(() => {
        const f = () => setRoute(parse(window.location.hash));
        window.addEventListener("hashchange", f);
        return () => window.removeEventListener("hashchange", f);
    }, []);
    return route;
}
