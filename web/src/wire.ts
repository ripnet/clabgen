// What monitor instances saw on mirrored links (internal/wire), and flows'
// wire expectations judged against it. The judging mirrors wire.Judge in Go
// (clabgenctl wire); keep them alike.
import { client, usePoll } from "./api";
import { agentLabel, useAgents } from "./agents";

export type Seen = {
  flow_id?: string;
  kind: string;
  reply?: boolean;
  level?: number;
  mep_id?: number;
  ma?: string;
  tags: string;
  src?: string;
  dst?: string;
  ip_src?: string;
  ip_dst?: string;
  count: number;
  first_s?: number;
  last_s: number;
};
export type Service = {
  label?: number;
  transport?: number[];
  flow_label?: boolean;
  entropy?: boolean;
  control_word?: boolean;
  ip?: boolean;
  src: string;
  dst: string;
  outer_tags?: string;
  frames: number;
  last_s: number;
  data?: Seen[];
  oam?: Seen[];
  other?: Seen[];
};
export type MonitorStatus = {
  direction?: string;
  frames: number;
  mpls: number;
  undecoded: number;
  links: { src: string; dst: string; frames: number; last_s: number }[];
  services: Service[];
  truncated?: boolean;
};
export type Expect = { data?: string; oam?: string; direction?: string };
export type MonitorAt = { agentId: string; agent: string; name: string; status: MonitorStatus };

// useMonitors polls every monitor instance on every known agent.
export function useMonitors(): { list: MonitorAt[]; loading: boolean } {
  const { agents, info } = useAgents();
  const key = agents.map((a) => a.id).join(",");
  const p = usePoll(
    async () => {
      const res = await Promise.allSettled(agents.map((a) => client(a.id).listProtocols({ withStatus: true, brief: true }, { timeoutMs: 3000 })));
      const out: MonitorAt[] = [];
      res.forEach((r, i) => {
        if (r.status !== "fulfilled") return;
        for (const pr of r.value.protocols)
          if (pr.type === "monitor" && pr.status)
            out.push({ agentId: agents[i].id, agent: agentLabel(agents[i], info[agents[i].id]), name: pr.name, status: pr.status as unknown as MonitorStatus });
      });
      return out;
    },
    [key],
    2000,
  );
  return { list: p.data ?? [], loading: p.loading };
}

// clearMonitors clears every monitor: what they saw so far, for a fresh
// verdict after a change.
export async function clearMonitors(mons: MonitorAt[]) {
  await Promise.all(mons.map((m) => client(m.agentId).runProtocolAction({ name: m.name, action: "clear" })));
}

// ---- stacks ----

const TPIDS: Record<string, number> = { "8100": 0x8100, "88a8": 0x88a8, "9100": 0x9100 };

// normalStack writes a stack the way monitors report it: "3162,163",
// "0x88a8:3162,163", "untagged"; "" when empty, null when invalid.
export function normalStack(s: string | undefined): string | null {
  const t = (s ?? "").trim();
  if (!t) return "";
  if (t.toLowerCase() === "untagged" || t === "-") return "untagged";
  const out: string[] = [];
  for (const p of t.split(/[,\s.]+/).filter(Boolean)) {
    let vid = p;
    let tpid = 0x8100;
    const i = p.indexOf(":");
    if (i >= 0) {
      const tp = TPIDS[p.slice(0, i).toLowerCase().replace(/^0x/, "")];
      if (!tp) return null;
      tpid = tp;
      vid = p.slice(i + 1);
    }
    if (!/^\d+$/.test(vid) || Number(vid) > 4095) return null;
    out.push(tpid === 0x8100 ? String(Number(vid)) : `0x${tpid.toString(16)}:${Number(vid)}`);
  }
  return out.join(",");
}

// ---- judging ----

// stale: not seen for STALE_AFTER s while another stack of its class is,
// so from before a change; shown, not judged.
export type StackCount = { tags: string; count: number; last_s: number; stale?: boolean };
export type Class = { expect: string; seen: StackCount[]; verdict: string };
export type Row = {
  direction: string;
  monitor?: MonitorAt;
  service?: Service;
  data: Class;
  oam: Class;
  verdict: string;
};
export type WireVerdict = { verdict: string; rows: Row[] };

export const STALE_AFTER = 10; // seconds

const add = (l: StackCount[], tags: string, n: number, last: number) => {
  const x = l.find((s) => s.tags === tags);
  if (x) {
    x.count += n;
    x.last_s = Math.min(x.last_s, last);
  } else l.push({ tags, count: n, last_s: last });
};

function markStale(l: StackCount[]) {
  const fresh = l.some((s) => s.last_s <= STALE_AFTER);
  for (const s of l) s.stale = fresh && s.last_s > STALE_AFTER;
}

// since: OAM seen since the flow first was (ages in seconds); from before,
// it's another service's (the router reused the label). An unknown first
// sighting (0) takes everything.
const since = (o: Seen, first: number) => !first || o.last_s <= first;

function judgeClass(c: Class) {
  markStale(c.seen);
  if (!c.expect) return;
  c.verdict = c.seen.length === 0 ? "pending" : c.seen.every((s) => s.tags === c.expect || s.stale) ? "pass" : "fail";
}

function rowVerdict(r: Row) {
  let v = "";
  for (const c of [r.data, r.oam]) {
    if (!c.verdict) continue;
    if (c.verdict === "fail") v = "fail";
    else if (c.verdict === "pending" && v !== "fail") v = "pending";
    else if (!v) v = "pass";
  }
  r.verdict = v;
}

// judge checks a flow (its ID, the MAC or IP it sends from) against the
// monitors: per service and direction its frames were seen in.
export function judge(flowId: string, from: { mac?: string; ip?: string }, expect: Expect | undefined, mons: MonitorAt[]): WireVerdict {
  const wantData = normalStack(expect?.data) ?? "";
  const wantOAM = normalStack(expect?.oam) ?? "";
  const dirs = expect?.direction === "a-to-z" || expect?.direction === "z-to-a" ? [expect.direction] : ["a-to-z", "z-to-a"];
  const dirOf = (s: Seen) =>
    (from.mac && s.src === from.mac) || (from.ip && s.ip_src === from.ip)
      ? "a-to-z"
      : (from.mac && s.dst === from.mac) || (from.ip && s.ip_dst === from.ip) || s.reply
        ? "z-to-a"
        : "a-to-z";
  const rows: Row[] = [];
  const seenDir = new Set<string>();
  for (const m of mons) {
    const monDir = new Set<string>();
    const mas = new Set<string>(); // MAs of the OAM in the flow's services
    const svcs = new Set<Service>(); // services the flow was seen in
    for (const svc of m.status.services) {
      const by = new Map<string, Row>();
      const first = new Map<string, number>(); // when the flow was first seen, per direction
      for (const s of svc.data ?? []) {
        if (s.flow_id !== flowId) continue;
        const d = dirOf(s);
        let r = by.get(d);
        if (!r) {
          const checked = dirs.includes(d);
          r = { direction: d, monitor: m, service: svc, data: { expect: checked ? wantData : "", seen: [], verdict: "" }, oam: { expect: checked ? wantOAM : "", seen: [], verdict: "" }, verdict: "" };
          by.set(d, r);
        }
        add(r.data.seen, s.tags, s.count, s.last_s);
        first.set(d, Math.max(first.get(d) ?? 0, s.first_s ?? 0));
      }
      for (const r of by.values()) {
        for (const o of svc.oam ?? [])
          if (since(o, first.get(r.direction) ?? 0)) {
            add(r.oam.seen, o.tags, o.count, o.last_s);
            if (o.ma) mas.add(o.ma);
          }
        judgeClass(r.data);
        judgeClass(r.oam);
        rowVerdict(r);
        seenDir.add(r.direction);
        monDir.add(r.direction);
        svcs.add(svc);
        rows.push(r);
      }
    }
    // A direction without the flow's frames: its OAM is in the service the
    // other way round, carrying the same MAs.
    for (const d of dirs) {
      if (monDir.has(d) || !mas.size) continue;
      for (const svc of m.status.services) {
        if (svcs.has(svc) || svc.label === undefined || ![...svcs].some((o) => o.src === svc.dst && o.dst === svc.src)) continue;
        const r: Row = { direction: d, monitor: m, service: svc, data: { expect: wantData, seen: [], verdict: "" }, oam: { expect: wantOAM, seen: [], verdict: "" }, verdict: "" };
        for (const o of svc.oam ?? []) if (o.ma && mas.has(o.ma)) add(r.oam.seen, o.tags, o.count, o.last_s);
        if (!r.oam.seen.length) continue;
        judgeClass(r.data);
        judgeClass(r.oam);
        rowVerdict(r);
        seenDir.add(d);
        rows.push(r);
      }
    }
  }
  for (const d of dirs)
    if (!seenDir.has(d))
      rows.push({
        direction: d,
        data: { expect: wantData, seen: [], verdict: wantData ? "pending" : "" },
        oam: { expect: wantOAM, seen: [], verdict: wantOAM ? "pending" : "" },
        verdict: "pending",
      });
  rows.sort((a, b) => a.direction.localeCompare(b.direction) || (a.monitor?.name ?? "").localeCompare(b.monitor?.name ?? ""));
  let verdict = wantData || wantOAM ? "pass" : "";
  if (verdict)
    for (const r of rows) {
      if (r.verdict === "fail") verdict = "fail";
      else if (r.verdict === "pending" && verdict !== "fail") verdict = "pending";
    }
  return { verdict, rows };
}

// stackParts splits a stack for drawing: each tag, TPID shown when not 0x8100.
export const stackParts = (tags: string) => (tags === "untagged" || !tags ? [] : tags.split(","));

// flowSource: where a flow sends from, by its status's source: a MAC (l2
// streams, maybe "×N") or an address.
export function flowSource(src?: string): { mac?: string; ip?: string } {
  const first = (src ?? "").split(" ")[0];
  return /^([0-9a-f]{2}:){5}[0-9a-f]{2}$/i.test(first) ? { mac: first.toLowerCase() } : { ip: first };
}

// judgeStatus judges a flow from its status (its flow ID, source and wire
// expectation); undefined for a flow without an ID.
export function judgeStatus(status: Record<string, unknown> | undefined, mons: MonitorAt[]): WireVerdict | undefined {
  const id = status?.flow_id as string | undefined;
  if (!id) return undefined;
  return judge(id, flowSource(status?.source as string | undefined), status?.wire as Expect | undefined, mons);
}

// DirSummary is one direction of a flow over every monitor that saw it.
export type DirSummary = { data: Class; oam: Class };

const worst = (a: string, b: string) => (a === "fail" || b === "fail" ? "fail" : a === "pending" || b === "pending" ? "pending" : a || b);

export function byDirection(v: WireVerdict): Record<"a-to-z" | "z-to-a", DirSummary> {
  const empty = (): DirSummary => ({ data: { expect: "", seen: [], verdict: "" }, oam: { expect: "", seen: [], verdict: "" } });
  const out = { "a-to-z": empty(), "z-to-a": empty() };
  for (const r of v.rows) {
    const d = out[r.direction as "a-to-z" | "z-to-a"];
    if (!d) continue;
    for (const [to, from] of [
      [d.data, r.data],
      [d.oam, r.oam],
    ] as const) {
      to.expect ||= from.expect;
      for (const s of from.seen) add(to.seen, s.tags, s.count, s.last_s);
      markStale(to.seen);
      to.verdict = worst(to.verdict, from.verdict);
    }
  }
  return out;
}

// combined is a flow's overall result from its checks and its wire verdict.
export function combined(checks?: string, wire?: string): string | undefined {
  const all = [checks, wire].filter((x): x is string => !!x);
  if (!all.length) return undefined;
  return all.includes("fail") ? "fail" : all.includes("pending") ? "pending" : "pass";
}
