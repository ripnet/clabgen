import { useEffect, useMemo, useRef, useState } from "react";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { client, errorText, usePoll } from "../api";
import { useAgents } from "../agents";
import { displayVersion } from "../version";
import type { CapturedFrame } from "../gen/clabgen/v1/agent_pb";
import { PacketHex, type Preview } from "../components/PacketHex";
import { pcapng } from "../pcapng";
import { CreateProtocol, type CreatePreset } from "./ProtocolsTab";
import { copyText, useToast } from "../ui";

// Packet capture: the frames the agent's ports receive and send, live.

const MAX = 2000; // frames kept in the page
const ETHERTYPES: { label: string; v: number[] }[] = [
  { label: "all", v: [] },
  { label: "IPv4", v: [0x0800] },
  { label: "IPv6", v: [0x86dd] },
  { label: "IP (v4 and v6)", v: [0x0800, 0x86dd] },
  { label: "ARP", v: [0x0806] },
  { label: "CFM", v: [0x8902] },
  { label: "LLDP", v: [0x88cc] },
  { label: "MPLS", v: [0x8847] },
];

type Row = { n: number; f: CapturedFrame; t: Date };

const hms = (t: Date) => t.toLocaleTimeString(undefined, { hour12: false }) + "." + String(t.getMilliseconds()).padStart(3, "0");

export function CaptureTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const ports = usePoll(() => c.listPorts({}), [agentId]);
  const devices = usePoll(() => c.listDevices({}), [agentId]);
  const [sel, setSel] = useState<string[]>([]); // ports; empty: all
  const [device, setDevice] = useState("");
  const [direction, setDirection] = useState("");
  const [et, setEt] = useState("all");
  const [running, setRunning] = useState(false);
  const [rows, setRows] = useState<Row[]>([]);
  const [dropped, setDropped] = useState(0);
  const [error, setError] = useState<string>();
  const [filter, setFilter] = useState("");
  const [follow, setFollow] = useState(true);
  const [picked, setPicked] = useState<Row>();
  const [detail, setDetail] = useState<Preview>();
  const [building, setBuilding] = useState<CreatePreset>();
  const ctl = useRef<AbortController>();
  const pending = useRef<Row[]>([]);
  const seq = useRef(0);
  const list = useRef<HTMLDivElement>(null);
  const toast = useToast();
  const { info, ui, agents } = useAgents();
  // Does the agent capture at all? (DecodeFrame came with capture.)
  const [unsupported, setUnsupported] = useState(false);
  useEffect(() => {
    setUnsupported(false);
    c.decodeFrame({ data: new Uint8Array() }).catch((e) => {
      if (e instanceof ConnectError && e.code === Code.Unimplemented) setUnsupported(true);
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId]);
  // Agents from before packet capture don't know the call.
  const why = (e: unknown) =>
    e instanceof ConnectError && e.code === Code.Unimplemented
      ? `This agent (${displayVersion(info[agentId]?.version) || "an older version"}) has no packet capture: redeploy its node with a newer clabgen image${ui?.version ? ` (this UI is ${displayVersion(ui.version)})` : ""}.`
      : errorText(e);

  // Frames arrive faster than React should render: flush a few times a second.
  useEffect(() => {
    const t = setInterval(() => {
      if (!pending.current.length) return;
      const add = pending.current;
      pending.current = [];
      setRows((r) => [...r, ...add].slice(-MAX));
    }, 200);
    return () => clearInterval(t);
  }, []);
  useEffect(() => () => ctl.current?.abort(), []);
  useEffect(() => {
    if (follow && list.current) list.current.scrollTop = list.current.scrollHeight;
  }, [rows, follow]);

  const request = () => ({
    ports: device ? [] : sel,
    device,
    direction,
    ethertypes: ETHERTYPES.find((x) => x.label === et)?.v ?? [],
    summaries: true,
  });

  const start = async () => {
    ctl.current?.abort();
    const a = new AbortController();
    ctl.current = a;
    setRows([]);
    setDropped(0);
    setError(undefined);
    setPicked(undefined);
    setDetail(undefined);
    pending.current = [];
    seq.current = 0;
    setRunning(true);
    try {
      for await (const f of c.capture(request(), { signal: a.signal })) {
        pending.current.push({ n: ++seq.current, f, t: f.time ? timestampDate(f.time) : new Date() });
        if (f.dropped) setDropped(Number(f.dropped));
      }
    } catch (e) {
      if (!a.signal.aborted) setError(why(e));
    }
    if (ctl.current === a) setRunning(false);
  };
  const stop = () => {
    ctl.current?.abort();
    setRunning(false);
  };

  const pick = async (r: Row) => {
    setPicked(r);
    setDetail(undefined);
    try {
      const d = await c.decodeFrame({ data: r.f.data });
      setDetail(d.result as unknown as Preview);
    } catch (e) {
      setError(why(e));
    }
  };

  // A packet flow that sends this frame again, from a device on its port:
  // the captured device tags dropped (the device adds its own), and the
  // source MAC too when it is that device's.
  const rebuild = () => {
    if (!picked || !detail?.decoded) return;
    const ls = structuredClone(detail.decoded) as Record<string, Record<string, unknown>>[];
    while (ls.length > 1 && ls[1].vlan) ls.splice(1, 1);
    const devs = (devices.data?.devices ?? []).filter((d) => d.port === picked.f.port);
    const dev = devs.find((d) => d.mac === ls[0].ethernet?.src) ?? devs[0];
    if (dev && ls[0].ethernet?.src === dev.mac) delete ls[0].ethernet.src;
    setBuilding({ type: "packet", device: dev?.name, config: { layers: ls, mode: "count", count: 1, rate: 1 }, title: "New flow from a captured frame" });
  };

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return q ? rows.filter((r) => `${r.f.port} ${r.f.summary}`.toLowerCase().includes(q)) : rows;
  }, [rows, filter]);

  const download = () => {
    const ns = (r: Row) => (r.f.time ? r.f.time.seconds * 1000000000n + BigInt(r.f.time.nanos) : BigInt(r.t.getTime()) * 1000000n);
    const blob = pcapng(rows.map((r) => ({ port: r.f.port, direction: r.f.direction, timeNs: ns(r), data: r.f.data, length: r.f.length })));
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = `clabgen-${agentId}-${new Date().toISOString().replace(/[:.]/g, "-")}.pcapng`;
    a.click();
    URL.revokeObjectURL(a.href);
  };

  // The same capture straight into Wireshark, through this UI.
  const wireshark = useMemo(() => {
    const q = new URLSearchParams();
    const r = request();
    r.ports.forEach((p) => q.append("port", p));
    if (r.device) q.set("device", r.device);
    if (r.direction) q.set("direction", r.direction);
    r.ethertypes.forEach((e) => q.append("ethertype", "0x" + e.toString(16).padStart(4, "0")));
    const base = new URL(`agents/${encodeURIComponent(agentId)}/capture.pcapng`, window.location.href);
    // -f: an error (an agent without capture) stops curl instead of feeding Wireshark.
    return `curl -sSfN '${base.href}${q.size ? "?" + q.toString() : ""}' | wireshark -k -i -`;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [agentId, sel, device, direction, et]);

  const portNames = (ports.data?.ports ?? []).map((p) => p.name);

  // Wireshark through Edgeshark, when the UI knows its packetflix: its
  // Wireshark plugin opens packetflix: links and captures the agent's
  // container on these ports. Only containers it discovered can be found.
  const packetflix = useMemo(() => {
    const es = ui?.edgeshark;
    const a = agents.find((x) => x.id === agentId);
    if (!es || !a || !(a.source === "docker" || a.source.startsWith("lab:"))) return undefined;
    let base = `ws://${window.location.hostname}:${es.port}`;
    if (es.url) {
      const u = new URL(es.url);
      base = `${u.protocol === "https:" ? "wss" : "ws"}://${u.host}`;
    }
    // A device: its port, and its MAC as the filter.
    const dev = device ? devices.data?.devices.find((d) => d.name === device) : undefined;
    const nifs = dev ? [dev.port] : sel.length ? sel : portNames;
    const q = [`container=${encodeURIComponent(JSON.stringify({ name: agentId, type: "docker" }))}`, `nif=${encodeURIComponent(nifs.join("/"))}`];
    if (dev?.mac) q.push(`filter=${encodeURIComponent("ether host " + dev.mac)}`);
    return `packetflix:${base}/capture?${q.join("&")}`;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ui, agents, agentId, sel, device, devices.data, ports.data]);
  const t0 = rows[0]?.t.getTime() ?? 0;
  return (
    <div className="card capture">
      <div className="card-head">
        <h2>Capture</h2>
        <span className="muted small-note">Frames the ports receive and send, as on the wire (device VLAN tags included).</span>
      </div>
      {unsupported && (
        <div className="warn-box" role="alert">
          {why(new ConnectError("", Code.Unimplemented))}
        </div>
      )}
      <div className="capture-controls">
        <fieldset className="capture-ports" disabled={running || !!device}>
          <legend className="field-label">ports</legend>
          {portNames.map((p) => (
            <label key={p} className="check">
              <input type="checkbox" checked={sel.includes(p)} onChange={(e) => setSel(e.target.checked ? [...sel, p] : sel.filter((x) => x !== p))} /> <span className="mono">{p}</span>
            </label>
          ))}
          <span className="muted small">{sel.length ? "" : "(all)"}</span>
        </fieldset>
        <label className="field">
          <span className="field-label">device</span>
          <select className="input small" value={device} disabled={running} onChange={(e) => setDevice(e.target.value)}>
            <option value="">any</option>
            {devices.data?.devices.map((d) => (
              <option key={d.name} value={d.name}>
                {d.name} ({d.port}
                {d.vlans.length ? " · " + d.vlans.map((v) => v.vid).join(".") : ""})
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field-label">direction</span>
          <select className="input small" value={direction} disabled={running} onChange={(e) => setDirection(e.target.value)}>
            <option value="">both</option>
            <option value="rx">received</option>
            <option value="tx">sent</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">ethertype</span>
          <select className="input small" value={et} disabled={running} onChange={(e) => setEt(e.target.value)}>
            {ETHERTYPES.map((x) => (
              <option key={x.label}>{x.label}</option>
            ))}
          </select>
        </label>
        <div className="btn-row">
          {running ? (
            <button className="btn" onClick={stop}>
              ■ Stop
            </button>
          ) : (
            <button className="btn primary" onClick={start} disabled={unsupported}>
              ● Start
            </button>
          )}
          <button className="btn" disabled={!rows.length} onClick={download} title="These frames as a pcapng file for Wireshark">
            Download .pcapng
          </button>
          {packetflix && (
            <a
              className="btn"
              href={packetflix}
              title="A live capture in Wireshark, through Edgeshark: needs its Wireshark plugin (cshargextcap) on this machine"
            >
              Open in Wireshark
            </a>
          )}
        </div>
      </div>
      {packetflix && (
        <div className="capture-ws muted small-note">
          Open in Wireshark goes through Edgeshark (its Wireshark plugin must be installed here). It captures the container's ports as the kernel sees
          them: the direction and ethertype choices don't apply, and a device becomes a filter on its MAC.
        </div>
      )}
      {!unsupported && (
      <div className="capture-ws muted small-note">
        Live in Wireshark:{" "}
        <code className="mono ws-cmd">{wireshark}</code>{" "}
        <button
          className="link small"
          onClick={async (e) => {
            if (await copyText(wireshark)) return toast("ok", "Copied");
            // Couldn't copy: select the command for Ctrl/Cmd+C.
            const code = (e.currentTarget.previousElementSibling ?? null) as HTMLElement | null;
            if (code) window.getSelection()?.selectAllChildren(code);
            toast("error", "Couldn't copy: the command is selected, press Ctrl+C (Cmd+C on a Mac)");
          }}
        >
          copy
        </button>
      </div>
      )}
      {!unsupported && (
      <div className="capture-ws muted small-note">
        Or in Wireshark itself, once: <code className="mono">clabgenctl wireshark install --ui {new URL(".", window.location.href).href.replace(/\/$/, "")}</code>, then pick a "clabgen …" interface (
        <a href="https://clabgen.dev/cli/#wireshark" target="_blank" rel="noreferrer">
          how
        </a>
        ).
      </div>
      )}
      {error && <div className="error-box">{error}</div>}
      <div className="capture-bar">
        <span className="mono">
          {running && <span className="pill pill-on">capturing</span>} {seq.current || rows.length} frames
          {rows.length >= MAX ? ` (last ${MAX} shown)` : ""}
          {dropped ? <span className="warn-text"> · {dropped} dropped (too fast for the page: filter more, or use Wireshark)</span> : null}
        </span>
        <input className="input small filter" type="search" placeholder="filter shown frames" aria-label="Filter shown frames" value={filter} onChange={(e) => setFilter(e.target.value)} />
        <label className="check small">
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> follow
        </label>
      </div>
      <div className="capture-split">
        <div
          className="capture-list mono"
          ref={list}
          onWheel={(e) => {
            // Scrolling up stops following; back down to the end follows again.
            const el = e.currentTarget;
            if (e.deltaY < 0 && follow) setFollow(false);
            else if (e.deltaY > 0 && !follow && el.scrollHeight - el.scrollTop - el.clientHeight < 24) setFollow(true);
          }}
        >
          {shown.length === 0 && <div className="muted capture-empty">{running ? "Waiting for frames…" : "Start a capture to see frames here."}</div>}
          {shown.map((r) => (
            <button key={r.n} className={`capture-row${picked?.n === r.n ? " on" : ""}`} onClick={() => pick(r)}>
              <span className="muted">{((r.t.getTime() - t0) / 1000).toFixed(3)}</span>
              <span>{r.f.port}</span>
              <span className={r.f.direction === "tx" ? "dir-tx" : "dir-rx"}>{r.f.direction === "tx" ? "out" : "in"}</span>
              <span className="capture-sum">{r.f.summary}</span>
            </button>
          ))}
        </div>
        {picked && (
          <div className="capture-detail">
            <div className="pkt-sec-head">
              <h4>
                Frame {picked.n} <span className="muted mono">· {hms(picked.t)} · {picked.f.port} {picked.f.direction === "tx" ? "out" : "in"}</span>
              </h4>
              <span className="btn-row">
                <button className="btn small" disabled={!detail?.decoded} onClick={rebuild} title="A packet flow that sends this frame">
                  Build a flow from it
                </button>
                <button className="icon-btn" onClick={() => setPicked(undefined)} aria-label="Close frame">
                  ×
                </button>
              </span>
            </div>
            {detail ? (
              <>
                <div className="mono pkt-summary">{detail.summary}</div>
                <PacketHex preview={detail} />
              </>
            ) : (
              <div className="muted">Decoding…</div>
            )}
          </div>
        )}
      </div>
      {building && <CreateProtocol agentId={agentId} category="traffic" preset={building} onClose={() => setBuilding(undefined)} onDone={() => {}} />}
    </div>
  );
}
