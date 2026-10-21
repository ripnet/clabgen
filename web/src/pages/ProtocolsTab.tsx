import { useMemo, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, usePoll } from "../api";
import { href, navigate } from "../router";
import { summarize } from "../protocols/registry";
import { SchemaForm, defaults, parseSchema, type Obj } from "../components/SchemaForm";
import { Empty, ErrorBox, HealthText, Modal, useBusy } from "../ui";

export function ProtocolsTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const list = usePoll(() => c.listProtocols({ withStatus: true, brief: true }), [agentId], 1500);
  const types = usePoll(() => c.listProtocolTypes({}), [agentId]);
  const [creating, setCreating] = useState(false);
  const { busy, run } = useBusy();
  // Traffic flows have their own tab.
  const traffic = new Set(types.data?.types.filter((t) => t.category === "traffic").map((t) => t.name));
  const protos = (list.data?.protocols ?? []).filter((p) => !traffic.has(p.type));

  return (
    <div className="card">
      <div className="card-head">
        <h2>Protocols</h2>
        <button className="btn primary" onClick={() => setCreating(true)}>
          New protocol
        </button>
      </div>
      <ErrorBox error={list.error} />
      {!list.loading && protos.length === 0 ? (
        <Empty title="No protocols yet">Create one, or apply a scenario from the Scenario tab.</Empty>
      ) : (
        <div className="table-wrap">
          <table className="table hover">
            <thead>
              <tr>
                <th>Name</th>
                <th>Type</th>
                <th>Device</th>
                <th>State</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {protos.map((p) => {
                const s = summarize(p.type, p.state, p.error, p.status);
                return (
                  <tr key={p.name} onClick={() => navigate({ page: "protocol", agent: agentId, protocol: p.name })}>
                    <td className="mono strong">
                      <a href={href({ page: "protocol", agent: agentId, protocol: p.name })} onClick={(e) => e.stopPropagation()}>
                        {p.name}
                      </a>
                    </td>
                    <td className="mono">{p.type}</td>
                    <td className="mono">{p.device}</td>
                    <td>
                      <HealthText health={s.health}>{s.text}</HealthText>
                    </td>
                    <td className="actions">
                      <button
                        className="btn small danger-ghost"
                        disabled={busy}
                        onClick={(e) => {
                          e.stopPropagation();
                          if (confirm(`Delete protocol ${p.name}?`))
                            run(() => c.deleteProtocol({ name: p.name }), `Deleted ${p.name}`).then(list.refresh);
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
      {creating && <CreateProtocol agentId={agentId} category="protocol" onClose={() => setCreating(false)} onDone={list.refresh} />}
    </div>
  );
}

export type CreatePreset = { type?: string; name?: string; device?: string; config?: Obj; title?: string };

// CreateProtocol creates an instance of a type in one category ("protocol"
// or "traffic"). A preset fills in the type, name, device and config fields
// (e.g. the Ping… shortcut on devices).
export function CreateProtocol({
  agentId,
  category,
  preset,
  onClose,
  onDone,
}: {
  agentId: string;
  category: string;
  preset?: CreatePreset;
  onClose: () => void;
  onDone: () => void;
}) {
  const c = client(agentId);
  const allTypes = usePoll(() => c.listProtocolTypes({}), [agentId]);
  const devices = usePoll(() => c.listDevices({}), [agentId]);
  const [type, setType] = useState(preset?.type ?? "");
  const [name, setName] = useState(preset?.name ?? "");
  const [device, setDevice] = useState(preset?.device ?? "");
  const [cfg, setCfg] = useState<Obj>({});
  const { busy, run } = useBusy();

  const types = (allTypes.data?.types ?? []).filter((x) => (x.category || "protocol") === category);
  const t = types.find((x) => x.name === (type || types[0]?.name));
  const schema = useMemo(() => parseSchema(t?.configSchema), [t?.configSchema]);
  const [cfgFor, setCfgFor] = useState("");
  if (t && cfgFor !== t.name) {
    setCfgFor(t.name);
    setCfg({ ...defaults(schema), ...(t.name === preset?.type ? preset?.config : {}) });
  }
  const dev = device || devices.data?.devices[0]?.name || "";

  return (
    <Modal title={preset?.title ?? (category === "traffic" ? "New flow" : "New protocol")} onClose={onClose} wide>
      <ErrorBox error={allTypes.error || devices.error} />
      {devices.data && devices.data.devices.length === 0 && (
        <div className="warn-box">This agent has no devices yet. Create one in the Devices tab first.</div>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!t) return;
          run(() => c.createProtocol({ protocol: { name, type: t.name, device: dev, config: cfg as JsonObject } }), `Created ${name}`).then((r) => {
            if (!r) return;
            onDone();
            onClose();
            navigate({ page: "protocol", agent: agentId, protocol: name });
          });
        }}
      >
        <div className="form-grid">
          <div className="field">
            <label className="field-label">
              name<span className="req">*</span>
            </label>
            <input className="input mono" value={name} onChange={(e) => setName(e.target.value)} required autoFocus pattern="[A-Za-z0-9][A-Za-z0-9_.:\-]{0,62}" />
          </div>
          <div className="field">
            <label className="field-label">type</label>
            <select className="input" value={t?.name ?? ""} onChange={(e) => setType(e.target.value)}>
              {types.map((x) => (
                <option key={x.name} value={x.name}>
                  {x.name}
                </option>
              ))}
            </select>
            {t && <div className="hint">{t.description}</div>}
          </div>
          <div className="field">
            <label className="field-label">device</label>
            <select className="input" value={dev} onChange={(e) => setDevice(e.target.value)}>
              {devices.data?.devices.map((d) => (
                <option key={d.name} value={d.name}>
                  {d.name} ({d.port}
                  {d.vlans.length ? " · " + d.vlans.map((v) => v.vid).join(".") : ""})
                </option>
              ))}
            </select>
          </div>
        </div>
        <h4>Configuration</h4>
        <SchemaForm schema={schema} value={cfg} onChange={setCfg} mode="config" />
        <div className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={busy || !t || !dev || !name}>
            Create
          </button>
        </div>
      </form>
    </Modal>
  );
}
