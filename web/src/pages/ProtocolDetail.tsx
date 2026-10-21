import { useMemo, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, errorText, usePoll, useStream } from "../api";
import { href, navigate } from "../router";
import { summarize, viewFor } from "../protocols/registry";
import { SchemaForm, defaults, parseSchema, type Obj } from "../components/SchemaForm";
import { ValueView } from "../components/ValueView";
import { Empty, ErrorBox, HealthText, Modal, useBusy } from "../ui";
import { AgentHeader } from "./AgentPage";
import type { Event as AgentEvent } from "../gen/clabgen/v1/agent_pb";
import { EventRow } from "./EventsTab";

type Result = { id: number; action: string; args: Obj; at: Date; ms: number; result?: JsonObject; error?: string };

export function ProtocolDetail({ agentId, name }: { agentId: string; name: string }) {
  const c = client(agentId);
  const p = usePoll(() => c.getProtocol({ name }), [agentId, name], 1000);
  const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
  const proto = p.data?.protocol;
  const type = types.data?.types.find((t) => t.name === proto?.type);
  const section = type?.category === "traffic" ? "traffic" : "protocols";
  const [editing, setEditing] = useState(false);
  const { busy, run } = useBusy();
  const view = viewFor(proto?.type ?? "");
  const s = proto ? summarize(proto.type, proto.state, proto.error, proto.status) : undefined;

  return (
    <div className="page">
      <AgentHeader agentId={agentId}>
        <div className="crumbs">
          <a href={href({ page: "agent", agent: agentId, tab: section })}>{section}</a> / <span className="mono">{name}</span>
        </div>
      </AgentHeader>
      {p.error && !proto && <ErrorBox error={p.error} />}
      {proto && (
        <>
          <div className="proto-head">
            <div>
              <h2 className="mono">{proto.name}</h2>
              <div className="muted">
                <span className="mono">{proto.type}</span> on device <span className="mono">{proto.device}</span>
              </div>
            </div>
            <div className="proto-state">{s && <HealthText health={s.health}>{proto.state === "running" ? s.text : `${proto.state}: ${proto.error}`}</HealthText>}</div>
            <div className="btn-row">
              <button className="btn" onClick={() => setEditing(true)} disabled={!type}>
                Edit config
              </button>
              <button
                className="btn danger-ghost"
                disabled={busy}
                onClick={() => {
                  if (confirm(`Delete protocol ${proto.name}?`))
                    run(() => c.deleteProtocol({ name: proto.name }), `Deleted ${proto.name}`).then((r) => r && navigate({ page: "agent", agent: agentId, tab: type?.category === "traffic" ? "traffic" : "protocols" }));
                }}
              >
                Delete
              </button>
            </div>
          </div>

          <div className="card">
            <div className="card-head">
              <h2>Status</h2>
              <span className="live on">live</span>
            </div>
            {proto.status ? view.Status ? <view.Status status={proto.status} agentId={agentId} name={proto.name} /> : <ValueView value={proto.status} /> : <div className="muted">Not running.</div>}
          </div>

          <div className="detail-grid">
            {type && type.actions.length > 0 ? (
              <Actions agentId={agentId} name={proto.name} actions={type.actions} disabled={proto.state !== "running"} />
            ) : (
              <div />
            )}
            <ProtocolEvents agentId={agentId} name={proto.name} />
          </div>

          <details className="card config-card">
            <summary>
              <h2>Configuration</h2>
            </summary>
            <ValueView value={proto.config ?? {}} />
          </details>

          {editing && type && (
            <EditConfig
              schemaText={type.configSchema}
              value={(proto.config ?? {}) as Obj}
              onClose={() => setEditing(false)}
              onSave={(cfg) =>
                run(() => c.updateProtocol({ name: proto.name, config: cfg as JsonObject }), "Configuration applied").then((r) => {
                  if (r) {
                    setEditing(false);
                    p.refresh();
                  }
                })
              }
              busy={busy}
            />
          )}
        </>
      )}
    </div>
  );
}

function EditConfig({ schemaText, value, onClose, onSave, busy }: { schemaText: string; value: Obj; onClose: () => void; onSave: (v: Obj) => void; busy: boolean }) {
  const schema = useMemo(() => parseSchema(schemaText), [schemaText]);
  const [cfg, setCfg] = useState<Obj>(value);
  return (
    <Modal title="Edit configuration" onClose={onClose} wide>
      <div className="warn-box">Applying restarts the instance: counters and the remote state are reset.</div>
      <SchemaForm schema={schema} value={cfg} onChange={setCfg} mode="config" />
      <div className="modal-foot">
        <button className="btn" onClick={onClose}>
          Cancel
        </button>
        <button className="btn primary" disabled={busy} onClick={() => onSave(cfg)}>
          Apply
        </button>
      </div>
    </Modal>
  );
}

function Actions({ agentId, name, actions, disabled }: { agentId: string; name: string; actions: { name: string; description: string; argsSchema: string }[]; disabled: boolean }) {
  const [sel, setSel] = useState(actions[0].name);
  const action = actions.find((a) => a.name === sel) ?? actions[0];
  const schema = useMemo(() => parseSchema(action.argsSchema), [action.argsSchema]);
  const [argsBy, setArgsBy] = useState<Record<string, Obj>>({});
  const args = argsBy[action.name] ?? defaults(schema);
  const [running, setRunning] = useState(false);
  const [results, setResults] = useState<Result[]>([]);

  const runAction = async () => {
    setRunning(true);
    const t0 = performance.now();
    const base = { id: Date.now(), action: action.name, args, at: new Date() };
    try {
      const r = await client(agentId).runProtocolAction({ name, action: action.name, args: args as JsonObject });
      setResults((x) => [{ ...base, ms: performance.now() - t0, result: r.result }, ...x].slice(0, 10));
    } catch (e) {
      setResults((x) => [{ ...base, ms: performance.now() - t0, error: errorText(e) }, ...x].slice(0, 10));
    } finally {
      setRunning(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Actions</h2>
      </div>
      <div className="seg" role="tablist">
        {actions.map((a) => (
          <button key={a.name} role="tab" aria-selected={a.name === action.name} className={a.name === action.name ? "seg-btn active" : "seg-btn"} onClick={() => setSel(a.name)}>
            {a.name}
          </button>
        ))}
      </div>
      <p className="muted action-desc">{action.description}</p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          runAction();
        }}
      >
        <SchemaForm schema={schema} value={args} onChange={(v) => setArgsBy((m) => ({ ...m, [action.name]: v }))} mode="args" />
        <div className="btn-row end">
          <button type="button" className="btn" onClick={() => setArgsBy((m) => ({ ...m, [action.name]: defaults(schema) }))}>
            Reset
          </button>
          <button className="btn primary" disabled={running || disabled}>
            {running ? "Running…" : `Run ${action.name}`}
          </button>
        </div>
      </form>
      {results.map((r, i) => (
        <div key={r.id} className={`result ${i > 0 ? "old" : ""}`}>
          <div className="result-head">
            <span className="mono strong">{r.action}</span>
            <span className="muted">
              {r.at.toLocaleTimeString()} · {(r.ms / 1000).toFixed(2)} s
            </span>
          </div>
          {r.error ? <div className="error-box">{r.error}</div> : i === 0 ? <ValueView value={r.result ?? {}} /> : <ResultSummary r={r.result} />}
        </div>
      ))}
    </div>
  );
}

function ResultSummary({ r }: { r?: JsonObject }) {
  if (!r) return null;
  const parts = Object.entries(r)
    .filter(([, v]) => typeof v !== "object")
    .slice(0, 6)
    .map(([k, v]) => `${k.replace(/_/g, " ")}: ${String(v)}`);
  return <div className="muted mono small">{parts.join(" · ")}</div>;
}

function ProtocolEvents({ agentId, name }: { agentId: string; name: string }) {
  const [events, setEvents] = useState<AgentEvent[]>([]);
  const source = `protocol/${name}`;
  useStream(
    (signal) => {
      setEvents([]); // the stream replays history on every (re)connect
      return client(agentId).watchEvents({ replay: true }, { signal });
    },
    (e) => e.source === source && setEvents((x) => [e, ...x].slice(0, 50)),
    [agentId, name],
  );
  return (
    <div className="card">
      <div className="card-head">
        <h2>Recent events</h2>
        <a className="muted" href={href({ page: "agent", agent: agentId, tab: "events" })}>
          all events →
        </a>
      </div>
      {events.length === 0 ? (
        <Empty title="No events yet" />
      ) : (
        <div className="events compact">
          {events.map((e, i) => (
            <EventRow key={i} e={e} hideSource />
          ))}
        </div>
      )}
    </div>
  );
}
