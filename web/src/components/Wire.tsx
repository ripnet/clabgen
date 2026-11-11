import { href } from "../router";
import { byDirection, judge, stackParts, useMonitors, type Class, type Expect, type Service, type WireVerdict } from "../wire";

const dirText: Record<string, string> = { "a-to-z": "from this flow's device", "z-to-a": "back (replies)" };

// Tags draws a VLAN stack as chips.
export function Tags({ tags }: { tags: string }) {
  const parts = stackParts(tags);
  if (!parts.length) return <span className="wire-tag wire-untagged">untagged</span>;
  return (
    <span className="wire-tags">
      {parts.map((t, i) => (
        <span key={i} className="wire-tag">
          {t.includes(":") ? (
            <>
              <span className="wire-tpid">{t.split(":")[0]}</span> {t.split(":")[1]}
            </>
          ) : (
            t
          )}
        </span>
      ))}
    </span>
  );
}

// Layout draws a service's frame: outer Ethernet, labels, control word, the
// frame inside (its tags).
export function Layout({ s, tags }: { s: Service; tags?: string }) {
  return (
    <div className="wire-frame" aria-label="Frame on the monitored link">
      <span className="ms-seg ms-clear">DA · SA{s.outer_tags ? ` · ${s.outer_tags}` : ""}</span>
      {s.label !== undefined ? (
        <>
          {(s.transport ?? []).map((l, i) => (
            <span key={i} className="ms-seg wire-transport" title="transport label (changes with the path)">
              {l}
            </span>
          ))}
          {s.entropy && <span className="ms-seg wire-transport">ELI · EL</span>}
          <span className="ms-seg ms-tag" title="service label: identifies the pseudowire in this direction">
            {s.label}
          </span>
          {s.flow_label && <span className="ms-seg wire-transport" title="flow label (FAT, RFC 6391)">FAT</span>}
          {s.control_word && <span className="ms-seg wire-transport">CW</span>}
        </>
      ) : null}
      {s.ip ? (
        <span className="ms-seg ms-enc">IP</span>
      ) : (
        <span className="ms-seg ms-enc">
          {s.label !== undefined ? "DA · SA · " : ""}
          {tags !== undefined && tags !== "untagged" ? `${tags} · ` : ""}
          data
        </span>
      )}
    </div>
  );
}

function ClassCell({ c }: { c: Class }) {
  if (!c.seen.length)
    return c.expect ? (
      <span className="muted">
        none yet · want <Tags tags={c.expect} />
      </span>
    ) : (
      <span className="muted">—</span>
    );
  return (
    <span className="wire-class">
      {c.seen.map((s) => (
        <span key={s.tags} className={`wire-seen ${c.expect ? (s.tags === c.expect ? "ok" : "bad") : ""}`}>
          <Tags tags={s.tags} /> <span className="muted small-note">×{s.count.toLocaleString()}</span>
        </span>
      ))}
      {c.verdict === "fail" && (
        <span className="muted small-note">
          want <Tags tags={c.expect} />
        </span>
      )}
    </span>
  );
}

// WireSection is a flow on the monitored links: per direction, the service it
// was seen in and the VLAN tags of its frames and of the OAM there, checked
// against its wire expectation.
export function WireSection({ flowId, from, expect }: { flowId: string; from: { mac?: string; ip?: string }; expect?: Expect }) {
  const { list, loading } = useMonitors();
  if (!list.length && !expect) return null;
  const v = judge(flowId, from, expect, list);
  const seen = v.rows.some((r) => r.monitor);
  if (!seen && !expect) return null;
  return (
    <div className="wire-section">
      <h4 className="wire-head">
        Wire {v.verdict && <span className={`verdict ${v.verdict}`}>{v.verdict}</span>}
      </h4>
      {expect && (
        <p className="muted small-note">
          Expected inside the service: data {expect.data ? <Tags tags={expect.data} /> : "not checked"}, OAM {expect.oam ? <Tags tags={expect.oam} /> : "not checked"}
          {expect.direction && expect.direction !== "both" ? `, ${expect.direction} only` : ", both directions"}.
        </p>
      )}
      {!list.length ? (
        <div className="muted">{loading ? "Looking for monitors…" : "No monitor instance on any agent: add one on a port that gets a router's port mirror."}</div>
      ) : (
        <div className="table-wrap">
          <table className="table dense wire-table">
            <thead>
              <tr>
                <th>Direction</th>
                <th>Seen at</th>
                <th>Data</th>
                <th>OAM in the service</th>
                <th>Result</th>
              </tr>
            </thead>
            <tbody>
              {v.rows.map((r, i) => (
                <tr key={i}>
                  <td>
                    {r.direction}
                    <div className="muted small-note">{dirText[r.direction]}</div>
                  </td>
                  <td>
                    {r.monitor && r.service ? (
                      <>
                        <a className="mono" href={href({ page: "protocol", agent: r.monitor.agentId, protocol: r.monitor.name })}>
                          {r.monitor.agent}/{r.monitor.name}
                        </a>
                        <Layout s={r.service} tags={r.data.seen[0]?.tags} />
                        {r.service.label !== undefined && (
                          <div className="muted small-note mono">
                            {r.service.src} → {r.service.dst}
                          </div>
                        )}
                      </>
                    ) : (
                      <span className="muted">not seen</span>
                    )}
                  </td>
                  <td>
                    <ClassCell c={r.data} />
                  </td>
                  <td>
                    <ClassCell c={r.oam} />
                  </td>
                  <td>{r.verdict ? <span className={`verdict ${r.verdict}`}>{r.verdict}</span> : <span className="muted">—</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function Stacks({ c }: { c: Class }) {
  if (!c.seen.length)
    return (
      <span className={`wire-none ${c.expect ? "warn" : ""}`} title={c.expect ? `none seen yet; want ${c.expect}` : "none seen"}>
        none
      </span>
    );
  return (
    <>
      {c.seen.map((s) => (
        <span key={s.tags} className={`wire-seen ${c.expect ? (s.tags === c.expect ? "ok" : "bad") : ""}`} title={c.expect && s.tags !== c.expect ? `want ${c.expect}` : `×${s.count}`}>
          <Tags tags={s.tags} />
        </span>
      ))}
    </>
  );
}

// WireCell is a flow's wire in a list: the tags of its data and of its
// service's OAM, each way, red where they aren't what it expects.
export function WireCell({ v }: { v?: WireVerdict }) {
  if (!v || (!v.verdict && !v.rows.some((r) => r.monitor))) return <span className="muted">—</span>;
  const d = byDirection(v);
  return (
    <div className="wire-cell">
      {(["data", "oam"] as const).map((k) => (
        <div key={k} className="wire-cell-line">
          <span className="wire-k">{k === "data" ? "Data" : "OAM"}</span>
          <span className="wire-dir" title="from the flow's device">→</span>
          <Stacks c={d["a-to-z"][k]} />
          <span className="wire-dir" title="back">←</span>
          <Stacks c={d["z-to-a"][k]} />
        </div>
      ))}
    </div>
  );
}
