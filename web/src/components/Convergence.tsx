import { useEffect, useMemo, useRef, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, errorText } from "../api";
import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { dur } from "./Flow";
import { defaults, parseSchema, SchemaForm, type Obj } from "./SchemaForm";
import { ErrorBox, HealthText } from "../ui";

// A convergence test: what a failure, and its repair, cost the flows
// listed. The agents do the sums (GetConvergence); this runs the steps and
// shows the result. Measuring leaves statistics and checks alone.

export type Span = { start_ms: number; end_ms: number; lost: number };
export type ConvFlow = {
  agent: string;
  side: "sender" | "receiver";
  name?: string;
  device: string;
  flow_id: string;
  one_way: boolean;
  running: boolean;
  status: string;
  loss_ms: number;
  lost: number;
  first_loss_ms?: number;
  last_loss_ms?: number;
  clean_ms: number;
  resolution_ms: number;
  truncated?: boolean;
  outages: Span[];
  prefixes?: PrefixLoss;
};
export type PrefixLoss = {
  total: number;
  impacted: number;
  probe_ms: number;
  measured?: boolean;
  list: { prefix: string; lost: number; loss_ms: number; first_loss_ms: number; last_loss_ms: number; ongoing?: boolean }[];
};
export type ConvEvent = { agent: string; at_ms: number; source: string; kind: string; message: string };
export type Phase = { phase: string; step: string; flows: ConvFlow[]; events: ConvEvent[] };

const noReceiver = "no_receiver";
const unmeasured = (f: ConvFlow) => f.status === "stopped" || f.status === noReceiver;

// Each flow's measurement: an echo flow's from its sender, a one-way flow's
// from its receivers (it can't know its own loss).
export function mergeFlows(rows: ConvFlow[]): ConvFlow[] {
  const senders = new Map(rows.filter((r) => r.side === "sender").map((r) => [r.flow_id, r]));
  const received = new Set(rows.filter((r) => r.side === "receiver").map((r) => r.flow_id));
  const out: ConvFlow[] = [];
  for (const r of rows) {
    const s = senders.get(r.flow_id);
    if (r.side === "sender") {
      if (!r.one_way) out.push(r);
      else if (!received.has(r.flow_id)) out.push(r.running ? { ...r, status: noReceiver } : r);
    } else if (!s) out.push(r);
    else if (s.one_way) out.push({ ...r, name: s.name, status: s.running ? r.status : "stopped" });
  }
  return out.sort((a, b) => `${a.name}/${a.agent}/${a.device}`.localeCompare(`${b.name}/${b.agent}/${b.device}`));
}

export function summarize(flows: ConvFlow[]) {
  const loss = flows.filter((f) => !unmeasured(f)).map((f) => f.loss_ms).sort((a, b) => a - b);
  const rank = (p: number) => (loss.length ? loss[Math.max(0, Math.ceil(p * loss.length) - 1)] : 0);
  return {
    flows: loss.length,
    impacted: flows.filter((f) => !unmeasured(f) && f.lost > 0).length,
    notRestored: flows.filter((f) => f.status === "not_restored").length,
    worst: rank(1),
    median: rank(0.5),
    p95: rank(0.95),
  };
}

const settled = (flows: ConvFlow[], settleMs: number) => flows.every((f) => unmeasured(f) || (f.status !== "not_restored" && f.clean_ms >= settleMs));

// A step causes the failure or repairs it. "none": someone else does it.
type Step =
  | { kind: "none" }
  | { kind: "port"; agentId: string; port: string; up: boolean }
  | { kind: "protocol"; agentId: string; name: string; action: string; args: Obj }
  | { kind: "group"; agentId: string; group: string; protocol: string; members: string; action: string; args: Obj };

const stepText = (s: Step, label: (id: string) => string) => {
  switch (s.kind) {
    case "none":
      return "made outside clabgen";
    case "port":
      return `${label(s.agentId)}: port ${s.port} ${s.up ? "up" : "down"}`;
    case "protocol":
      return `${label(s.agentId)}: ${s.name} ${s.action}`;
    case "group":
      return `${label(s.agentId)}: group ${s.group}${s.members ? ` members ${s.members}` : ""} ${s.protocol} ${s.action}`;
  }
};

async function runStep(s: Step) {
  const c = s.kind === "none" ? undefined : client(s.agentId);
  if (s.kind === "port") await c!.setPortAdmin({ name: s.port, up: s.up });
  if (s.kind === "protocol") await c!.runProtocolAction({ name: s.name, action: s.action, args: s.args as JsonObject });
  if (s.kind === "group") {
    const r = await c!.runGroupAction({ group: s.group, protocol: s.protocol, members: s.members, action: s.action, args: s.args as JsonObject });
    const failed = r.results.filter((x) => x.error);
    if (failed.length) throw new Error(`${failed.length} of ${r.results.length} members failed: ${failed[0].error}`);
  }
}

type Agent = { id: string; label: string };

const asRows = (agent: Agent, flows: JsonObject[]) => flows.map((f) => ({ ...(f as unknown as ConvFlow), agent: agent.label }));

// Agents that don't answer aren't part of the measurement.
async function poll(agents: Agent[], ids: string[], since?: Map<string, Timestamp>) {
  const got = await Promise.allSettled(agents.map((a) => client(a.id).getConvergence({ flowIds: ids, since: since?.get(a.id) })));
  const rows: ConvFlow[] = [];
  const events: ConvEvent[] = [];
  const now = new Map<string, Timestamp>();
  const ms = (t?: Timestamp) => (t ? Number(t.seconds) * 1000 + t.nanos / 1e6 : 0);
  got.forEach((g, i) => {
    if (g.status !== "fulfilled") return;
    const a = agents[i];
    rows.push(...asRows(a, g.value.flows as unknown as JsonObject[]));
    if (g.value.now) now.set(a.id, g.value.now);
    for (const e of g.value.events) {
      events.push({ agent: a.label, at_ms: ms(e.time) - ms(since?.get(a.id)), source: e.source, kind: e.kind, message: e.message });
    }
  });
  if (!now.size && agents.length) {
    const first = got.find((g) => g.status === "rejected");
    throw new Error(first?.status === "rejected" ? errorText(first.reason) : "no agent answered");
  }
  return { flows: mergeFlows(rows), events: events.sort((x, y) => x.at_ms - y.at_ms), now };
}

const flowName = (f: ConvFlow) => f.name || f.flow_id;

// precheckProblems: loss before the failure couldn't be told from its cost.
export function precheckProblems(flows: ConvFlow[]): string[] {
  const out: string[] = [];
  for (const f of flows) {
    if (f.status === noReceiver) out.push(`${flowName(f)} has no receiver here, so it can't be measured`);
    else if (f.status !== "stopped" && f.lost > 0) out.push(`${flowName(f)} lost ${f.lost.toLocaleString()} packets in the last 5 s`);
  }
  return out;
}

type Options = { agents: Agent[]; flowIds: string[]; settleMs: number; maxMs: number };

async function measure(name: string, step: Step, label: (id: string) => string, o: Options, stop: { current: boolean }, live: (t: string) => void): Promise<Phase> {
  const mark = await poll(o.agents, o.flowIds);
  await runStep(step);
  const end = performance.now() + o.maxMs;
  for (;;) {
    const r = await poll(o.agents, o.flowIds, mark.now);
    const s = summarize(r.flows);
    const clean = settled(r.flows, o.settleMs);
    if (stop.current || performance.now() > end || (clean && (step.kind !== "none" || s.impacted > 0))) {
      // Gave up on a flow still losing a packet now and then: not restored.
      const flows = r.flows.map((f) => (f.status === "restored" && f.clean_ms < o.settleMs ? { ...f, status: "not_restored" } : f));
      return { phase: name, step: stepText(step, label), flows, events: r.events.filter((e) => !["flow-started", "flow-stopped", "flow-done"].includes(e.kind)) };
    }
    live(
      step.kind === "none" && !s.impacted
        ? "Measuring: make the failure now. No loss so far."
        : `Measuring the ${name === "trigger" ? "failure" : "repair"}: ${s.impacted} of ${s.flows} flows lost traffic, worst ${dur(s.worst)}. Waiting for them to stay clean for ${dur(o.settleMs)}.`,
    );
    await new Promise((res) => setTimeout(res, 500));
  }
}

function download(name: string, type: string, text: string) {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type }));
  a.download = name;
  a.click();
  URL.revokeObjectURL(a.href);
}

export function toCSV(phases: Phase[]): string {
  const opt = (v?: number) => (v === undefined ? "" : String(v));
  const lines = ["phase,flow,prefix,agent,device,status,loss_ms,lost,first_loss_ms,last_loss_ms,resolution_ms,truncated"];
  for (const p of phases) {
    for (const f of p.flows) {
      lines.push([p.phase, flowName(f), "", `"${f.agent}"`, `"${f.device}"`, f.status, f.loss_ms, f.lost, opt(f.first_loss_ms), opt(f.last_loss_ms), f.resolution_ms, !!f.truncated].join(","));
      for (const x of f.prefixes?.list ?? []) {
        lines.push([p.phase, flowName(f), x.prefix, `"${f.agent}"`, `"${f.device}"`, "", x.loss_ms, x.lost, x.first_loss_ms, x.last_loss_ms, f.prefixes!.probe_ms, !!f.truncated].join(","));
      }
    }
  }
  return lines.join("\n") + "\n";
}

// Pieces of the step editor's choices, per agent, loaded when picked.
function useAgentChoices(agentId: string) {
  const [ports, setPorts] = useState<string[]>([]);
  const [protocols, setProtocols] = useState<{ name: string; type: string; traffic: boolean }[]>([]);
  const [groups, setGroups] = useState<{ name: string; protocols: { name: string; type: string }[] }[]>([]);
  const [types, setTypes] = useState<{ name: string; actions: { name: string; argsSchema: string }[] }[]>([]);
  useEffect(() => {
    if (!agentId) return;
    let live = true;
    const c = client(agentId);
    c.listPorts({}).then((r) => live && setPorts(r.ports.map((p) => p.name)), () => {});
    c.listProtocolTypes({}).then(
      (r) => {
        if (!live) return;
        setTypes(r.types.map((t) => ({ name: t.name, actions: t.actions })));
        const traffic = new Set(r.types.filter((t) => t.category === "traffic").map((t) => t.name));
        c.listProtocols({ noGroupMembers: true }).then((l) => live && setProtocols(l.protocols.map((p) => ({ name: p.name, type: p.type, traffic: traffic.has(p.type) }))), () => {});
      },
      () => {},
    );
    c.listDeviceGroups({}).then(
      (r) => live && setGroups(r.groups.map((g) => ({ name: g.name, protocols: ((g.spec as { protocols?: { name?: string; type: string }[] } | undefined)?.protocols ?? []).map((p) => ({ name: p.name ?? p.type, type: p.type })) }))),
      () => {},
    );
    return () => {
      live = false;
    };
  }, [agentId]);
  return { ports, protocols, groups, types };
}

function StepEditor({ title, step, onChange, agents, allowNone }: { title: string; step: Step; onChange: (s: Step) => void; agents: Agent[]; allowNone: string }) {
  const agentId = step.kind === "none" ? "" : step.agentId;
  const { ports, protocols, groups, types } = useAgentChoices(agentId || agents[0]?.id || "");
  const first = agentId || agents[0]?.id || "";
  const choices = protocols.filter((p) => !p.traffic);
  const kind = (k: Step["kind"]) => {
    if (k === "none") onChange({ kind: "none" });
    if (k === "port") onChange({ kind: "port", agentId: first, port: ports[0] ?? "", up: title === "Repair" });
    if (k === "protocol") onChange({ kind: "protocol", agentId: first, name: "", action: "", args: {} });
    if (k === "group") onChange({ kind: "group", agentId: first, group: "", protocol: "", members: "", action: "", args: {} });
  };
  const typeName = step.kind === "protocol" ? choices.find((p) => p.name === step.name)?.type : step.kind === "group" ? groups.find((g) => g.name === step.group)?.protocols.find((p) => p.name === step.protocol)?.type : undefined;
  const actions = types.find((t) => t.name === typeName)?.actions ?? [];
  const actionName = step.kind === "protocol" || step.kind === "group" ? step.action : "";
  const argsSchema = actions.find((a) => a.name === actionName)?.argsSchema;
  const schema = useMemo(() => parseSchema(argsSchema), [argsSchema]);
  const hasArgs = !!schema && Object.keys((schema as { properties?: object }).properties ?? {}).length > 0;

  return (
    <div className="conv-step">
      <div className="conv-fields">
        <label className="field">
          <span className="field-label">{title}</span>
          <select className="input" aria-label={`${title} kind`} value={step.kind} onChange={(e) => kind(e.target.value as Step["kind"])}>
            <option value="none">{allowNone}</option>
            <option value="port">Port</option>
            <option value="protocol">Protocol action</option>
            <option value="group">Group action</option>
          </select>
        </label>
        {step.kind !== "none" && (
          <label className="field">
            <span className="field-label">Agent</span>
            <select className="input" aria-label={`${title} agent`} value={agentId} onChange={(e) => onChange({ ...step, agentId: e.target.value } as Step)}>
              {agents.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.label}
                </option>
              ))}
            </select>
          </label>
        )}
        {step.kind === "port" && (
          <>
            <label className="field">
              <span className="field-label">Port</span>
              <select className="input" aria-label={`${title} port`} value={step.port || ports[0] || ""} onChange={(e) => onChange({ ...step, port: e.target.value })}>
                {ports.map((p) => (
                  <option key={p}>{p}</option>
                ))}
              </select>
            </label>
            <label className="field">
              <span className="field-label">Set</span>
              <select className="input" aria-label={`${title} port state`} value={step.up ? "up" : "down"} onChange={(e) => onChange({ ...step, up: e.target.value === "up" })}>
                <option value="down">down</option>
                <option value="up">up</option>
              </select>
            </label>
          </>
        )}
        {step.kind === "protocol" && (
          <label className="field">
            <span className="field-label">Protocol</span>
            <select className="input" aria-label={`${title} protocol`} value={step.name} onChange={(e) => onChange({ ...step, name: e.target.value, action: "", args: {} })}>
              <option value="">Pick one</option>
              {choices.map((p) => (
                <option key={p.name}>{p.name}</option>
              ))}
            </select>
          </label>
        )}
        {step.kind === "group" && (
          <>
            <label className="field">
              <span className="field-label">Group</span>
              <select
                className="input"
                aria-label={`${title} group`}
                value={step.group}
                onChange={(e) => onChange({ ...step, group: e.target.value, protocol: groups.find((g) => g.name === e.target.value)?.protocols[0]?.name ?? "", action: "", args: {} })}
              >
                <option value="">Pick one</option>
                {groups.map((g) => (
                  <option key={g.name}>{g.name}</option>
                ))}
              </select>
            </label>
            <label className="field">
              <span className="field-label">Protocol</span>
              <select className="input" aria-label={`${title} group protocol`} value={step.protocol} onChange={(e) => onChange({ ...step, protocol: e.target.value, action: "", args: {} })}>
                {groups.find((g) => g.name === step.group)?.protocols.map((p) => <option key={p.name}>{p.name}</option>)}
              </select>
            </label>
            <label className="field">
              <span className="field-label">Members</span>
              <input className="input" aria-label={`${title} members`} placeholder="all, or 1-5,8" value={step.members} onChange={(e) => onChange({ ...step, members: e.target.value })} />
            </label>
          </>
        )}
        {(step.kind === "protocol" || step.kind === "group") && (
          <label className="field">
            <span className="field-label">Action</span>
            <select
              className="input"
              aria-label={`${title} action`}
              value={step.action}
              onChange={(e) => onChange({ ...step, action: e.target.value, args: defaults(parseSchema(actions.find((a) => a.name === e.target.value)?.argsSchema)) })}
            >
              <option value="">Pick one</option>
              {actions.map((a) => (
                <option key={a.name}>{a.name}</option>
              ))}
            </select>
          </label>
        )}
      </div>
      {(step.kind === "protocol" || step.kind === "group") && step.action && hasArgs && <SchemaForm schema={schema} value={step.args} onChange={(v) => onChange({ ...step, args: v })} mode="args" />}
    </div>
  );
}

const stepReady = (s: Step) => {
  if (s.kind === "port") return !!s.port;
  if (s.kind === "protocol") return !!s.name && !!s.action;
  if (s.kind === "group") return !!s.group && !!s.action;
  return true;
};

const statusHealth = (s: string) => (s === "not_restored" ? "bad" : s === "restored" ? "warn" : s === "no_impact" ? "ok" : "idle") as "bad" | "warn" | "ok" | "idle";
const statusWord: Record<string, string> = { no_impact: "no impact", restored: "restored", not_restored: "not restored", stopped: "stopped", [noReceiver]: "no receiver" };
const when = (v?: number) => (v === undefined ? "—" : `+${dur(v)}`);

// Nice axis ticks: about five, at 1, 2 or 5 × a power of ten.
function ticks(maxMs: number): number[] {
  const raw = maxMs / 5;
  const pow = 10 ** Math.floor(Math.log10(raw || 1));
  const step = [1, 2, 5, 10].map((m) => m * pow).find((s) => s >= raw) ?? pow * 10;
  const out = [];
  for (let t = 0; t <= maxMs; t += step) out.push(t);
  return out;
}

// One row per flow, when it lost traffic after the step, with the events
// from that time as ticks. The flows that lost the most, up to a limit.
function ConvTimeline({ phase }: { phase: Phase }) {
  const limit = 20;
  const flows = [...phase.flows].filter((f) => f.outages.length).sort((a, b) => b.loss_ms - a.loss_ms);
  const shown = flows.slice(0, limit);
  if (!shown.length) return null;
  // The scale follows the loss: a hold timer expiring long after traffic
  // recovered is in the events list, not worth squashing the bars for.
  const end = Math.max(...shown.flatMap((f) => f.outages.map((o) => o.end_ms)), 1) * 1.25;
  const events = phase.events.filter((e) => e.at_ms >= 0 && e.at_ms <= end);
  const W = 900;
  const left = 190;
  const rowH = 22;
  const top = 8;
  const H = top + shown.length * rowH + 26;
  const x = (ms: number) => left + (Math.max(ms, 0) / end) * (W - left - 12);
  return (
    <div className="conv-timeline">
      <div className="chart-head">
        <span className="chart-title">When traffic was lost, from the start of the step</span>
        <span className="conv-legend">
          <span className="conv-key conv-key-lost" /> lost traffic
          {events.length > 0 && (
            <>
              <span className="conv-key conv-key-event" /> event
            </>
          )}
        </span>
      </div>
      <svg className="chart" viewBox={`0 0 ${W} ${H}`} role="img" aria-label="Timeline of each flow's lost traffic">
        {ticks(end).map((t) => (
          <g key={t}>
            <line x1={x(t)} x2={x(t)} y1={top} y2={H - 22} className="conv-grid" />
            <text x={x(t)} y={H - 8} textAnchor="middle" className="conv-axis">
              {t === 0 ? "0" : `+${dur(t)}`}
            </text>
          </g>
        ))}
        {events.map((e, i) => (
          <line key={i} x1={x(e.at_ms)} x2={x(e.at_ms)} y1={top} y2={H - 22} className="conv-event">
            <title>{`+${dur(e.at_ms)} ${e.agent} ${e.source}: ${e.message}`}</title>
          </line>
        ))}
        {shown.map((f, i) => {
          const y = top + i * rowH;
          const label = flowName(f);
          return (
            <g key={f.agent + f.device + f.flow_id}>
              <text x={left - 8} y={y + 15} textAnchor="end" className="conv-label">
                {label.length > 24 ? label.slice(0, 23) + "…" : label}
                <title>{`${label} (${f.agent}/${f.device})`}</title>
              </text>
              {f.outages.map((o, j) => (
                <rect key={j} x={x(o.start_ms)} y={y + 4} width={Math.max(2, x(o.end_ms) - x(o.start_ms))} height={rowH - 8} rx={2} className="conv-lost">
                  <title>{`${label}: ${o.lost.toLocaleString()} packets, +${dur(Math.max(o.start_ms, 0))} to +${dur(o.end_ms)}`}</title>
                </rect>
              ))}
            </g>
          );
        })}
      </svg>
      {flows.length > limit && <div className="muted">The {limit} flows that lost the most are shown, of {flows.length}.</div>}
    </div>
  );
}

// PrefixTable lists what each of a flow's prefixes lost. Each prefix is
// probed once a cycle, so a failure shorter than that misses some.
export function PrefixTable({ p, span }: { p: PrefixLoss; span?: number }) {
  return (
    <>
      {span !== undefined && p.probe_ms > span && (
        <div className="warn-box">
          Each prefix is only probed every {dur(p.probe_ms)}, longer than this {dur(span)} of loss, so more prefixes may have been affected. Send faster, or to fewer prefixes.
        </div>
      )}
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>Prefix</th>
              <th className="num" title={`Lost packets × the ${dur(p.probe_ms)} between probes of a prefix`}>Loss duration</th>
              <th className="num">Lost</th>
              <th className="num">First loss</th>
              <th className="num">Last loss</th>
            </tr>
          </thead>
          <tbody>
            {p.list.map((x) => (
              <tr key={x.prefix}>
                <td className="mono">{x.prefix}</td>
                <td className="num mono">{dur(x.loss_ms)}</td>
                <td className="num mono">{x.lost.toLocaleString()}</td>
                <td className="num mono">{when(x.first_loss_ms)}</td>
                <td className={x.ongoing ? "num mono bad-text" : "num mono"}>{x.ongoing ? "still losing" : when(x.last_loss_ms)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {p.impacted > p.list.length && <div className="muted">The {p.list.length} that lost the most are listed, of {p.impacted}.</div>}
    </>
  );
}

function PhaseResult({ phase }: { phase: Phase }) {
  const s = summarize(phase.flows);
  return (
    <div className="conv-phase">
      <h3>
        {phase.phase === "trigger" ? "Failure" : "Repair"}: <span className="muted">{phase.step}</span>
      </h3>
      <div className="stat-row">
        <div className={`stat ${s.notRestored ? "bad" : ""}`}>
          <div className="stat-n">{dur(s.worst)}</div>
          <div className="stat-l">Worst loss</div>
        </div>
        <div className="stat">
          <div className="stat-n">{dur(s.median)}</div>
          <div className="stat-l">Median</div>
        </div>
        <div className="stat">
          <div className="stat-n">{dur(s.p95)}</div>
          <div className="stat-l">95th percentile</div>
        </div>
        <div className="stat">
          <div className="stat-n">
            {s.impacted}/{s.flows}
          </div>
          <div className="stat-l">Flows lost traffic</div>
        </div>
        <div className={`stat ${s.notRestored ? "bad" : ""}`}>
          <div className="stat-n">{s.notRestored}</div>
          <div className="stat-l">Not restored</div>
        </div>
      </div>
      <ConvTimeline phase={phase} />
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>Flow</th>
              <th>Measured at</th>
              <th>Result</th>
              <th className="num" title="Lost packets × send interval">Loss duration</th>
              <th className="num">Lost</th>
              <th className="num" title="From the start of the step to the first lost packet">First loss</th>
              <th className="num" title="From the start of the step to the end of the last loss. Much later than the loss duration means partial loss, such as one ECMP path failing.">Last loss</th>
              <th className="num" title="The send interval: the smallest loss a flow can show">Resolution</th>
              {phase.flows.some((f) => f.prefixes) && <th className="num" title="A flow to many prefixes: how many lost traffic">Prefixes</th>}
            </tr>
          </thead>
          <tbody>
            {phase.flows.map((f) => (
              <tr key={f.agent + f.device + f.flow_id}>
                <td className="mono strong">{flowName(f)}</td>
                <td className="mono">
                  {f.agent} · {f.device}
                </td>
                <td>
                  <HealthText health={statusHealth(f.status)}>{statusWord[f.status] ?? f.status}</HealthText>
                </td>
                <td className={f.lost ? "num mono strong" : "num mono"}>
                  {f.truncated ? "≥ " : ""}
                  {unmeasured(f) ? "—" : dur(f.loss_ms)}
                </td>
                <td className="num mono">{unmeasured(f) ? "—" : f.lost.toLocaleString()}</td>
                <td className="num mono">{when(f.first_loss_ms)}</td>
                <td className="num mono">{when(f.last_loss_ms)}</td>
                <td className="num mono">{dur(f.resolution_ms)}</td>
                {phase.flows.some((x) => x.prefixes) && <td className="num mono">{f.prefixes ? `${f.prefixes.impacted}/${f.prefixes.total}` : "—"}</td>}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {phase.flows
        .filter((f) => f.prefixes && f.prefixes.impacted > 0)
        .map((f) => (
          <details className="conv-events" key={f.agent + f.device + f.flow_id}>
            <summary>
              {flowName(f)}: {f.prefixes!.impacted} of {f.prefixes!.total} prefixes lost traffic
            </summary>
            <PrefixTable p={f.prefixes!} span={(f.last_loss_ms ?? 0) - (f.first_loss_ms ?? 0)} />
          </details>
        ))}
      {phase.events.length > 0 && (
        <details className="conv-events">
          <summary>{phase.events.length} events during this step</summary>
          <table className="table dense">
            <tbody>
              {phase.events.slice(0, 100).map((e, i) => (
                <tr key={i}>
                  <td className="num mono">{when(e.at_ms)}</td>
                  <td className="mono">{e.agent}</td>
                  <td className="mono">{e.source}</td>
                  <td>{e.message}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
    </div>
  );
}

// ConvergencePanel measures the flows given, with every agent's senders and
// receivers. flowIds are the flows listed on the page (running or not).
export function ConvergencePanel({ agents, flowIds }: { agents: Agent[]; flowIds: string[] }) {
  const [trigger, setTrigger] = useState<Step>({ kind: "none" });
  const [restore, setRestore] = useState<Step>({ kind: "none" });
  const [settleS, setSettleS] = useState("5");
  const [maxS, setMaxS] = useState("60");
  const [running, setRunning] = useState(false);
  const [live, setLive] = useState("");
  const [error, setError] = useState<string>();
  const [problems, setProblems] = useState<string[]>([]);
  const [phases, setPhases] = useState<Phase[]>([]);
  const stop = useRef(false);
  const label = (id: string) => agents.find((a) => a.id === id)?.label ?? id;

  const run = async (force: boolean) => {
    setError(undefined);
    setProblems([]);
    if (!flowIds.length) return;
    const o: Options = { agents, flowIds, settleMs: Number(settleS) * 1000 || 5000, maxMs: Number(maxS) * 1000 || 60000 };
    stop.current = false;
    setRunning(true);
    setLive("Checking the flows are clean…");
    try {
      const first = await poll(agents, flowIds);
      if (first.flows.every((f) => f.status === "stopped")) throw new Error("No flows running: start them first.");
      const bad = precheckProblems(first.flows);
      if (bad.length && !force) {
        setProblems(bad);
        return;
      }
      setPhases([]);
      const done: Phase[] = [await measure("trigger", trigger, label, o, stop, setLive)];
      setPhases([...done]);
      if (restore.kind !== "none" && !stop.current) {
        done.push(await measure("restore", restore, label, o, stop, setLive));
        setPhases([...done]);
      }
    } catch (e) {
      setError(errorText(e));
    } finally {
      setRunning(false);
      setLive("");
    }
  };
  const stamp = () => new Date().toISOString().replace(/[:.]/g, "-");

  return (
    <div className="card conv">
      <div className="card-head">
        <h2>Convergence test</h2>
      </div>
      <p className="muted">
        What a failure costs the <b>{flowIds.length}</b> flow{flowIds.length === 1 ? "" : "s"} listed: <b>loss duration</b>, the lost packets × the send interval. Their statistics and checks are left alone. A fast send rate gives a
        finer measurement.
      </p>
      <StepEditor title="Failure" step={trigger} onChange={setTrigger} agents={agents} allowNone="I'll make it myself" />
      <StepEditor title="Repair" step={restore} onChange={setRestore} agents={agents} allowNone="Don't measure a repair" />
      <div className="conv-fields">
        <label className="field">
          <span className="field-label" title="Longer than the failure takes to reach traffic: a silent failure found by a 90 s hold timer needs more than 90 s">
            Clean for (s)
          </span>
          <input className="input" aria-label="Clean for" inputMode="decimal" value={settleS} onChange={(e) => setSettleS(e.target.value)} />
        </label>
        <label className="field">
          <span className="field-label">Give up after (s)</span>
          <input className="input" aria-label="Give up after" inputMode="decimal" value={maxS} onChange={(e) => setMaxS(e.target.value)} />
        </label>
      </div>
      <div className="btn-row">
        {running ? (
          <button className="btn" onClick={() => (stop.current = true)}>
            ■ Finish now
          </button>
        ) : (
          <button className="btn primary" disabled={!flowIds.length || !stepReady(trigger) || !stepReady(restore)} onClick={() => run(false)}>
            ▶ Run test
          </button>
        )}
        {phases.length > 0 && !running && (
          <>
            <button className="btn" onClick={() => download(`convergence-${stamp()}.json`, "application/json", JSON.stringify(phases, null, 2))}>
              Export JSON
            </button>
            <button className="btn" onClick={() => download(`convergence-${stamp()}.csv`, "text/csv", toCSV(phases))}>
              Export CSV
            </button>
          </>
        )}
        {live && <span className="muted conv-live">{live}</span>}
      </div>
      <ErrorBox error={error} />
      {problems.length > 0 && (
        <div className="conv-problems">
          <b>These flows aren't clean, so their loss could not be told from what the failure costs:</b>
          <ul>
            {problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
          <button className="btn" onClick={() => run(true)}>
            Run anyway
          </button>
        </div>
      )}
      {phases.map((p) => (
        <PhaseResult key={p.phase} phase={p} />
      ))}
    </div>
  );
}
