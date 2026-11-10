import type { JsonObject } from "@bufbuild/protobuf";
import { HealthText } from "../ui";
import { registerView, type Health } from "./registry";

type SA = { an: number; next_pn: number; lowest_pn?: number; created: string } & Record<string, number | string>;
type RxSC = { sci: string; mac: string; port: number; last_rx?: string; sas: SA[] } & Record<string, unknown>;
type Key = { kn: number; an: number; key_server_mi: string; tx: boolean; rx: boolean; created: string };
type Peer = { sci: string; mi: string; mn: number; live: boolean; priority: number; key_server: boolean; receiving_latest: boolean; sending_latest: boolean; expires_in_s: number };
type MKA = {
  state: string;
  mi: string;
  mn: number;
  priority: number;
  key_server: boolean;
  key_server_sci?: string;
  cipher_suite?: string;
  latest_key?: Key;
  old_key?: Key;
  peers: Peer[];
  paused?: boolean;
  last_error?: string;
} & Record<string, unknown>;
type MACsecStatus = {
  scope: string;
  clear_tags: number;
  clear_vlans: number[];
  encrypted_vlans: number[];
  overhead: number;
  encrypt: boolean;
  confidentiality_offset: number;
  keys: string;
  secured: boolean;
  policy: string;
  mka?: MKA;
  sci: string;
  cipher: string;
  encoding_sa: number | null;
  tx_sas: SA[];
  rx_scs: RxSC[];
} & Record<string, unknown>;

const as = (s: JsonObject) => s as unknown as MACsecStatus;

const mkaHealth = (st: string): Health => (st === "secured" ? "ok" : st === "keying" || st === "potential peers" ? "warn" : st === "paused" ? "idle" : "bad");

function summary(st: JsonObject): { health: Health; text: string } {
  const s = as(st);
  const where = s.scope === "port" ? "port" : s.clear_vlans.length ? `VLAN ${s.clear_vlans.join(".")}` : "untagged";
  if (s.mka) {
    const m = s.mka;
    const live = m.peers.filter((p) => p.live).length;
    const key = m.latest_key ? `, SAK ${m.latest_key.kn} AN ${m.latest_key.an}` : "";
    return { health: mkaHealth(m.state), text: `${m.state}, ${live} peer${live === 1 ? "" : "s"}${m.key_server ? ", key server" : ""}${key} · ${where}` };
  }
  return { health: s.secured ? "ok" : "bad", text: `static key${s.secured ? "" : ", no transmit SA"} · ${where}` };
}

// Frame is the layout of a protected frame: what stays in clear, the SecTAG,
// what's encrypted, the ICV.
function Frame({ s }: { s: MACsecStatus }) {
  const inner = [...(s.scope === "port" ? ["each device's VLAN tags"] : s.encrypted_vlans.map((v) => `VLAN ${v}`)), "EtherType · data"].join(" · ");
  return (
    <div className="ms-frame-wrap">
      <div className="ms-frame" aria-label="Protected frame layout">
        <span className="ms-seg ms-clear">DA · SA</span>
        {s.clear_vlans.map((v) => (
          <span key={v} className="ms-seg ms-clear">
            VLAN {v}
          </span>
        ))}
        <span className="ms-seg ms-tag">SecTAG {s.overhead - 16} B</span>
        {s.encrypt && s.confidentiality_offset > 0 && <span className="ms-seg ms-clear">first {s.confidentiality_offset} B</span>}
        <span className={`ms-seg ${s.encrypt ? "ms-enc" : "ms-auth"}`}>{inner}</span>
        <span className="ms-seg ms-tag">ICV 16 B</span>
      </div>
      <div className="muted small-note ms-legend">
        <span className="ms-key ms-clear" /> in clear <span className="ms-key ms-tag" /> added by MACsec{" "}
        <span className={`ms-key ${s.encrypt ? "ms-enc" : "ms-auth"}`} /> {s.encrypt ? "encrypted" : "integrity protected, readable"}
      </div>
    </div>
  );
}

const counters = (o: Record<string, unknown>, keys: string[]) =>
  keys
    .filter((k) => typeof o[k] === "number" && (o[k] as number) > 0)
    .map((k) => `${k.replace(/_/g, " ")} ${(o[k] as number).toLocaleString()}`)
    .join(" · ");

const rxDrops = ["in_pkts_late", "in_pkts_invalid", "in_pkts_not_valid", "in_pkts_not_using_sa", "in_pkts_unused_sa", "in_pkts_delayed", "in_pkts_unchecked"];
const portDrops = ["in_pkts_no_tag", "in_pkts_untagged", "in_pkts_bad_tag", "in_pkts_no_sci", "in_pkts_unknown_sci", "out_pkts_no_sa", "out_pkts_pn_exhausted"];
const mkaCounters = ["tx_mkpdus", "rx_mkpdus", "rx_bad_icv", "rx_unknown_ckn", "rx_malformed", "rx_stale", "saks_distributed", "saks_received", "rekeys"];

function KeyCell({ k }: { k?: Key }) {
  if (!k) return <span className="muted">—</span>;
  return (
    <span className="mono">
      SAK {k.kn} · AN {k.an} · {k.rx ? "rx" : ""}
      {k.rx && k.tx ? "+" : ""}
      {k.tx ? "tx" : ""}
    </span>
  );
}

function Status({ status }: { status: JsonObject }) {
  const s = as(status);
  const m = s.mka;
  const tx = s.tx_sas.find((a) => a.an === s.encoding_sa);
  return (
    <div>
      <dl className="kv">
        <div className="kv-row">
          <dt>scope</dt>
          <dd>{s.scope === "port" ? "whole port (one secure channel)" : "this device"}</dd>
        </div>
        <div className="kv-row">
          <dt>keys</dt>
          <dd>{s.keys === "mka" ? "MKA" : "static"}</dd>
        </div>
        <div className="kv-row">
          <dt>cipher</dt>
          <dd className="mono">{s.cipher}</dd>
        </div>
        <div className="kv-row">
          <dt>SCI</dt>
          <dd className="mono">{s.sci}</dd>
        </div>
        <div className="kv-row">
          <dt>overhead</dt>
          <dd className="mono">+{s.overhead} bytes per frame</dd>
        </div>
        <div className="kv-row">
          <dt>policy</dt>
          <dd>{s.policy}</dd>
        </div>
        <div className="kv-row">
          <dt>sending</dt>
          <dd className="mono">{tx ? `AN ${tx.an}, next PN ${tx.next_pn.toLocaleString()}` : <span className="warn-text">no transmit SA</span>}</dd>
        </div>
      </dl>
      <h4>Frame</h4>
      <Frame s={s} />
      {m && (
        <>
          <h4>
            MKA <HealthText health={mkaHealth(m.state)}>{m.state}</HealthText>
          </h4>
          <p className="muted">
            {m.key_server ? "Key server" : m.key_server_sci ? <>Key server <span className="mono">{m.key_server_sci}</span></> : "No key server yet"}, priority {m.priority}. Latest key <KeyCell k={m.latest_key} />
            {m.old_key && (
              <>
                , old key <KeyCell k={m.old_key} />
              </>
            )}
            .
          </p>
          {m.last_error && <p className="muted small-note">last error: {m.last_error}</p>}
          {m.peers.length === 0 ? (
            <div className="muted">No peers heard.</div>
          ) : (
            <div className="table-wrap">
              <table className="table dense">
                <thead>
                  <tr>
                    <th>Peer SCI</th>
                    <th>State</th>
                    <th className="num">Priority</th>
                    <th>Latest key</th>
                    <th className="num">Expires</th>
                  </tr>
                </thead>
                <tbody>
                  {m.peers.map((p) => (
                    <tr key={p.mi}>
                      <td className="mono">
                        {p.sci}
                        {p.key_server && <div className="muted small-note">key server</div>}
                      </td>
                      <td>
                        <HealthText health={p.live ? "ok" : "warn"}>{p.live ? "live" : "potential"}</HealthText>
                      </td>
                      <td className="num mono">{p.priority}</td>
                      <td className="mono">{p.receiving_latest ? (p.sending_latest ? "rx+tx" : "rx") : "—"}</td>
                      <td className="num mono">{Math.max(0, p.expires_in_s).toFixed(1)} s</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          <div className="muted mono small-note">{counters(m, mkaCounters)}</div>
        </>
      )}
      <h4>Receive channels</h4>
      {s.rx_scs.length === 0 ? (
        <div className="muted">None yet.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>SCI</th>
                <th>SAs</th>
                <th className="num">OK</th>
                <th>Dropped</th>
              </tr>
            </thead>
            <tbody>
              {s.rx_scs.map((sc) => (
                <tr key={sc.sci}>
                  <td className="mono">
                    {sc.sci}
                    <div className="muted small-note">{sc.mac}</div>
                  </td>
                  <td className="mono">{sc.sas.map((a) => `AN ${a.an} next ${a.next_pn.toLocaleString()}`).join(", ") || "—"}</td>
                  <td className="num mono">{((sc.in_pkts_ok as number) ?? 0).toLocaleString()}</td>
                  <td className="mono small-note">{counters(sc, rxDrops) || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <div className="muted mono small-note">
        {counters(s, ["out_pkts_encrypted", "out_pkts_protected"])}
        {counters(s, portDrops) && ` · ${counters(s, portDrops)}`}
      </div>
    </div>
  );
}

registerView("macsec", { summary, Status });
