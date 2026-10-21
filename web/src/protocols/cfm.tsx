import type { JsonObject } from "@bufbuild/protobuf";
import { registerView, type Health } from "./registry";
import { ago } from "../components/ValueView";

type RMEP = {
  mep_id: number;
  state: string;
  configured: boolean;
  mac?: string;
  rdi: boolean;
  port_status?: string;
  interface_status?: string;
  ccm_interval?: string;
  ccm_rx: number;
  seq_errors: number;
  last_rx?: string;
  state_since?: string;
};

type MEPStatus = {
  mac: string;
  level: number;
  mep_id: number;
  maid: string;
  ccm_interval: string;
  ccm_enabled: boolean;
  present_rdi: boolean;
  defects: string[];
  highest_defect: string;
  error_reason?: string;
  xcon_reason?: string;
  ais_received: boolean;
  lck_received: boolean;
  remote_meps: RMEP[];
  tx: Record<string, number>;
  rx: Record<string, number>;
  rx_errors: number;
  faults?: Record<string, unknown>;
};

const ALL_DEFECTS: [string, string][] = [
  ["DefRDICCM", "RDI"],
  ["DefMACstatus", "MAC status"],
  ["DefRemoteCCM", "LOC"],
  ["DefErrorCCM", "Error CCM"],
  ["DefXconCCM", "Xcon"],
];

function health(s: MEPStatus): Health {
  if (!s.highest_defect || s.highest_defect === "none") return s.ais_received || s.lck_received ? "warn" : "ok";
  return s.highest_defect === "DefRDICCM" ? "warn" : "bad";
}

const short = (d: string) => ALL_DEFECTS.find(([k]) => k === d)?.[1] ?? d;

function rmepHealth(r: RMEP): Health {
  if (r.state === "failed") return "bad";
  if (r.state !== "ok") return "idle";
  const macBad = (r.port_status && r.port_status !== "up") || (r.interface_status && r.interface_status !== "up");
  return r.rdi || macBad ? "warn" : "ok";
}

function Counters({ title, c }: { title: string; c: Record<string, number> }) {
  const keys = Object.keys(c).sort();
  return (
    <div className="counter-block">
      <h4>{title}</h4>
      {keys.length === 0 ? (
        <div className="muted">none</div>
      ) : (
        <div className="counters">
          {keys.map((k) => (
            <div key={k} className="counter">
              <span className="counter-k">{k}</span>
              <span className="counter-v mono">{c[k].toLocaleString()}</span>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function CFMStatus({ status }: { status: JsonObject }) {
  const s = status as unknown as MEPStatus;
  const active = new Set(s.defects ?? []);
  return (
    <div className="cfm">
      <div className="facts">
        <Fact k="MAID" v={s.maid} mono />
        <Fact k="Level" v={String(s.level)} />
        <Fact k="MEP ID" v={String(s.mep_id)} />
        <Fact k="Interval" v={s.ccm_interval} />
        <Fact k="MAC" v={s.mac} mono />
      </div>

      <div className="defects">
        {ALL_DEFECTS.map(([k, name]) => (
          <span key={k} className={`defect ${active.has(k) ? (k === "DefRDICCM" ? "warn" : "bad") : ""}`} title={k}>
            {name}
          </span>
        ))}
        <span className="sep" />
        <span className={`defect ${s.ccm_enabled ? "" : "warn"}`}>{s.ccm_enabled ? "CCM tx on" : "CCM tx off"}</span>
        <span className={`defect ${s.present_rdi ? "warn" : ""}`}>{s.present_rdi ? "sending RDI" : "no RDI tx"}</span>
        {s.ais_received && <span className="defect warn">AIS rx</span>}
        {s.lck_received && <span className="defect warn">LCK rx</span>}
      </div>
      {(s.error_reason || s.xcon_reason) && (
        <div className="reasons">
          {s.error_reason && <div><b>Error CCM:</b> {s.error_reason}</div>}
          {s.xcon_reason && <div><b>Xcon:</b> {s.xcon_reason}</div>}
        </div>
      )}
      {s.faults && Object.keys(s.faults).length > 0 && (
        <div className="reasons warn-box">
          <b>Injected faults:</b>{" "}
          {Object.entries(s.faults)
            .map(([k, v]) => `${k.replace(/_/g, " ")}=${String(v)}`)
            .join(", ")}
        </div>
      )}

      <h4>Remote MEPs</h4>
      {(s.remote_meps ?? []).length === 0 ? (
        <div className="muted">None yet. With remote_meps empty, peers are learned from their CCMs.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>MEP</th>
                <th>State</th>
                <th>MAC</th>
                <th>RDI</th>
                <th title="Port Status / Interface Status TLVs">Port/Intf</th>
                <th>Interval</th>
                <th className="num">CCM rx</th>
                <th className="num">Seq err</th>
                <th>Last CCM / state since</th>
              </tr>
            </thead>
            <tbody>
              {s.remote_meps.map((r) => (
                <tr key={r.mep_id}>
                  <td className="mono">
                    {r.mep_id}
                    {!r.configured && <span className="tag" title="auto-discovered">AD</span>}
                  </td>
                  <td>
                    <span className={`dot ${rmepHealth(r)}`} /> {r.state}
                  </td>
                  <td className="mono">{r.mac ?? "—"}</td>
                  <td>{r.rdi ? <span className="pill pill-warn">RDI</span> : "—"}</td>
                  <td className="mono">
                    {r.port_status ?? "—"} / {r.interface_status ?? "—"}
                  </td>
                  <td className="mono">{r.ccm_interval ?? "—"}</td>
                  <td className="num mono">{(r.ccm_rx ?? 0).toLocaleString()}</td>
                  <td className={`num mono ${r.seq_errors ? "warn-text" : ""}`}>{r.seq_errors ?? 0}</td>
                  <td className="small">
                    {r.last_rx && !r.last_rx.startsWith("0001") ? ago(new Date(r.last_rx)) : "—"}
                    <span className="muted"> / {r.state_since ? ago(new Date(r.state_since)) : "—"}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="counter-row">
        <Counters title="Transmitted" c={s.tx ?? {}} />
        <Counters title="Received" c={{ ...(s.rx ?? {}), ...(s.rx_errors ? { errors: s.rx_errors } : {}) }} />
      </div>
    </div>
  );
}

function Fact({ k, v, mono }: { k: string; v: string; mono?: boolean }) {
  return (
    <div className="fact">
      <div className="fact-k">{k}</div>
      <div className={`fact-v ${mono ? "mono" : ""}`}>{v}</div>
    </div>
  );
}

registerView("cfm-mep", {
  summary: (st) => {
    const s = st as unknown as MEPStatus;
    const rm = (s.remote_meps ?? []).map((r) => `${r.mep_id}:${r.state}`).join(" ");
    const def = (s.defects ?? []).map(short).join(", ");
    return {
      health: health(s),
      text: `L${s.level} MEP ${s.mep_id} · ${s.maid} · rmeps [${rm || "none"}]${def ? " · " + def : ""}`,
    };
  },
  Status: CFMStatus,
});
