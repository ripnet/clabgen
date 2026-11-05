import { useEffect, useState } from "react";
import { client, errorText } from "../api";
import type { ConfigEditorProps } from "../protocols/registry";
import { ErrorBox } from "../ui";
import { compressPrefixes } from "./RoutePicker";

type Route = { family: string; vpn_prefix?: string; route_targets?: string[] };
type BGPConfig = {
  peers?: { address: string; families?: string[] }[];
  routes?: { prefix: string; rd?: string; route_targets?: string[] }[];
};
type Speaker = { name: string; peers: string[]; own: NonNullable<BGPConfig["routes"]> };
export type VPNChoice = { key: string; target: string; v6: boolean; prefixes: string[] };

const isVPN = (f: string) => f === "ipv4-vpn" || f === "ipv6-vpn";

// firstHost is the address a flow should use in a prefix: the one after the
// network address, or the only one.
export function firstHost(prefix: string): string {
  const [addr, len] = prefix.split("/");
  if (addr.includes(":")) return Number(len) >= 127 ? addr : addr.replace(/::$/, "::1");
  const parts = addr.split(".").map(Number);
  if (Number(len) < 31) parts[3] += 1;
  return parts.join(".");
}

// vpnChoices groups received VPN routes by route target and address family:
// a flow is in one VPN and of one family.
export function vpnChoices(routes: Route[]): VPNChoice[] {
  const m = new Map<string, VPNChoice>();
  for (const r of routes) {
    if (!r.vpn_prefix || !isVPN(r.family)) continue;
    const v6 = r.family === "ipv6-vpn";
    for (const target of r.route_targets ?? []) {
      const key = `${target}/${v6 ? 6 : 4}`;
      const c = m.get(key) ?? { key, target, v6, prefixes: [] };
      if (!c.prefixes.includes(r.vpn_prefix)) c.prefixes.push(r.vpn_prefix);
      m.set(key, c);
    }
  }
  return [...m.values()].sort((a, b) => a.key.localeCompare(b.key));
}

// VPNPicker puts a flow in a VPN: pick a route target from the routes the
// device's BGP speakers received, and the flow sends to those prefixes, from
// one the device advertises in that VPN. It's hidden without a VPN peer.
export function VPNPicker({ value, onChange, agentId, device }: ConfigEditorProps) {
  const [speakers, setSpeakers] = useState<Speaker[]>([]);
  const [open, setOpen] = useState(false);
  const [choices, setChoices] = useState<VPNChoice[]>();
  const [error, setError] = useState<string>();

  useEffect(() => {
    let live = true;
    client(agentId)
      .listProtocols({}, { timeoutMs: 4000 })
      .then((r) => {
        if (!live) return;
        const out: Speaker[] = [];
        for (const p of r.protocols) {
          if (p.type !== "bgp" || p.device !== device) continue;
          const cfg = (p.config ?? {}) as unknown as BGPConfig;
          const peers = (cfg.peers ?? []).filter((x) => x.families?.some(isVPN)).map((x) => x.address);
          if (peers.length) out.push({ name: p.name, peers, own: cfg.routes ?? [] });
        }
        setSpeakers(out);
      })
      .catch(() => setSpeakers([]));
    return () => {
      live = false;
    };
  }, [agentId, device]);

  useEffect(() => {
    if (!open) return;
    let live = true;
    setChoices(undefined);
    setError(undefined);
    const asks = speakers.flatMap((s) =>
      s.peers.map((peer) => client(agentId).runProtocolAction({ name: s.name, action: "rib", args: { peer, limit: 100000 } })),
    );
    Promise.all(asks)
      .then((rs) => {
        if (live) setChoices(vpnChoices(rs.flatMap((r) => (r.result as { routes?: Route[] } | undefined)?.routes ?? [])));
      })
      .catch((e) => live && setError(errorText(e)));
    return () => {
      live = false;
    };
  }, [open, agentId, speakers]);

  if (!speakers.length && !value.route_target) return null;

  // A source in the VPN: the first of our routes with this route target.
  const ownSource = (c: VPNChoice) => {
    for (const s of speakers) {
      for (const r of s.own) {
        if (r.rd && r.route_targets?.includes(c.target) && r.prefix.includes(":") === c.v6) return firstHost(r.prefix);
      }
    }
    return undefined;
  };
  const use = (c: VPNChoice) => {
    const { destination: _drop, ...rest } = value;
    const source = ownSource(c) ?? (value.source as string | undefined);
    onChange({ ...rest, route_target: c.target, destinations: compressPrefixes(c.prefixes), ...(source ? { source } : {}) });
    setOpen(false);
  };
  const leave = () => {
    const { route_target: _drop, ...rest } = value;
    onChange(rest);
  };

  return (
    <div className="route-picker">
      <div className="btn-row">
        <button type="button" className="btn small" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
          In a VPN…
        </button>
        {!!value.route_target && (
          <>
            <span className="muted">
              Route target <span className="mono">{value.route_target as string}</span>
            </span>
            <button type="button" className="btn small" onClick={leave}>
              Not in a VPN
            </button>
          </>
        )}
      </div>
      {open && (
        <div className="route-picker-body">
          <ErrorBox error={error} />
          {!choices && !error && <div className="muted">Loading received routes…</div>}
          {choices?.length === 0 && <div className="muted">No VPN routes received yet.</div>}
          {!!choices?.length && (
            <div className="route-picker-list">
              {choices.map((c) => (
                <button type="button" key={c.key} className="btn small vpn-choice" onClick={() => use(c)}>
                  <span className="mono">{c.target}</span> · {c.prefixes.length.toLocaleString()} IPv{c.v6 ? 6 : 4} route{c.prefixes.length === 1 ? "" : "s"}
                </button>
              ))}
            </div>
          )}
          <span className="muted">The flow looks up each packet's label in the live routes. Only the prefixes are copied into it.</span>
        </div>
      )}
    </div>
  );
}
