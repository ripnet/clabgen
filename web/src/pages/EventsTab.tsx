import { useMemo, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { client, useStream } from "../api";
import type { Event as AgentEvent } from "../gen/clabgen/v1/agent_pb";

const MAX = 2000;

function kindClass(kind: string, data?: Record<string, unknown>, msg = ""): string {
  if (kind === "port-oper" || kind === "port-admin") return msg.endsWith(" up") ? "ok" : "bad";
  if (kind === "rmep-down" || kind === "port-removed") return "bad";
  if (kind === "defects") {
    const h = data?.highest;
    if (h === "none") return "ok";
    return h === "DefRDICCM" ? "warn" : "bad";
  }
  if (kind.endsWith("-up") || kind.endsWith("-discovered") || kind.endsWith("-added")) return "ok";
  return "";
}

export function EventRow({ e, hideSource }: { e: AgentEvent; hideSource?: boolean }) {
  const t = e.time ? timestampDate(e.time) : undefined;
  const data = e.data as Record<string, unknown> | undefined;
  const extra = data
    ? Object.entries(data)
        .filter(([k, v]) => v !== "" && v !== null && !(Array.isArray(v) && v.length === 0) && k !== "previous")
        .map(([k, v]) => `${k}=${Array.isArray(v) ? v.join(",") : String(v)}`)
        .join(" ")
    : "";
  return (
    <div className={`event ${kindClass(e.kind, data, e.message)}`}>
      <span className="ev-time mono">{t ? t.toLocaleTimeString(undefined, { hour12: false }) + "." + String(t.getMilliseconds()).padStart(3, "0") : ""}</span>
      {!hideSource && <span className="ev-src mono">{e.source}</span>}
      <span className="ev-kind mono">{e.kind}</span>
      <span className="ev-msg">
        {e.message}
        {extra && <span className="ev-data mono"> {extra}</span>}
      </span>
    </div>
  );
}

export function EventsTab({ agentId }: { agentId: string }) {
  const [events, setEvents] = useState<AgentEvent[]>([]);
  const [filter, setFilter] = useState("");
  const [paused, setPaused] = useState(false);
  const [queued, setQueued] = useState<AgentEvent[]>([]);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;
  const st = useStream(
    (signal) => {
      setEvents([]);
      return client(agentId).watchEvents({ replay: true }, { signal });
    },
    (e) => (pausedRef.current ? setQueued((q) => [e, ...q].slice(0, MAX)) : setEvents((x) => [e, ...x].slice(0, MAX))),
    [agentId],
  );
  const shown = useMemo(() => {
    const f = filter.trim().toLowerCase();
    if (!f) return events;
    return events.filter((e) => `${e.source} ${e.kind} ${e.message} ${JSON.stringify(e.data ?? {})}`.toLowerCase().includes(f));
  }, [events, filter]);

  return (
    <div className="card">
      <div className="card-head">
        <h2>Events</h2>
        <div className="btn-row">
          <span className={`live ${st.connected ? "on" : ""}`}>{st.connected ? "live" : st.error ? "reconnecting…" : "connecting…"}</span>
          <input className="input search" placeholder="Filter (source, kind, text)" value={filter} onChange={(e) => setFilter(e.target.value)} />
          <button
            className="btn"
            onClick={() => {
              if (paused) {
                setEvents((x) => [...queued, ...x].slice(0, MAX));
                setQueued([]);
              }
              setPaused(!paused);
            }}
          >
            {paused ? `Resume${queued.length ? ` (${queued.length})` : ""}` : "Pause"}
          </button>
          <button className="btn" onClick={() => setEvents([])}>
            Clear
          </button>
        </div>
      </div>
      <div className="events">
        {shown.length === 0 ? <div className="muted pad">No events{filter ? " match the filter" : " yet"}.</div> : shown.map((e, i) => <EventRow key={i} e={e} />)}
      </div>
    </div>
  );
}
