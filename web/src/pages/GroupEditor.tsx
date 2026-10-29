import { useEffect, useMemo, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { parse as parseYAML, stringify as toYAML } from "yaml";
import { client, errorText, usePoll } from "../api";
import { navigate } from "../router";
import type { DeviceGroup } from "../gen/clabgen/v1/agent_pb";
import { SchemaForm, defaults, normalize, parseSchema, type Obj } from "../components/SchemaForm";
import { StepInput } from "../components/Stepped";
import { Modal, useToast } from "../ui";

// GroupEditor creates or edits a device group as a form: the device
// template, each group protocol's config (any number, address or MAC can
// step per member) and a live preview of members rendered by the agent.
// The whole spec can also be edited as YAML.

type Spec = { name?: string; count?: number; device?: Obj; protocols?: Proto[] } & Obj;
type Proto = { name?: string; type: string; config?: Obj; disabled?: boolean };

type PreviewMember = {
  index: number;
  device: { name: string; port: string; vlans?: { vid: number; tpid?: number }[]; addresses?: string[]; gateway4?: string; gateway6?: string };
  mac: string;
  protocols: { name: string; type: string; config: Obj }[];
};
type Plan = { unchanged: number; changed: number; added: number; removed: number };
type Preview = { count: number; members: PreviewMember[]; plan?: Plan };

const blank = (port: string): Spec => ({ name: "", count: 10, device: { port }, protocols: [] });

const placeholders = "{i} (1-based), {i0}, {group}; in protocol configs also the member's {device}, {ipv4}, {ipv6}, {gateway4}, {gateway6}, {mac}, {vlan}, {inner_vlan}";

export function GroupEditor({ agentId, group, onClose, onDone }: { agentId: string; group?: DeviceGroup; onClose: () => void; onDone: () => void }) {
  const c = client(agentId);
  const ports = usePoll(() => c.listPorts({}), [agentId]);
  const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
  const portNames = (ports.data?.ports ?? []).map((p) => p.name);
  const [spec, setSpec] = useState<Spec>(() => (group?.spec as Spec | undefined) ?? blank(""));
  // A new group starts on the first port, once the ports are known.
  useEffect(() => {
    if (!group && !spec.device?.port && portNames.length) setSpec((s) => ({ ...s, device: { ...s.device, port: portNames[0] } }));
  }, [portNames.join()]); // eslint-disable-line react-hooks/exhaustive-deps
  const [sel, setSel] = useState<string>("device");
  const [yaml, setYaml] = useState<string>();
  const [yamlErr, setYamlErr] = useState<string>();
  const [preview, setPreview] = useState<Preview>();
  const [previewErr, setPreviewErr] = useState<string>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const toast = useToast();

  // The agent renders a few members after each pause in editing.
  const text = JSON.stringify(spec);
  useEffect(() => {
    if (!spec.name || !spec.count) {
      setPreview(undefined);
      setPreviewErr(!spec.name ? "Name the group to see its members." : "Set how many members.");
      return;
    }
    let live = true;
    const t = setTimeout(() => {
      c.previewDeviceGroup({ group: { name: spec.name, spec: spec as JsonObject } })
        .then((r) => {
          if (!live) return;
          setPreview(r.result as unknown as Preview);
          setPreviewErr(undefined);
        })
        .catch((e) => {
          if (!live) return;
          const t = errorText(e);
          // Agents before the group editor don't have the call.
          setPreviewErr(/404|unimplemented/i.test(t) ? "This agent can't preview groups (it needs a newer clabgen); creating and editing still work." : t);
        });
    }, 350);
    return () => {
      live = false;
      clearTimeout(t);
    };
  }, [agentId, text]); // eslint-disable-line react-hooks/exhaustive-deps

  // clean drops protocol values equal to their defaults, so a spec built
  // here is as short as one written by hand.
  const clean = (s: Spec): Spec => ({
    ...s,
    protocols: s.protocols?.map((p) => {
      const t = types.data?.types.find((x) => x.name === p.type);
      return t && p.config ? { ...p, config: normalize(parseSchema(t.configSchema), p.config) } : p;
    }),
  });

  const protos = spec.protocols ?? [];
  const protoKey = (p: Proto, i: number) => `p${i}:${p.name || p.type}`;
  const setProto = (i: number, p: Proto | undefined) => {
    const next = [...protos];
    if (p) next[i] = p;
    else next.splice(i, 1);
    setSpec({ ...spec, protocols: next });
  };
  const addProto = (type: string) => {
    const t = types.data?.types.find((x) => x.name === type);
    const p: Proto = { type, config: t ? defaults(parseSchema(t.configSchema)) : {} };
    // A second instance of a type needs its own name.
    if (protos.some((x) => (x.name || x.type) === type)) p.name = `${type}-${protos.length + 1}`;
    setSpec({ ...spec, protocols: [...protos, p] });
    setSel(protoKey(p, protos.length));
  };

  // A rejected spec stays in the editor with the agent's error.
  const save = async () => {
    setError(undefined);
    setBusy(true);
    const req = { group: { name: spec.name ?? "", spec: clean(spec) as JsonObject } };
    try {
      const r = group ? (await c.updateDeviceGroup(req)).group : (await c.createDeviceGroup(req)).group;
      toast("ok", group ? `Updated ${group.name}` : "Group created");
      onDone();
      onClose();
      if (!group && r) navigate({ page: "group", agent: agentId, group: r.name });
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  const selIdx = protos.findIndex((p, i) => protoKey(p, i) === sel);
  const plan = group ? preview?.plan : undefined;

  return (
    <Modal title={group ? `Edit group ${group.name}` : "New device group"} onClose={onClose} wide="x">
      {error && (
        <div className="error-box" role="alert">
          The agent rejected the group: {error}
        </div>
      )}
      <div className="group-head">
        <label className="field">
          <span className="field-label">name</span>
          <input
            className="input mono"
            aria-label="Group name"
            value={spec.name ?? ""}
            disabled={!!group}
            placeholder="e.g. rmep"
            onChange={(e) => setSpec({ ...spec, name: e.target.value })}
          />
        </label>
        <label className="field">
          <span className="field-label">members</span>
          <input
            className="input mono"
            aria-label="Member count"
            type="number"
            min={1}
            max={4096}
            value={spec.count ?? ""}
            onChange={(e) => setSpec({ ...spec, count: e.target.value === "" ? undefined : Number(e.target.value) })}
          />
        </label>
        <button
          type="button"
          className="link group-yaml-toggle"
          onClick={() => {
            if (yaml === undefined) {
              setYaml(toYAML(clean(spec)));
              setYamlErr(undefined);
            } else if (!yamlErr) setYaml(undefined);
          }}
        >
          {yaml === undefined ? "edit as YAML" : "form view"}
        </button>
      </div>

      {yaml !== undefined ? (
        <>
          <textarea
            className="input mono code scenario"
            aria-label="Group as YAML"
            spellCheck={false}
            value={yaml}
            onChange={(e) => {
              setYaml(e.target.value);
              try {
                const v = parseYAML(e.target.value) ?? {};
                if (typeof v !== "object" || Array.isArray(v)) throw new Error("must be a mapping of fields");
                setYamlErr(undefined);
                setSpec(v as Spec);
              } catch (x) {
                setYamlErr((x as Error).message);
              }
            }}
          />
          {yamlErr && <div className="hint error-text">{yamlErr}</div>}
        </>
      ) : (
        <div className="sectioned group-sections">
          <nav className="section-nav" aria-label="Group parts">
            <button type="button" className={sel === "device" ? "active" : ""} onClick={() => setSel("device")}>
              <span>device</span>
            </button>
            {protos.map((p, i) => (
              <button type="button" key={i} className={sel === protoKey(p, i) ? "active" : ""} onClick={() => setSel(protoKey(p, i))}>
                <span className="mono">{p.name || p.type}</span>
              </button>
            ))}
            <select
              className="input add-proto"
              aria-label="Add a protocol"
              value=""
              onChange={(e) => e.target.value && addProto(e.target.value)}
            >
              <option value="">+ protocol…</option>
              {(types.data?.types ?? []).map((t) => (
                <option key={t.name} value={t.name}>
                  {t.name}
                </option>
              ))}
            </select>
          </nav>
          <div className="section-pane">
            <p className="muted small-note">
              <b>±</b> steps a value per member: start, plus a step (1, or an address/MAC form like <code>0.0.0.4</code>) for each member. Text takes{" "}
              <code>{placeholders}</code>.
            </p>
            {selIdx < 0 ? (
              <DeviceTemplate value={spec.device ?? {}} ports={portNames} onChange={(d) => setSpec({ ...spec, device: d })} />
            ) : (
              <ProtoTemplate
                key={sel}
                proto={protos[selIdx]}
                schemaText={types.data?.types.find((t) => t.name === protos[selIdx].type)?.configSchema}
                onChange={(p) => setProto(selIdx, p)}
                onRemove={() => {
                  setProto(selIdx, undefined);
                  setSel("device");
                }}
              />
            )}
          </div>
        </div>
      )}

      <MemberPreview preview={preview} error={previewErr} protos={protos} />

      <div className="modal-foot">
        {plan && (
          <span className={`muted small group-plan${plan.changed + plan.removed ? " warn-text" : ""}`}>
            Applying: {planText(plan)}
          </span>
        )}
        <button type="button" className="btn" onClick={onClose}>
          Cancel
        </button>
        <button type="button" className="btn primary" disabled={busy || !!yamlErr || !spec.name || !!(plan && !plan.changed && !plan.added && !plan.removed)} onClick={save}>
          {group ? "Apply" : "Create"}
        </button>
      </div>
    </Modal>
  );
}

export function planText(p: Plan): string {
  const parts = [
    p.changed && `${p.changed} changed`,
    p.added && `${p.added} added`,
    p.removed && `${p.removed} removed`,
    p.unchanged && `${p.unchanged} unchanged (keep running)`,
  ].filter(Boolean);
  return !p.changed && !p.added && !p.removed ? "nothing changes" : parts.join(", ");
}

const tpids = [
  { v: undefined, label: "802.1Q" },
  { v: 0x88a8, label: "802.1ad (0x88a8)" },
  { v: 0x9100, label: "0x9100" },
];

// DeviceTemplate edits the member device: port, VLAN tags, addresses,
// gateways and MAC, each value steppable.
function DeviceTemplate({ value, ports, onChange }: { value: Obj; ports: string[]; onChange: (v: Obj) => void }) {
  const set = (k: string, v: unknown) => {
    const next = { ...value };
    if (v === undefined || v === "" || (Array.isArray(v) && !v.length)) delete next[k];
    else next[k] = v;
    onChange(next);
  };
  const vlans = (value.vlans as Obj[] | undefined) ?? [];
  const addrs = (value.addresses as unknown[] | undefined) ?? [];
  const textStep = (k: string, label: string, ph: string) => (
    <div className="field">
      <span className="field-label">{label}</span>
      <StepInput value={value[k]} numeric={false} label={label} onChange={(v) => set(k, v)}>
        <input className="input mono" aria-label={label} placeholder={ph} value={(value[k] as string) ?? ""} onChange={(e) => set(k, e.target.value)} />
      </StepInput>
    </div>
  );
  return (
    <div className="fieldset">
      <div className="form-grid">
        <div className="field">
          <span className="field-label">
            port<span className="req">*</span>
          </span>
          <select className="input" aria-label="port" value={(value.port as string) ?? ""} onChange={(e) => set("port", e.target.value)}>
            {!ports.includes(value.port as string) && <option value={(value.port as string) ?? ""}>{(value.port as string) || "—"}</option>}
            {ports.map((p) => (
              <option key={p}>{p}</option>
            ))}
          </select>
        </div>
        <div className="field">
          <span className="field-label">name</span>
          <input className="input mono" aria-label="device name" placeholder="{group}-{i}" value={(value.name as string) ?? ""} onChange={(e) => set("name", e.target.value)} />
        </div>
        {textStep("mac", "MAC", "derived from the name")}
      </div>

      <div className="field-block">
        <span className="field-label">VLAN tags (outer first)</span>
        {vlans.map((t, i) => (
          <div className="form-grid tag-row" key={i}>
            <div className="field">
              <span className="field-label">VLAN ID</span>
              <StepInput
                value={t.vid}
                numeric
                label={`VLAN ${i + 1}`}
                onChange={(v) => set("vlans", vlans.map((x, j) => (j === i ? { ...x, vid: v } : x)))}
              >
                <input
                  className="input mono"
                  type="number"
                  min={0}
                  max={4095}
                  aria-label={`VLAN ${i + 1}`}
                  value={(t.vid as number) ?? ""}
                  onChange={(e) => set("vlans", vlans.map((x, j) => (j === i ? { ...x, vid: e.target.value === "" ? undefined : Number(e.target.value) } : x)))}
                />
              </StepInput>
            </div>
            <div className="field">
              <span className="field-label">type</span>
              <select
                className="input"
                aria-label={`VLAN ${i + 1} type`}
                value={String(t.tpid ?? "")}
                onChange={(e) =>
                  set(
                    "vlans",
                    vlans.map((x, j) => {
                      if (j !== i) return x;
                      const { tpid: _, ...rest } = x;
                      return e.target.value ? { ...rest, tpid: Number(e.target.value) } : rest;
                    }),
                  )
                }
              >
                {tpids.map((o) => (
                  <option key={o.label} value={o.v ?? ""}>
                    {o.label}
                  </option>
                ))}
              </select>
            </div>
            <button type="button" className="icon-btn tag-remove" aria-label={`Remove VLAN ${i + 1}`} onClick={() => set("vlans", vlans.filter((_, j) => j !== i))}>
              ×
            </button>
          </div>
        ))}
        {vlans.length < 4 && (
          <button type="button" className="btn small" onClick={() => set("vlans", [...vlans, { vid: vlans.length ? 1 : 100 }])}>
            Add VLAN tag
          </button>
        )}
      </div>

      <div className="field-block">
        <span className="field-label">addresses</span>
        {addrs.map((a, i) => (
          <div className="addr-row" key={i}>
            <StepInput value={a} numeric={false} label={`address ${i + 1}`} onChange={(v) => set("addresses", addrs.map((x, j) => (j === i ? v : x)))}>
              <input
                className="input mono"
                aria-label={`address ${i + 1}`}
                placeholder="10.1.0.2/30 or 2001:db8::2/64"
                value={(a as string) ?? ""}
                onChange={(e) => set("addresses", addrs.map((x, j) => (j === i ? e.target.value : x)))}
              />
            </StepInput>
            <button type="button" className="icon-btn" aria-label={`Remove address ${i + 1}`} onClick={() => set("addresses", addrs.filter((_, j) => j !== i))}>
              ×
            </button>
          </div>
        ))}
        <button type="button" className="btn small" onClick={() => set("addresses", [...addrs, ""])}>
          Add address
        </button>
      </div>

      <div className="form-grid">
        {textStep("gateway4", "IPv4 gateway", "10.1.0.1")}
        {textStep("gateway6", "IPv6 gateway", "2001:db8::1")}
      </div>
    </div>
  );
}

// ProtoTemplate edits one group protocol: its name, type and config.
function ProtoTemplate({ proto, schemaText, onChange, onRemove }: { proto: Proto; schemaText?: string; onChange: (p: Proto) => void; onRemove: () => void }) {
  const schema = useMemo(() => parseSchema(schemaText), [schemaText]);
  return (
    <div className="fieldset">
      <div className="form-grid">
        <div className="field">
          <span className="field-label">type</span>
          <input className="input mono" value={proto.type} disabled aria-label="protocol type" />
        </div>
        <div className="field">
          <span className="field-label">name in the group</span>
          <input
            className="input mono"
            aria-label="protocol name"
            placeholder={proto.type}
            value={proto.name ?? ""}
            onChange={(e) => {
              const { name: _, ...rest } = proto;
              onChange(e.target.value ? { ...rest, name: e.target.value } : rest);
            }}
          />
        </div>
        <div className="field proto-remove">
          <button type="button" className="btn small danger-ghost" onClick={onRemove}>
            Remove from group
          </button>
        </div>
      </div>
      <SchemaForm schema={schema} value={proto.config ?? {}} mode="config" stepping onChange={(cfg) => onChange({ ...proto, config: cfg })} />
    </div>
  );
}

// flat lists a config's scalar values by path ("peers[0].address").
function flat(v: unknown, at = "", out: Record<string, string> = {}): Record<string, string> {
  if (Array.isArray(v)) v.forEach((x, i) => flat(x, `${at}[${i}]`, out));
  else if (v && typeof v === "object") for (const [k, x] of Object.entries(v)) flat(x, at ? `${at}.${k}` : k, out);
  else if (v !== undefined) out[at] = String(v);
  return out;
}

// MemberPreview shows the members the agent rendered: the device, and the
// protocol values that differ between them (what the template steps).
function MemberPreview({ preview, error, protos }: { preview?: Preview; error?: string; protos: Proto[] }) {
  const varying = useMemo(() => {
    const ms = preview?.members ?? [];
    const out: Record<string, string[]> = {};
    for (const p of ms[0]?.protocols ?? []) {
      const short = p.name.slice(ms[0].device.name.length + 1);
      const vals = ms.map((m) => flat(m.protocols.find((x) => x.name.endsWith("-" + short))?.config));
      out[short] = Object.keys(vals[0] ?? {}).filter((k) => vals.some((v) => v[k] !== vals[0][k]));
    }
    return out;
  }, [preview]);
  const stepped = protos.some((p) => JSON.stringify(p.config ?? {}).includes('"start"'));
  return (
    <div className="group-preview">
      <div className="field-label">
        preview{preview && <span className="muted"> · {preview.count} members, showing {preview.members.map((m) => m.index).join(", ")}</span>}
      </div>
      {error ? (
        <div className={error.startsWith("Name the") || error.startsWith("Set how") || error.startsWith("This agent can") ? "muted small-note" : "error-text small"} role="status">
          {error}
        </div>
      ) : !preview ? (
        <div className="muted small-note">Rendering…</div>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>#</th>
                <th>device</th>
                <th>VLANs</th>
                <th>addresses</th>
                <th>MAC</th>
                {Object.keys(varying).map((p) => (
                  <th key={p} className="mono">
                    {p}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {preview.members.map((m) => (
                <tr key={m.index}>
                  <td>{m.index}</td>
                  <td className="mono">{m.device.name}</td>
                  <td className="mono">{(m.device.vlans ?? []).map((v) => v.vid).join(".") || "—"}</td>
                  <td className="mono">
                    {[...(m.device.addresses ?? []), m.device.gateway4 && `gw ${m.device.gateway4}`, m.device.gateway6 && `gw ${m.device.gateway6}`].filter(Boolean).join(", ") || "—"}
                  </td>
                  <td className="mono">{m.mac}</td>
                  {Object.entries(varying).map(([p, keys]) => {
                    const cfg = flat(m.protocols.find((x) => x.name.endsWith("-" + p))?.config);
                    return (
                      <td key={p} className="mono">
                        {keys.length ? keys.map((k) => `${k} ${cfg[k]}`).join(", ") : stepped ? "—" : "same on every member"}
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
