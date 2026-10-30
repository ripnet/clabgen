import type { JsonObject } from "@bufbuild/protobuf";
import { ValueView, fmtScalar } from "../components/ValueView";
import { registerView, type Summary } from "./registry";

type Binding = { prefix: string };
type V4 = { state: string; address?: string; gateway?: string; last_nak?: string };
type V6 = { state: string; addresses?: Binding[]; prefixes?: Binding[]; ia_na_status?: string; ia_pd_status?: string };

const leased = (state: string) => state === "bound" || state === "renewing" || state === "rebinding";

// Waiting for a lease is a warning: in a test, a client without one is
// what to look at.
function idle(state: string, why?: string): Summary {
  if (state === "released") return { health: "idle", text: "released" };
  return { health: "warn", text: why ? `${state} · ${why}` : state };
}

function summary4(st: JsonObject): Summary {
  const s = st as unknown as V4;
  if (!leased(s.state) || !s.address) return idle(s.state, s.last_nak !== undefined ? `NAK ${s.last_nak}`.trim() : undefined);
  const tail = s.state === "bound" ? "" : ` · ${s.state}`;
  return { health: "ok", text: `${s.address}${s.gateway ? ` · gw ${s.gateway}` : ""}${tail}` };
}

function summary6(st: JsonObject): Summary {
  const s = st as unknown as V6;
  const got = [...(s.addresses ?? []), ...(s.prefixes ?? [])].map((b) => b.prefix);
  if (!leased(s.state) || !got.length) return idle(s.state);
  const missing = [s.ia_na_status, s.ia_pd_status].filter(Boolean);
  const tail = s.state === "bound" ? "" : ` · ${s.state}`;
  return { health: missing.length ? "warn" : "ok", text: [...got, ...missing].join(" · ") + tail };
}

registerView("dhcp", { summary: summary4 });
registerView("dhcpv6", { summary: summary6 });

// ---- servers ----

type Relay4 = { giaddr: string; hops: number; circuit_id?: string; remote_id?: string; link_selection?: string; subscriber_id?: string; other_suboptions?: string[] };
type Lease4 = { address: string; state: string; mac: string; client_id: string; hostname?: string; expires_in_s: number; relay?: Relay4 };
type Pool4 = { subnet: string; range: string; size: string; bound: number; offered: number };
type Server4 = { paused?: boolean; paused_for_s?: number; pools: Pool4[]; leases?: Lease4[]; counters: Record<string, number> };

type Hop6 = {
  link_address: string;
  peer_address: string;
  hop_count: number;
  interface_id?: string;
  remote_id?: string;
  subscriber_id?: string;
  client_link_layer_address?: string;
  other_options?: string[];
};
type Binding6 = { prefix: string; kind: string; state: string; duid: string; iaid: number; preferred_in_s: number; valid_in_s: number; relays?: Hop6[] };
type Pool6 = { link: string; addresses?: string; addresses_bound: number; prefixes?: string; prefixes_bound: number };
type Server6 = { duid: string; paused?: boolean; paused_for_s?: number; pools: Pool6[]; bindings?: Binding6[]; counters: Record<string, number> };

const pausedText = (s: { paused?: boolean; paused_for_s?: number }) => (s.paused_for_s ? `paused for ${Math.ceil(s.paused_for_s)} s` : "paused");

function summaryServer4(st: JsonObject): Summary {
  const s = st as unknown as Server4;
  const text = (s.pools ?? []).map((p) => `${p.subnet} ${p.bound}/${p.size}`).join(" · ");
  return s.paused ? { health: "warn", text: `${pausedText(s)} · ${text}` } : { health: "ok", text };
}

function summaryServer6(st: JsonObject): Summary {
  const s = st as unknown as Server6;
  const text = (s.pools ?? []).map((p) => `${p.link} ${p.addresses_bound} addr, ${p.prefixes_bound} pd`).join(" · ");
  return s.paused ? { health: "warn", text: `${pausedText(s)} · ${text}` } : { health: "ok", text };
}

// relay4Text is how a DHCPv4 lease was relayed, in one line.
function relay4Text(r?: Relay4): string {
  if (!r) return "direct";
  const parts = [`${r.giaddr} (hops ${r.hops})`];
  if (r.circuit_id) parts.push(`circuit ${r.circuit_id}`);
  if (r.remote_id) parts.push(`remote ${r.remote_id}`);
  if (r.link_selection) parts.push(`link ${r.link_selection}`);
  if (r.subscriber_id) parts.push(`subscriber ${r.subscriber_id}`);
  for (const o of r.other_suboptions ?? []) parts.push(o);
  return parts.join(" · ");
}

// A DHCPv6 binding's relays, the one nearest the client first.
function hopsText(hs?: Hop6[]): string[] {
  if (!hs?.length) return ["direct"];
  return [...hs].reverse().map((h) => {
    const parts = [`link ${h.link_address}`];
    if (h.interface_id) parts.push(`interface ${h.interface_id}`);
    if (h.remote_id) parts.push(`remote ${h.remote_id}`);
    if (h.subscriber_id) parts.push(`subscriber ${h.subscriber_id}`);
    if (h.client_link_layer_address) parts.push(`client ${h.client_link_layer_address}`);
    for (const o of h.other_options ?? []) parts.push(o);
    return parts.join(" · ");
  });
}

const secsText = (s: number) => `${Math.round(s)} s`;

function Counters({ c }: { c: Record<string, number> }) {
  return <ValueView value={{ counters: c }} />;
}

function Server4Status({ status }: { status: JsonObject }) {
  const s = status as unknown as Server4;
  const leases = s.leases ?? [];
  return (
    <div>
      {s.paused && <p className="warn-text">{pausedText(s)}: requests are dropped</p>}
      <h4>Pools</h4>
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>subnet</th>
              <th>range</th>
              <th>size</th>
              <th>bound</th>
              <th>offered</th>
            </tr>
          </thead>
          <tbody>
            {(s.pools ?? []).map((p) => (
              <tr key={p.subnet}>
                <td className="mono">{p.subnet}</td>
                <td className="mono">{p.range}</td>
                <td className="mono">{p.size}</td>
                <td className="mono">{p.bound}</td>
                <td className="mono">{p.offered}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h4>Leases ({leases.length})</h4>
      {leases.length > 0 && (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>address</th>
                <th>state</th>
                <th>client</th>
                <th>expires in</th>
                <th>relayed by</th>
              </tr>
            </thead>
            <tbody>
              {leases.map((l) => (
                <tr key={l.address}>
                  <td className="mono">{l.address}</td>
                  <td>{l.state}</td>
                  <td className="mono">
                    {l.mac}
                    {l.hostname && <div className="muted">{l.hostname}</div>}
                  </td>
                  <td className="mono">{secsText(l.expires_in_s)}</td>
                  <td className="mono">{relay4Text(l.relay)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <Counters c={s.counters ?? {}} />
    </div>
  );
}

function Server6Status({ status }: { status: JsonObject }) {
  const s = status as unknown as Server6;
  const bs = s.bindings ?? [];
  return (
    <div>
      {s.paused && <p className="warn-text">{pausedText(s)}: requests are dropped</p>}
      <dl className="kv">
        <div className="kv-row">
          <dt>server DUID</dt>
          <dd className="mono">{s.duid}</dd>
        </div>
      </dl>
      <h4>Pools</h4>
      <div className="table-wrap">
        <table className="table dense">
          <thead>
            <tr>
              <th>link</th>
              <th>addresses</th>
              <th>bound</th>
              <th>prefixes</th>
              <th>delegated</th>
            </tr>
          </thead>
          <tbody>
            {(s.pools ?? []).map((p) => (
              <tr key={p.link}>
                <td className="mono">{p.link}</td>
                <td className="mono">{p.addresses || "—"}</td>
                <td className="mono">{p.addresses_bound}</td>
                <td className="mono">{p.prefixes || "—"}</td>
                <td className="mono">{p.prefixes_bound}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <h4>Bindings ({bs.length})</h4>
      {bs.length > 0 && (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>address or prefix</th>
                <th>state</th>
                <th>client DUID · IAID</th>
                <th>valid for</th>
                <th>relayed by (nearest the client first)</th>
              </tr>
            </thead>
            <tbody>
              {bs.map((b) => (
                <tr key={b.prefix}>
                  <td className="mono">{b.prefix}</td>
                  <td>{b.state}</td>
                  <td className="mono">
                    {b.duid} · {fmtScalar(b.iaid, "iaid")}
                  </td>
                  <td className="mono">{secsText(b.valid_in_s)}</td>
                  <td className="mono">
                    {hopsText(b.relays).map((h, i) => (
                      <div key={i}>{h}</div>
                    ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <Counters c={s.counters ?? {}} />
    </div>
  );
}

registerView("dhcp-server", { summary: summaryServer4, Status: Server4Status });
registerView("dhcpv6-server", { summary: summaryServer6, Status: Server6Status });
