import { useRef } from "react";
import { client, usePoll } from "../api";
import type { Port } from "../gen/clabgen/v1/agent_pb";
import { Dot, Empty, ErrorBox, useBusy } from "../ui";

type Sample = { t: number; rx: bigint; tx: bigint };

const rate = (d: bigint, ms: number) => (ms > 0 ? (Number(d) * 1000) / ms : 0);
const fmtRate = (r: number) => (r < 0.05 ? "—" : r < 10 ? r.toFixed(1) : Math.round(r).toLocaleString());

export function PortsTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const ports = usePoll(() => c.listPorts({}), [agentId], 1000);
  const prev = useRef<Record<string, Sample>>({});
  const rates = useRef<Record<string, { rx: number; tx: number }>>({});
  const { busy, run } = useBusy();

  const now = Date.now();
  for (const p of ports.data?.ports ?? []) {
    const k = p.counters;
    if (!k) continue;
    const s = prev.current[p.name];
    if (!s || now - s.t >= 900) {
      if (s) rates.current[p.name] = { rx: rate(k.rxFrames - s.rx, now - s.t), tx: rate(k.txFrames - s.tx, now - s.t) };
      prev.current[p.name] = { t: now, rx: k.rxFrames, tx: k.txFrames };
    }
  }

  const toggle = (p: Port) =>
    run(() => c.setPortAdmin({ name: p.name, up: !p.adminUp }), `${p.name} admin ${p.adminUp ? "down" : "up"}`).then(ports.refresh);

  return (
    <div className="card">
      <div className="card-head">
        <h2>Ports</h2>
        <span className="muted">Setting a port down drops carrier on its veth peer, which is how the DUT sees a link failure.</span>
      </div>
      <ErrorBox error={ports.error} />
      {!ports.loading && !ports.data?.ports.length ? (
        <Empty title="No data ports">The agent adopts eth1, eth2, … as containerlab attaches them.</Empty>
      ) : (
        <div className="table-wrap">
          <table className="table">
            <thead>
              <tr>
                <th>Port</th>
                <th>Admin</th>
                <th>Oper</th>
                <th>MAC</th>
                <th className="num">MTU</th>
                <th className="num">RX frames</th>
                <th className="num">RX pps</th>
                <th className="num">TX frames</th>
                <th className="num">TX pps</th>
                <th className="num">TX errors</th>
                <th className="num" title="Frames no device matched (wrong VLAN or unicast MAC)">Unmatched</th>
              </tr>
            </thead>
            <tbody>
              {ports.data?.ports.map((p) => {
                const k = p.counters;
                const r = rates.current[p.name];
                return (
                  <tr key={p.name}>
                    <td className="mono strong">{p.name}</td>
                    <td>
                      <button className={`toggle ${p.adminUp ? "on" : ""}`} disabled={busy} onClick={() => toggle(p)} aria-pressed={p.adminUp} title={`Set admin ${p.adminUp ? "down" : "up"}`}>
                        <span className="toggle-knob" />
                        <span>{p.adminUp ? "up" : "down"}</span>
                      </button>
                    </td>
                    <td>
                      <Dot health={p.operUp ? "ok" : "bad"} /> {p.operUp ? "up" : "down"}
                    </td>
                    <td className="mono">{p.mac}</td>
                    <td className="num mono">{p.mtu}</td>
                    <td className="num mono">{k?.rxFrames.toLocaleString()}</td>
                    <td className="num mono">{fmtRate(r?.rx ?? 0)}</td>
                    <td className="num mono">{k?.txFrames.toLocaleString()}</td>
                    <td className="num mono">{fmtRate(r?.tx ?? 0)}</td>
                    <td className={`num mono ${k?.txErrors ? "bad-text" : ""}`}>{k?.txErrors.toLocaleString()}</td>
                    <td className="num mono">{k?.rxUnmatched.toLocaleString()}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
