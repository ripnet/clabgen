import { useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { useFlowIndex } from "../components/FlowViews";
import { Layout, Tags } from "../components/Wire";
import { href } from "../router";
import type { MonitorStatus, Seen, Service } from "../wire";
import { registerView, type Summary } from "./registry";

const as = (s: JsonObject) => s as unknown as MonitorStatus;
const age = (s: number) => (s < 1 ? "now" : s < 120 ? `${s.toFixed(0)} s ago` : `${(s / 60).toFixed(0)} min ago`);

function summary(st: JsonObject): Summary {
  const s = as(st);
  const mpls = s.services.filter((x) => x.label !== undefined).length;
  const flows = new Set(s.services.flatMap((x) => (x.data ?? []).map((d) => d.flow_id))).size;
  if (!s.frames) return { health: "idle", text: "no frames yet" };
  return { health: "ok", text: `${s.frames.toLocaleString()} frames · ${mpls} services · ${flows} flow${flows === 1 ? "" : "s"}${s.direction ? ` · ${s.direction}` : ""}` };
}

const oamText = (o: Seen) => [o.kind, o.level !== undefined ? `level ${o.level}` : "", o.ma ?? "", o.mep_id !== undefined ? `MEP ${o.mep_id}` : ""].filter(Boolean).join(" · ");

function Flows({ s, index }: { s: Service; index: Map<string, { agentId: string; agent: string; name: string }> }) {
  if (!s.data?.length) return <span className="muted">—</span>;
  return (
    <div className="wire-list">
      {s.data.map((d, i) => {
        const f = d.flow_id ? index.get(d.flow_id) : undefined;
        return (
          <div key={i}>
            {f ? (
              <a className="mono" href={href({ page: "protocol", agent: f.agentId, protocol: f.name })}>
                {f.agent}/{f.name}
              </a>
            ) : (
              <span className="mono muted">{d.flow_id}</span>
            )}{" "}
            <span className="muted small-note">
              {d.kind}
              {d.reply ? " reply" : ""}
            </span>{" "}
            {d.tags ? <Tags tags={d.tags} /> : null} <span className="muted small-note">×{d.count.toLocaleString()}</span>
          </div>
        );
      })}
    </div>
  );
}

function OAM({ s }: { s: Service }) {
  if (!s.oam?.length) return <span className="muted">—</span>;
  return (
    <div className="wire-list">
      {s.oam.map((o, i) => (
        <div key={i}>
          <span className="small-note">{oamText(o)}</span> <Tags tags={o.tags} /> <span className="muted small-note">×{o.count.toLocaleString()}</span>
        </div>
      ))}
    </div>
  );
}

function Status({ status }: { status: JsonObject }) {
  const s = as(status);
  const index = useFlowIndex();
  const [filter, setFilter] = useState("");
  const [plain, setPlain] = useState(false);
  const linkNo = new Map(s.links.map((l, i) => [`${l.src}>${l.dst}`, i + 1]));
  const match = (x: Service) => {
    if (!plain && x.label === undefined && !x.data?.length && !x.oam?.length) return false;
    const q = filter.trim().toLowerCase();
    if (!q) return true;
    const names = (x.data ?? []).map((d) => (d.flow_id ? (index.get(d.flow_id)?.name ?? d.flow_id) : ""));
    const hay = [String(x.label ?? ""), x.src, x.dst, ...names, ...(x.data ?? []).map((d) => d.tags), ...(x.oam ?? []).map((o) => `${o.ma ?? ""} ${o.tags}`)].join(" ").toLowerCase();
    return hay.includes(q);
  };
  const shown = s.services.filter(match);
  return (
    <div>
      <dl className="kv">
        <div className="kv-row">
          <dt>frames</dt>
          <dd className="mono">
            {s.frames.toLocaleString()} ({s.mpls.toLocaleString()} MPLS{s.undecoded ? `, ${s.undecoded.toLocaleString()} not Ethernet` : ""})
          </dd>
        </div>
        <div className="kv-row">
          <dt>mirror</dt>
          <dd>{s.direction ? `${s.direction} only` : "both directions, told apart by MAC address"}</dd>
        </div>
      </dl>
      <h4>Links</h4>
      {s.links.length === 0 ? (
        <div className="muted">Nothing received yet. The port should get a router's port mirror of the link to watch.</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense">
            <thead>
              <tr>
                <th>#</th>
                <th>From → to</th>
                <th className="num">Frames</th>
                <th className="num">Last</th>
              </tr>
            </thead>
            <tbody>
              {s.links.map((l, i) => (
                <tr key={i}>
                  <td className="mono">{i + 1}</td>
                  <td className="mono">
                    {l.src} → {l.dst}
                  </td>
                  <td className="num mono">{l.frames.toLocaleString()}</td>
                  <td className="num">{age(l.last_s)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <h4>Services</h4>
      <div className="wire-filter">
        <input className="input" placeholder="Filter: label, flow, MA, tags…" value={filter} onChange={(e) => setFilter(e.target.value)} aria-label="Filter services" />
        <label className="muted small-note">
          <input type="checkbox" checked={plain} onChange={(e) => setPlain(e.target.checked)} /> plain Ethernet on the link (routing protocols…)
        </label>
      </div>
      {shown.length === 0 ? (
        <div className="muted">{s.services.length ? "None match." : "None yet."}</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense wire-table">
            <thead>
              <tr>
                <th>Service</th>
                <th>Flows</th>
                <th>OAM</th>
                <th>Other</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((x) => (
                <tr key={`${x.label ?? "-"} ${x.src}>${x.dst} ${x.outer_tags ?? ""}`}>
                  <td>
                    <div className="mono">
                      {x.label !== undefined ? <>label {x.label}</> : <>plain{x.outer_tags ? ` · ${x.outer_tags}` : ""}</>}
                      {x.label !== undefined && <span className="muted small-note"> · link {linkNo.get(`${x.src}>${x.dst}`) ?? "?"}</span>}
                    </div>
                    <Layout s={x} tags={x.data?.[0]?.tags ?? x.oam?.[0]?.tags} />
                    <div className="muted small-note">
                      {x.frames.toLocaleString()} frames · {age(x.last_s)}
                    </div>
                  </td>
                  <td>
                    <Flows s={x} index={index} />
                  </td>
                  <td>
                    <OAM s={x} />
                  </td>
                  <td className="small-note">{x.other?.length ? x.other.map((o) => `${o.kind} ×${o.count.toLocaleString()}`).join(", ") : <span className="muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {s.truncated && <p className="muted small-note">More services or entries than are kept: the rest are only counted in frames. Clear to start over.</p>}
    </div>
  );
}

registerView("monitor", { summary, Status });
