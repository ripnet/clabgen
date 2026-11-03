import { useEffect, useState } from "react";
import { client, errorText } from "../api";
import { agentLabel, useAgents } from "../agents";
import type { ConfigEditorProps } from "../protocols/registry";
import { ErrorBox } from "../ui";

type Block = { prefix: string; count: number };
type Advertised = Block & { key: string; agent: string; speaker: string; nextHop?: string };
type Speaker = { agentId: string; agent: string; name: string; peers: string[] };
type BGPConfig = { routes?: { prefix: string; count?: number; next_hop?: string; rd?: string }[]; peers?: { address: string }[] };

const v4Num = (a: string) => a.split(".").reduce((n, x) => n * 256 + Number(x), 0);
const v4Str = (n: number) => [24, 16, 8, 0].map((s) => Math.floor(n / 2 ** s) % 256).join(".");

// compressPrefixes turns a list of prefixes into blocks of consecutive ones
// of one length. Only IPv4 is compressed; each IPv6 prefix is its own block.
export function compressPrefixes(list: string[]): Block[] {
  const out: Block[] = [];
  let last: { len: number; num: number; block: Block } | undefined;
  const v4 = list.filter((p) => !p.includes(":"));
  const parsed = v4.map((p) => ({ p, num: v4Num(p.split("/")[0]), len: Number(p.split("/")[1]) })).sort((a, b) => a.len - b.len || a.num - b.num);
  for (const x of parsed) {
    if (last && last.len === x.len && last.num + 2 ** (32 - x.len) === x.num) {
      last.block.count++;
      last.num = x.num;
      continue;
    }
    const block = { prefix: `${v4Str(x.num)}/${x.len}`, count: 1 };
    out.push(block);
    last = { len: x.len, num: x.num, block };
  }
  for (const p of list) if (p.includes(":")) out.push({ prefix: p, count: 1 });
  return out;
}

const blockText = (b: Block) => `${b.prefix}${b.count > 1 ? ` ×${b.count.toLocaleString()}` : ""}`;

// RoutePicker fills a flow's destinations from BGP routes: those any agent
// advertises, or those a speaker received from a peer. It copies them: later
// changes to the route don't reach the flow, and a withdrawn route keeps
// getting traffic, which is how a black hole shows.
export function RoutePicker({ value, onChange }: ConfigEditorProps) {
  const { agents, info } = useAgents();
  const [open, setOpen] = useState(false);
  const [source, setSource] = useState<"advertised" | "received">("advertised");
  const [advertised, setAdvertised] = useState<Advertised[]>([]);
  const [speakers, setSpeakers] = useState<Speaker[]>([]);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [speaker, setSpeaker] = useState("");
  const [peer, setPeer] = useState("");
  const [received, setReceived] = useState<Block[]>();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!open) return;
    let live = true;
    Promise.allSettled(agents.map((a) => client(a.id).listProtocols({}, { timeoutMs: 4000 }).then((r) => ({ a, r })))).then((got) => {
      if (!live) return;
      const adv: Advertised[] = [];
      const sp: Speaker[] = [];
      for (const g of got) {
        if (g.status !== "fulfilled") continue;
        const agent = agentLabel(g.value.a, info[g.value.a.id]);
        for (const p of g.value.r.protocols.filter((x) => x.type === "bgp")) {
          const cfg = (p.config ?? {}) as unknown as BGPConfig;
          sp.push({ agentId: g.value.a.id, agent, name: p.name, peers: (cfg.peers ?? []).map((x) => x.address) });
          (cfg.routes ?? [])
            .filter((r) => !r.rd)
            .forEach((r, i) => adv.push({ key: `${g.value.a.id}/${p.name}/${i}`, agent, speaker: p.name, prefix: r.prefix, count: r.count || 1, nextHop: r.next_hop }));
        }
      }
      setAdvertised(adv);
      setSpeakers(sp);
    });
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, agents.map((a) => a.id).join("|")]);

  const chosen = speakers.find((s) => `${s.agentId}/${s.name}` === speaker);
  const load = async () => {
    if (!chosen || !peer) return;
    setBusy(true);
    setError(undefined);
    try {
      const r = await client(chosen.agentId).runProtocolAction({ name: chosen.name, action: "rib", args: { peer, limit: 100000 } });
      const routes = ((r.result as { routes?: { prefix: string; family: string }[] } | undefined)?.routes ?? []).filter((x) => x.family === "ipv4-unicast" || x.family === "ipv6-unicast");
      setReceived(compressPrefixes(routes.map((x) => x.prefix)));
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  const toggle = (k: string) =>
    setPicked((s) => {
      const n = new Set(s);
      if (!n.delete(k)) n.add(k);
      return n;
    });

  const rows: { key: string; text: string; from: string; note?: string; block: Block }[] =
    source === "advertised"
      ? advertised.map((r) => ({ key: r.key, text: blockText(r), from: `${r.agent} · ${r.speaker}`, note: r.nextHop ? `next hop ${r.nextHop}: not answered by clabgen` : undefined, block: { prefix: r.prefix, count: r.count } }))
      : (received ?? []).map((b) => ({ key: `rx/${b.prefix}`, text: blockText(b), from: `${chosen?.agent} · ${chosen?.name} · from ${peer}`, block: b }));
  const blocks = rows.filter((r) => picked.has(r.key)).map((r) => r.block);
  const total = blocks.reduce((n, b) => n + b.count, 0);
  const apply = () => {
    const { destination: _drop, ...rest } = value;
    onChange({ ...rest, destinations: blocks });
    setOpen(false);
    setPicked(new Set());
  };

  return (
    <div className="route-picker">
      <button type="button" className="btn small" aria-expanded={open} onClick={() => setOpen((o) => !o)}>
        From BGP routes…
      </button>
      {open && (
        <div className="route-picker-body">
          <div className="seg" role="tablist">
            {(["advertised", "received"] as const).map((s) => (
              <button type="button" key={s} role="tab" aria-selected={source === s} className={`seg-btn ${source === s ? "active" : ""}`} onClick={() => setSource(s)}>
                {s}
              </button>
            ))}
          </div>
          {source === "received" && (
            <div className="btn-row">
              <select className="input" aria-label="BGP speaker" value={speaker} onChange={(e) => (setSpeaker(e.target.value), setPeer(""), setReceived(undefined))}>
                <option value="">Speaker</option>
                {speakers.map((s) => (
                  <option key={s.agentId + s.name} value={`${s.agentId}/${s.name}`}>
                    {s.agent} · {s.name}
                  </option>
                ))}
              </select>
              <select className="input" aria-label="BGP peer" value={peer} onChange={(e) => setPeer(e.target.value)}>
                <option value="">Peer</option>
                {chosen?.peers.map((p) => <option key={p}>{p}</option>)}
              </select>
              <button type="button" className="btn small" disabled={!peer || busy} onClick={load}>
                Load routes
              </button>
            </div>
          )}
          <ErrorBox error={error} />
          {rows.length === 0 ? (
            <div className="muted">{source === "advertised" ? "No agent advertises BGP routes." : received ? "No routes received from that peer." : "Pick a speaker and a peer."}</div>
          ) : (
            <div className="route-picker-list">
              {rows.map((r) => (
                <label key={r.key} className="route-picker-row">
                  <input type="checkbox" checked={picked.has(r.key)} onChange={() => toggle(r.key)} />
                  <span className="mono">{r.text}</span>
                  <span className="muted">
                    {r.from}
                    {r.note ? ` · ${r.note}` : ""}
                  </span>
                </label>
              ))}
            </div>
          )}
          <div className="btn-row">
            <button type="button" className="btn small primary" disabled={!total} onClick={apply}>
              Use {total.toLocaleString()} prefix{total === 1 ? "" : "es"}
            </button>
            <span className="muted">Copied into this flow: later changes to the routes won't reach it.</span>
          </div>
        </div>
      )}
    </div>
  );
}
