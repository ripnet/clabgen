import { useState } from "react";
import { client, usePoll } from "../api";
import type { Device, VlanTag } from "../gen/clabgen/v1/agent_pb";
import { DeviceIP } from "./DeviceIP";
import { CreateProtocol } from "./ProtocolsTab";
import { Empty, ErrorBox, Modal, useBusy } from "../ui";

const TPIDS = [0x8100, 0x88a8, 0x9100];
const hex = (n: number) => "0x" + n.toString(16).padStart(4, "0");

export function vlanText(v: VlanTag[]): string {
  if (!v.length) return "untagged";
  return v.map((t) => `${t.tpid && t.tpid !== 0x8100 ? hex(t.tpid) + ":" : ""}${t.vid}${t.pcp ? "." + t.pcp : ""}`).join(" / ");
}

export function DevicesTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const devs = usePoll(() => c.listDevices({}), [agentId], 5000);
  const protos = usePoll(() => c.listProtocols({ withStatus: false }), [agentId], 5000);
  const [editing, setEditing] = useState<Device | "new">();
  const [ipOf, setIpOf] = useState<string>();
  const [pingFrom, setPingFrom] = useState<Device>();
  const { busy, run } = useBusy();
  const all = devs.data?.devices ?? [];
  // Group members are listed in the Groups tab.
  const list = all.filter((d) => !d.group);
  const members = all.length - list.length;
  const using = (d: string) => (protos.data?.protocols ?? []).filter((p) => p.device === d).map((p) => p.name);

  return (
    <div className="card">
      <div className="card-head">
        <h2>Devices</h2>
        <button className="btn primary" onClick={() => setEditing("new")}>
          New device
        </button>
      </div>
      <p className="muted">A device is an emulated endpoint on a port: a MAC address, a VLAN stack and optionally IP addresses (it answers ARP, ND and ping). Protocols run on devices.</p>
      <ErrorBox error={devs.error} />
      {members > 0 && (
        <p className="muted small-note">
          {members} more devices belong to <a href={`#/a/${encodeURIComponent(agentId)}/groups`}>device groups</a>.
        </p>
      )}
      {!devs.loading && list.length === 0 ? (
        <Empty title="No devices yet" />
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Port</th>
                <th>MAC</th>
                <th>VLANs (outer first)</th>
                <th>Addresses</th>
                <th>Protocols</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {list.map((d) => {
                const u = using(d.name);
                return (
                  <tr key={d.name}>
                    <td className="mono strong">{d.name}</td>
                    <td className="mono">{d.port}</td>
                    <td className="mono">
                      {d.mac}
                      {d.macDerived && (
                        <span className="tag" title="Derived from host and device name">
                          auto
                        </span>
                      )}
                    </td>
                    <td className="mono">{vlanText(d.vlans)}</td>
                    <td className="mono addr-cell">
                      {d.addresses.length ? d.addresses.map((a) => <div key={a}>{a}</div>) : "—"}
                      {(d.gateway4 || d.gateway6) && <div className="muted">gw {[d.gateway4, d.gateway6].filter(Boolean).join(", ")}</div>}
                    </td>
                    <td className="mono">{u.join(", ") || "—"}</td>
                    <td className="actions">
                      <button className="btn small" disabled={!d.addresses.length} title={d.addresses.length ? "Neighbors, counters and resolve" : "No IP addresses"} onClick={() => setIpOf(d.name)}>
                        IP
                      </button>
                      <button className="btn small" disabled={!d.addresses.length} title={d.addresses.length ? "Start an ICMP flow from this device" : "No IP addresses"} onClick={() => setPingFrom(d)}>
                        Ping…
                      </button>
                      <button className="btn small" disabled={busy} onClick={() => setEditing(d)}>
                        Edit
                      </button>
                      <button
                        className="btn small danger-ghost"
                        disabled={busy}
                        onClick={() => {
                          const extra = u.length ? `\n\nIts protocols (${u.join(", ")}) will stop and wait for a device with this name.` : "";
                          if (confirm(`Delete device ${d.name}?${extra}`)) run(() => c.deleteDevice({ name: d.name }), `Deleted ${d.name}`).then(devs.refresh);
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
      {pingFrom && (
        <CreateProtocol
          agentId={agentId}
          category="traffic"
          preset={{
            type: "icmp",
            title: `Ping from ${pingFrom.name}`,
            device: pingFrom.name,
            name: uniqueName(`ping-${pingFrom.name}`, protos.data?.protocols.map((p) => p.name) ?? []),
            config: { auto_start: true, destination: pingFrom.gateway4 || pingFrom.gateway6 || "" },
          }}
          onClose={() => setPingFrom(undefined)}
          onDone={protos.refresh}
        />
      )}
      {ipOf && <DeviceIP agentId={agentId} device={ipOf} onClose={() => setIpOf(undefined)} />}
      {editing && (
        <DeviceForm
          agentId={agentId}
          device={editing === "new" ? undefined : editing}
          protocols={editing === "new" ? [] : using(editing.name)}
          onClose={() => setEditing(undefined)}
          onDone={() => {
            devs.refresh();
            protos.refresh();
          }}
        />
      )}
    </div>
  );
}

type Tag = { tpid: number; vid: string; pcp: string };

const toTags = (v: VlanTag[]): Tag[] => v.map((t) => ({ tpid: t.tpid || 0x8100, vid: String(t.vid), pcp: t.pcp ? String(t.pcp) : "" }));

// DeviceForm creates a device, or edits one when `device` is set. The name
// is the device's key and can't change.
function DeviceForm({
  agentId,
  device,
  protocols,
  onClose,
  onDone,
}: {
  agentId: string;
  device?: Device;
  protocols: string[];
  onClose: () => void;
  onDone: () => void;
}) {
  const c = client(agentId);
  const editing = !!device;
  const ports = usePoll(() => c.listPorts({}), [agentId]);
  const [name, setName] = useState(device?.name ?? "");
  const [port, setPort] = useState(device?.port ?? "");
  const [derived, setDerived] = useState(device ? device.macDerived : true);
  const [mac, setMac] = useState(device && !device.macDerived ? device.mac : "");
  const [tags, setTags] = useState<Tag[]>(device ? toTags(device.vlans) : []);
  const [addrs, setAddrs] = useState<string[]>(device?.addresses.length ? [...device.addresses] : [""]);
  const [gw4, setGw4] = useState(device?.gateway4 ?? "");
  const [gw6, setGw6] = useState(device?.gateway6 ?? "");
  const { busy, run } = useBusy();
  const p = port || ports.data?.ports[0]?.name || "eth1";

  const set = (i: number, t: Partial<Tag>) => setTags((x) => x.map((y, j) => (j === i ? { ...y, ...t } : y)));

  const vlans = tags.map((t) => ({ tpid: t.tpid, vid: Number(t.vid), pcp: Number(t.pcp || 0) }));
  // Only port, MAC and VLAN changes restart protocols; IP changes apply in place.
  const l2Changed =
    !!device &&
    (p !== device.port ||
      (derived ? !device.macDerived : device.macDerived || mac.toLowerCase() !== device.mac.toLowerCase()) ||
      vlanText(vlans as VlanTag[]) !== vlanText(device.vlans));

  const submit = () => {
    const d = {
      name,
      port: p,
      mac: derived ? "" : mac,
      vlans,
      addresses: addrs.map((a) => a.trim()).filter(Boolean),
      gateway4: gw4.trim(),
      gateway6: gw6.trim(),
    };
    const call = (): Promise<unknown> => (editing ? c.updateDevice({ device: d }) : c.createDevice({ device: d }));
    run(call, editing ? `Updated ${name}` : `Created ${name}`).then((r) => {
      if (r) {
        onDone();
        onClose();
      }
    });
  };

  return (
    <Modal title={editing ? `Edit device ${device!.name}` : "New device"} onClose={onClose}>
      {editing && l2Changed && protocols.length > 0 && (
        <div className="warn-box">
          Saving restarts the protocols on this device ({protocols.join(", ")}): their counters and remote state are reset.
        </div>
      )}
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
      >
        <div className="form-grid">
          <div className="field">
            <label className="field-label">
              name<span className="req">*</span>
            </label>
            <input
              className="input mono"
              value={name}
              onChange={(e) => setName(e.target.value)}
              required
              autoFocus={!editing}
              readOnly={editing}
              title={editing ? "The name identifies the device; delete and recreate to rename" : undefined}
              pattern="[A-Za-z0-9][A-Za-z0-9_.:\-]{0,62}"
            />
          </div>
          <div className="field">
            <label className="field-label">port</label>
            <input className="input mono" list="ports" value={p} onChange={(e) => setPort(e.target.value)} />
            <datalist id="ports">
              {ports.data?.ports.map((x) => (
                <option key={x.name} value={x.name} />
              ))}
            </datalist>
            <div className="hint">Ports that don't exist yet are fine: the device starts working when the port appears.</div>
          </div>
          <div className="field">
            <label className="field-label">mac</label>
            <input
              className="input mono"
              value={derived ? "" : mac}
              disabled={derived}
              onChange={(e) => setMac(e.target.value)}
              placeholder={derived ? (device?.macDerived ? device.mac : "derived from host + name") : "02:00:00:00:00:01"}
              required={!derived}
              pattern="([0-9A-Fa-f]{2}[:\-]){5}[0-9A-Fa-f]{2}"
              aria-label="MAC address"
            />
            <label className="switch small">
              <input
                type="checkbox"
                checked={derived}
                onChange={(e) => {
                  setDerived(e.target.checked);
                  if (!e.target.checked && !mac && device) setMac(device.mac);
                }}
              />
              <span>use derived MAC</span>
            </label>
          </div>
        </div>
        <h4>VLAN stack</h4>
        {tags.length === 0 && <div className="muted">Untagged.</div>}
        {tags.map((t, i) => (
          <div className="vlan-row" key={i}>
            <span className="muted">{tags.length === 1 ? "tag" : i === 0 ? "outer" : i === tags.length - 1 ? "inner" : `tag ${i + 1}`}</span>
            <select className="input" value={t.tpid} onChange={(e) => set(i, { tpid: Number(e.target.value) })} aria-label="TPID">
              {TPIDS.map((x) => (
                <option key={x} value={x}>
                  {hex(x)}
                </option>
              ))}
            </select>
            <input className="input mono" type="number" min={0} max={4095} required placeholder="VID" value={t.vid} onChange={(e) => set(i, { vid: e.target.value })} aria-label="VLAN ID" />
            <input className="input mono" type="number" min={0} max={7} placeholder="PCP" value={t.pcp} onChange={(e) => set(i, { pcp: e.target.value })} aria-label="PCP" />
            <button type="button" className="icon-btn" onClick={() => setTags((x) => x.filter((_, j) => j !== i))} aria-label="Remove tag">
              ×
            </button>
          </div>
        ))}
        <div className="btn-row">
          {tags.length < 4 && (
            <button type="button" className="btn small" onClick={() => setTags((x) => [...x, { tpid: 0x8100, vid: "", pcp: "" }])}>
              Add tag
            </button>
          )}
          {tags.length === 1 && (
            <button type="button" className="link" onClick={() => setTags((x) => [{ ...x[0], tpid: 0x88a8 }, { tpid: 0x8100, vid: "", pcp: "" }])}>
              make it QinQ (0x88a8 outer)
            </button>
          )}
          {tags.length > 0 && (
            <button type="button" className="link" onClick={() => setTags([])}>
              remove all tags
            </button>
          )}
        </div>
        <h4>IP</h4>
        {addrs.map((a, i) => (
          <div className="vlan-row ip-row" key={i}>
            <span className="muted">{i === 0 ? "address" : ""}</span>
            <input
              className="input mono"
              value={a}
              placeholder="10.0.0.2/24 or 2001:db8::2/64"
              aria-label="IP address"
              onChange={(e) => setAddrs((x) => x.map((y, j) => (j === i ? e.target.value : y)))}
            />
            <button type="button" className="icon-btn" onClick={() => setAddrs((x) => (x.length > 1 ? x.filter((_, j) => j !== i) : [""]))} aria-label="Remove address">
              ×
            </button>
          </div>
        ))}
        <div className="btn-row">
          <button type="button" className="btn small" onClick={() => setAddrs((x) => [...x, ""])}>
            Add address
          </button>
        </div>
        <div className="form-grid">
          <div className="field">
            <label className="field-label">gateway4</label>
            <input className="input mono" value={gw4} onChange={(e) => setGw4(e.target.value)} placeholder="optional, e.g. 10.0.0.1" />
          </div>
          <div className="field">
            <label className="field-label">gateway6</label>
            <input className="input mono" value={gw6} onChange={(e) => setGw6(e.target.value)} placeholder="optional, e.g. fe80::1" />
          </div>
        </div>
        <div className="hint">The device answers ARP, ND and ping on these addresses. With any IPv6 address an EUI-64 link-local is added automatically.</div>
        <div className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={busy || !name}>
            {editing ? "Save" : "Create"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function uniqueName(base: string, taken: string[]): string {
  if (!taken.includes(base)) return base;
  for (let i = 2; ; i++) if (!taken.includes(`${base}-${i}`)) return `${base}-${i}`;
}
