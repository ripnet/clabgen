import { useMemo, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, errorText, usePoll } from "../api";
import { href, navigate } from "../router";
import type { Health, Summary } from "../protocols/registry";
import type { DeviceGroup, GroupActionResult } from "../gen/clabgen/v1/agent_pb";
import { SchemaForm, defaults, parseSchema, type Obj } from "../components/SchemaForm";
import { ValueView } from "../components/ValueView";
import { Empty, ErrorBox, HealthText, Modal, useBusy } from "../ui";
import { AgentHeader } from "./AgentPage";
import { EnableToggle } from "./ProtocolsTab";

// throwFailed turns per-member failures of a group change into an error.
function throwFailed(r: { results: GroupActionResult[] }) {
  const failed = r.results.filter((x) => x.error);
  if (failed.length)
    throw new Error(`${failed.length} of ${r.results.length} failed: ${failed.slice(0, 3).map((f) => `${f.protocol}: ${f.error}`).join("; ")}`);
  return r;
}

// Group-wide pseudo-actions, next to the protocol type's own.
const toggleActions = [
  { name: "disable", description: "Stop the protocol on the selected members, keeping their config. On every member it is kept in the group spec; on some, until they are enabled again.", argsSchema: "" },
  { name: "enable", description: "Start the protocol again on the selected members.", argsSchema: "" },
];

type Counts = Record<string, number>;
type ProtoSummary = { name: string; type: string; states: Counts; health?: Counts; disabled?: boolean };
type MemberProto = { name: string; state: string; health?: string; error?: string };
type Member = { index: number; device: string; mac: string; vlans: string; addresses?: string[]; protocols?: MemberProto[] };

const goodWords = /^(established|up|ok|running|passive|\d+ neighbors?)$/;
const idleWords = /^(stopped|idle|no peers)$/;

export function wordHealth(state: string, health?: string): Health {
  if (state === "error") return "bad";
  if (state !== "running") return "idle";
  if (!health || goodWords.test(health)) return "ok";
  return idleWords.test(health) ? "idle" : "warn";
}

export function countsText(c: Counts | undefined): string {
  return Object.entries(c ?? {})
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([k, n]) => `${n} ${k}`)
    .join(", ");
}

const sum = (c: Counts | undefined) => Object.values(c ?? {}).reduce((a, b) => a + b, 0);

// groupHealth rolls a group protocol's counts into one health: ok when
// every member is running and healthy, bad when none is. Disabled members
// don't count against it.
export function groupHealth(p: ProtoSummary): Summary {
  const total = sum(p.states) - (p.states.disabled ?? 0);
  const running = p.states.running ?? 0;
  if (total === 0) return { health: "idle", text: countsText(p.states) };
  if (p.states.error) return { health: "bad", text: `${p.states.error} failed: ${countsText(p.states)}` };
  if (!running) return { health: "idle", text: countsText(p.states) };
  if (!p.health) return { health: running === total ? "ok" : "warn", text: countsText(p.states) };
  const entries = Object.entries(p.health);
  const good = entries.filter(([k]) => goodWords.test(k)).reduce((a, [, n]) => a + n, 0);
  const idle = entries.filter(([k]) => idleWords.test(k)).reduce((a, [, n]) => a + n, 0);
  let text = countsText(p.health);
  if (running < sum(p.states)) text += ` (${countsText(p.states)})`;
  if (good === total) return { health: "ok", text };
  if (idle === total) return { health: "idle", text };
  return { health: good > 0 ? "warn" : "bad", text };
}

export function groupStatus(g: DeviceGroup): { devices: number; count: number; protocols: ProtoSummary[]; members: Member[] } {
  const st = (g.status ?? {}) as unknown as { devices?: number; protocols?: ProtoSummary[]; members?: Member[] };
  const count = Number((g.spec as JsonObject | undefined)?.count ?? 0);
  return { devices: st.devices ?? 0, count, protocols: st.protocols ?? [], members: st.members ?? [] };
}

export function GroupsTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const list = usePoll(() => c.listDeviceGroups({}), [agentId], 2000);
  const [editing, setEditing] = useState<DeviceGroup | "new">();
  const { busy, run } = useBusy();
  const groups = list.data?.groups ?? [];

  return (
    <div className="card">
      <div className="card-head">
        <h2>Device groups</h2>
        <button className="btn primary" onClick={() => setEditing("new")}>
          New group
        </button>
      </div>
      <p className="muted">
        A group is many devices from one template (VLANs, MACs and addresses stepped per member), each running the same protocols. Members
        are shown here rather than in the Devices and Protocols tabs.
      </p>
      <ErrorBox error={list.error} />
      {!list.loading && groups.length === 0 ? (
        <Empty title="No device groups">Create one, or add device_groups to a scenario.</Empty>
      ) : (
        <div className="table-wrap">
          <table className="table hover">
            <thead>
              <tr>
                <th>Name</th>
                <th>Devices</th>
                <th>Protocols</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {groups.map((g) => {
                const s = groupStatus(g);
                return (
                  <tr key={g.name} onClick={() => navigate({ page: "group", agent: agentId, group: g.name })}>
                    <td className="mono strong">
                      <a href={href({ page: "group", agent: agentId, group: g.name })} onClick={(e) => e.stopPropagation()}>
                        {g.name}
                      </a>
                    </td>
                    <td className="mono">
                      {s.devices}/{s.count}
                    </td>
                    <td>
                      {s.protocols.map((p) => {
                        const h = groupHealth(p);
                        return (
                          <div key={p.name}>
                            <HealthText health={h.health}>
                              <span className="mono">{p.name}</span>: {h.text}
                            </HealthText>
                          </div>
                        );
                      })}
                      {s.protocols.length === 0 && <span className="muted">—</span>}
                    </td>
                    <td className="actions">
                      <button
                        className="btn small"
                        disabled={busy}
                        onClick={(e) => {
                          e.stopPropagation();
                          setEditing(g);
                        }}
                      >
                        Edit
                      </button>
                      <button
                        className="btn small danger-ghost"
                        disabled={busy}
                        onClick={(e) => {
                          e.stopPropagation();
                          if (confirm(`Delete group ${g.name} and its ${s.devices} devices?`))
                            run(() => c.deleteDeviceGroup({ name: g.name }), `Deleted ${g.name}`).then(list.refresh);
                        }}
                      >
                        Delete
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {editing && <GroupForm agentId={agentId} group={editing === "new" ? undefined : editing} onClose={() => setEditing(undefined)} onDone={list.refresh} />}
    </div>
  );
}

const example = `name: ce
count: 10
device:
  port: eth1
  vlans: [{vid: {start: 1000}}]
  addresses: [{start: 10.1.0.2/30, step: 0.0.0.4}]
  gateway4: {start: 10.1.0.1, step: 0.0.0.4}
protocols:
  - type: bgp
    config:
      local_as: {start: 65001}
      peers: [{address: "{gateway4}", remote_as: 65000}]
      routes: [{prefix: {start: 100.64.0.0/24, step: 0.0.1.0}, count: 10}]
`;

function GroupForm({ agentId, group, onClose, onDone }: { agentId: string; group?: DeviceGroup; onClose: () => void; onDone: () => void }) {
  const c = client(agentId);
  const [text, setText] = useState(group?.yaml ?? example);
  const { busy, run } = useBusy();
  return (
    <Modal title={group ? `Edit group ${group.name}` : "New device group"} onClose={onClose} wide>
      <p className="muted small">
        Any value can be stepped per member: <code>{"{start, step, repeat, cycle}"}</code> with an integer, IP address, prefix or MAC. Strings take{" "}
        <code>{"{i}"}</code> (1-based), <code>{"{i0}"}</code> and <code>{"{group}"}</code>; protocol configs also take the member's{" "}
        <code>{"{device} {ipv4} {ipv6} {gateway4} {gateway6} {mac} {vlan}"}</code>. Devices are named <code>{"{group}-{i}"}</code> unless the
        device sets a name; protocols <code>{"<device>-<name>"}</code>.
        {group && " Members whose rendered config is unchanged keep running."}
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const req = { group: { name: group?.name ?? "", yaml: text } };
          run(async () => (group ? (await c.updateDeviceGroup(req)).group : (await c.createDeviceGroup(req)).group), group ? `Updated ${group.name}` : "Group created").then((r) => {
            if (!r) return;
            onDone();
            onClose();
            if (!group) navigate({ page: "group", agent: agentId, group: r.name });
          });
        }}
      >
        <textarea className="input mono code scenario" spellCheck={false} value={text} onChange={(e) => setText(e.target.value)} />
        <div className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={busy || !text.trim()}>
            {group ? "Apply" : "Create"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

const pageSize = 100;

export function GroupDetail({ agentId, name }: { agentId: string; name: string }) {
  const c = client(agentId);
  const g = usePoll(() => c.getDeviceGroup({ name }), [agentId, name], 2000);
  const [editing, setEditing] = useState(false);
  const [filter, setFilter] = useState("");
  const [page, setPage] = useState(0);
  const { busy, run } = useBusy();
  const group = g.data?.group;
  const s = group ? groupStatus(group) : undefined;

  const members = useMemo(() => {
    const f = filter.trim().toLowerCase();
    if (!s || !f) return s?.members ?? [];
    return s.members.filter(
      (m) =>
        m.device.toLowerCase().includes(f) ||
        m.vlans.includes(f) ||
        (m.addresses ?? []).some((a) => a.includes(f)) ||
        (m.protocols ?? []).some((p) => (p.health || p.state).toLowerCase().includes(f)),
    );
  }, [s, filter]);
  const pages = Math.max(1, Math.ceil(members.length / pageSize));
  const shown = members.slice(page * pageSize, (page + 1) * pageSize);

  return (
    <div className="page">
      <AgentHeader agentId={agentId}>
        <div className="crumbs">
          <a href={href({ page: "agent", agent: agentId, tab: "groups" })}>groups</a> / <span className="mono">{name}</span>
        </div>
      </AgentHeader>
      {g.error && !group && <ErrorBox error={g.error} />}
      {group && s && (
        <>
          <div className="proto-head">
            <div>
              <h2 className="mono">{group.name}</h2>
              <div className="muted">
                {s.devices}/{s.count} devices
              </div>
            </div>
            <div className="proto-state">
              {s.protocols.map((p) => {
                const h = groupHealth(p);
                return (
                  <div key={p.name} className="group-proto">
                    <EnableToggle
                      disabled={!!p.disabled}
                      busy={busy}
                      title={p.disabled ? `Enable ${p.name} on every member` : `Disable ${p.name} on every member, keeping its config`}
                      onToggle={() => {
                        if (!p.disabled && !confirm(`Disable ${p.name} on all ${s.count} members of ${group.name}?`)) return;
                        run(() => c.setGroupDisabled({ group: group.name, protocol: p.name, disabled: !p.disabled }).then(throwFailed), `${p.disabled ? "Enabled" : "Disabled"} ${p.name} on ${group.name}`).then(g.refresh);
                      }}
                    />
                    <HealthText health={h.health}>
                      <span className="mono">{p.name}</span> ({p.type}): {h.text}
                    </HealthText>
                  </div>
                );
              })}
            </div>
            <div className="btn-row">
              <button className="btn" onClick={() => setEditing(true)}>
                Edit
              </button>
              <button
                className="btn danger-ghost"
                disabled={busy}
                onClick={() => {
                  if (confirm(`Delete group ${group.name} and its ${s.devices} devices?`))
                    run(() => c.deleteDeviceGroup({ name: group.name }), `Deleted ${group.name}`).then((r) => r && navigate({ page: "agent", agent: agentId, tab: "groups" }));
                }}
              >
                Delete
              </button>
            </div>
          </div>

          {s.protocols.length > 0 && <GroupActions agentId={agentId} group={group.name} protocols={s.protocols} count={s.count} />}

          <div className="card">
            <div className="card-head">
              <h2>Members</h2>
              <div className="btn-row">
                <input
                  className="input"
                  style={{ width: 260 }}
                  placeholder="Filter: name, VLAN, address, state"
                  value={filter}
                  onChange={(e) => {
                    setFilter(e.target.value);
                    setPage(0);
                  }}
                />
                {pages > 1 && (
                  <>
                    <button className="btn small" disabled={page === 0} onClick={() => setPage(page - 1)}>
                      ‹
                    </button>
                    <span className="muted small">
                      {page * pageSize + 1}–{Math.min(members.length, (page + 1) * pageSize)} of {members.length}
                    </span>
                    <button className="btn small" disabled={page >= pages - 1} onClick={() => setPage(page + 1)}>
                      ›
                    </button>
                  </>
                )}
              </div>
            </div>
            <div className="table-wrap">
              <table className="table dense">
                <thead>
                  <tr>
                    <th>#</th>
                    <th>Device</th>
                    <th>VLANs</th>
                    <th>MAC</th>
                    <th>Addresses</th>
                    {s.protocols.map((p) => (
                      <th key={p.name}>{p.name}</th>
                    ))}
                  </tr>
                </thead>
                <tbody>
                  {shown.map((m) => (
                    <tr key={m.device}>
                      <td className="mono muted">{m.index}</td>
                      <td className="mono strong">{m.device}</td>
                      <td className="mono">{m.vlans}</td>
                      <td className="mono">{m.mac || "—"}</td>
                      <td className="mono">{(m.addresses ?? []).join(", ") || "—"}</td>
                      {(m.protocols ?? []).map((p) => (
                        <td key={p.name}>
                          <a href={href({ page: "protocol", agent: agentId, protocol: p.name })} title={p.error || p.name}>
                            <HealthText health={wordHealth(p.state, p.health)}>{p.state === "running" ? p.health || "running" : p.state}</HealthText>
                          </a>
                        </td>
                      ))}
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {members.length === 0 && <div className="muted">No member matches.</div>}
          </div>
          {editing && <GroupForm agentId={agentId} group={group} onClose={() => setEditing(false)} onDone={g.refresh} />}
        </>
      )}
    </div>
  );
}

type GroupResult = { id: number; action: string; at: Date; ms: number; results?: GroupActionResult[]; error?: string };

function GroupActions({ agentId, group, protocols, count }: { agentId: string; group: string; protocols: ProtoSummary[]; count: number }) {
  const types = usePoll(() => client(agentId).listProtocolTypes({}), [agentId]);
  const [protoName, setProtoName] = useState(protocols[0].name);
  const proto = protocols.find((p) => p.name === protoName) ?? protocols[0];
  const typeActions = types.data?.types.find((t) => t.name === proto.type)?.actions;
  const actions = typeActions ? [...typeActions, ...toggleActions] : [];
  const [sel, setSel] = useState("");
  const action = actions.find((a) => a.name === sel) ?? actions[0];
  const schema = useMemo(() => parseSchema(action?.argsSchema), [action?.argsSchema]);
  const [argsBy, setArgsBy] = useState<Record<string, Obj>>({});
  const key = proto.type + "/" + (action?.name ?? "");
  const args = argsBy[key] ?? defaults(schema);
  const [members, setMembers] = useState("");
  const [random, setRandom] = useState("");
  const [running, setRunning] = useState(false);
  const [results, setResults] = useState<GroupResult[]>([]);

  if (!action) return null;
  const runAction = async () => {
    setRunning(true);
    const t0 = performance.now();
    const base = { id: Date.now(), action: `${proto.name} ${action.name}`, at: new Date() };
    try {
      const sel = { group, protocol: proto.name, members, random: Number(random) || 0 };
      const toggle = toggleActions.some((a) => a.name === action.name);
      const r = toggle
        ? await client(agentId).setGroupDisabled({ ...sel, disabled: action.name === "disable" })
        : await client(agentId).runGroupAction({ ...sel, action: action.name, args: args as JsonObject });
      setResults((x) => [{ ...base, ms: performance.now() - t0, results: r.results }, ...x].slice(0, 5));
    } catch (e) {
      setResults((x) => [{ ...base, ms: performance.now() - t0, error: errorText(e) }, ...x].slice(0, 5));
    } finally {
      setRunning(false);
    }
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Actions</h2>
        {protocols.length > 1 && (
          <div className="seg">
            {protocols.map((p) => (
              <button key={p.name} className={p.name === proto.name ? "seg-btn active" : "seg-btn"} onClick={() => setProtoName(p.name)}>
                {p.name}
              </button>
            ))}
          </div>
        )}
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
        <div className="form-grid">
          <div className="field">
            <label className="field-label">members</label>
            <input className="input mono" placeholder={`all (1-${count})`} value={members} onChange={(e) => setMembers(e.target.value)} />
            <div className="hint">1-based indices and ranges, e.g. 1-10,15</div>
          </div>
          <div className="field">
            <label className="field-label">random</label>
            <input className="input mono" type="number" min={0} placeholder="0" value={random} onChange={(e) => setRandom(e.target.value)} />
            <div className="hint">Run on this many of the selected members, picked at random</div>
          </div>
        </div>
        {action.argsSchema && (
          <>
            <h4>Arguments</h4>
            <SchemaForm schema={schema} value={args} onChange={(v) => setArgsBy((m) => ({ ...m, [key]: v }))} mode="args" />
          </>
        )}
        <div className="btn-row end">
          <button className="btn primary" disabled={running}>
            {running ? "Running…" : action.name === "disable" ? "Disable" : action.name === "enable" ? "Enable" : `Run ${action.name}`}
          </button>
        </div>
      </form>
      {results.map((r, i) => {
        const failed = (r.results ?? []).filter((x) => x.error);
        return (
          <div key={r.id} className={`result ${i > 0 ? "old" : ""}`}>
            <div className="result-head">
              <span className="mono strong">{r.action}</span>
              <span className="muted">
                {r.at.toLocaleTimeString()} · {(r.ms / 1000).toFixed(2)} s
              </span>
            </div>
            {r.error ? (
              <div className="error-box">{r.error}</div>
            ) : (
              <>
                <HealthText health={failed.length ? (failed.length === r.results?.length ? "bad" : "warn") : "ok"}>
                  {r.results?.length} members: {(r.results?.length ?? 0) - failed.length} ok, {failed.length} failed
                </HealthText>
                {failed.slice(0, 20).map((f) => (
                  <div key={f.index} className="mono small error-text">
                    {f.protocol}: {f.error}
                  </div>
                ))}
                {i === 0 && (r.results?.length ?? 0) > 0 && (
                  <details>
                    <summary className="muted small">Results per member</summary>
                    <ValueView value={(r.results ?? []).map((x) => ({ member: x.index, protocol: x.protocol, error: x.error, ...(x.result ?? {}) })) as never} />
                  </details>
                )}
              </>
            )}
          </div>
        );
      })}
    </div>
  );
}
