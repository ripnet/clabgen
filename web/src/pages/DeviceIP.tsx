import { useState } from "react";
import { client, usePoll } from "../api";
import { ErrorBox, Modal, useBusy } from "../ui";

const fmtAge = (ms: bigint) => {
  const s = Number(ms) / 1000;
  return s < 60 ? `${s.toFixed(0)}s` : s < 3600 ? `${(s / 60).toFixed(0)}m` : `${(s / 3600).toFixed(1)}h`;
};

// DeviceIP shows a device's effective addresses, neighbor cache and IP
// counters, and resolves addresses on demand.
export function DeviceIP({ agentId, device, onClose }: { agentId: string; device: string; onClose: () => void }) {
  const c = client(agentId);
  const ip = usePoll(() => c.getDeviceIP({ name: device }), [agentId, device], 2000);
  const [target, setTarget] = useState("");
  const [result, setResult] = useState<string>();
  const { busy, run } = useBusy();
  const m = ip.data;
  const counters = Object.entries(m?.counters ?? {})
    .filter(([, v]) => v > 0n)
    .sort(([a], [b]) => a.localeCompare(b));

  const resolve = () =>
    run(() => c.resolveNeighbor({ device, ip: target.trim() })).then((r) => {
      if (!r) return setResult(undefined);
      const via = r.nextHop !== target.trim() ? ` via ${r.nextHop}` : "";
      setResult(`${target.trim()}${via} is at ${r.mac} (${r.cached ? "cached" : (Number(r.rttUs) / 1000).toFixed(2) + " ms"})`);
      ip.refresh();
    });

  return (
    <Modal title={`IP on ${device}`} onClose={onClose} wide>
      <ErrorBox error={ip.error} />
      {m && (
        <>
          <dl className="kv ip-kv">
            <div className="kv-row">
              <dt>addresses</dt>
              <dd className="mono">
                {m.addresses.map((a) => (
                  <div key={a}>{a}</div>
                ))}
              </dd>
            </div>
            <div className="kv-row">
              <dt>gateways</dt>
              <dd className="mono">{[m.gateway4, m.gateway6].filter(Boolean).join(", ") || "—"}</dd>
            </div>
          </dl>
          <form
            className="resolve-row"
            onSubmit={(e) => {
              e.preventDefault();
              resolve();
            }}
          >
            <input className="input mono" value={target} onChange={(e) => setTarget(e.target.value)} placeholder="IP to resolve (off-link: via gateway)" aria-label="IP to resolve" />
            <button className="btn" disabled={busy || !target.trim()}>
              {busy ? "Resolving…" : "Resolve"}
            </button>
            <button type="button" className="btn danger-ghost" disabled={busy} onClick={() => run(() => c.clearNeighbors({ device }), "Neighbor cache cleared").then(ip.refresh)}>
              Clear cache
            </button>
          </form>
          {result && <div className="result mono">{result}</div>}
          <h4>Neighbors</h4>
          {m.neighbors.length === 0 ? (
            <div className="muted">None learned yet.</div>
          ) : (
            <div className="table-wrap">
              <table className="table dense">
                <thead>
                  <tr>
                    <th>IP</th>
                    <th>MAC</th>
                    <th>State</th>
                    <th>Age</th>
                  </tr>
                </thead>
                <tbody>
                  {m.neighbors.map((n) => (
                    <tr key={n.ip}>
                      <td className="mono">{n.ip}</td>
                      <td className="mono">{n.mac || "—"}</td>
                      <td>
                        <span className={n.state === "reachable" ? "pill pill-on" : n.state === "incomplete" ? "pill pill-warn" : "pill"}>{n.state}</span>
                      </td>
                      <td className="mono">{n.mac ? fmtAge(n.ageMs) : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <h4>Counters</h4>
          {counters.length === 0 ? (
            <div className="muted">No IP traffic yet.</div>
          ) : (
            <div className="counters">
              {counters.map(([k, v]) => (
                <div className="counter" key={k}>
                  <span className="counter-k">{k.replace(/_/g, " ")}</span>
                  <span className={k === "address_conflicts" ? "counter-v warn-text" : "counter-v"}>{v.toString()}</span>
                </div>
              ))}
            </div>
          )}
        </>
      )}
    </Modal>
  );
}
