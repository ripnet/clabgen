import { useEffect, useState } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import { client, errorText } from "../api";
import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { PacketEditor } from "../components/PacketEditor";
import { PacketHex, type Preview } from "../components/PacketHex";
import { registerView } from "./registry";

// The packet a running flow sends: its preview action, packet by packet.
function NextPacket({ agentId, name }: { agentId: string; name: string }) {
  const [index, setIndex] = useState<number>();
  const [p, setP] = useState<Preview>();
  const [err, setErr] = useState<string>();
  useEffect(() => {
    client(agentId)
      .runProtocolAction({ name, action: "preview", args: (index === undefined ? {} : { index }) as JsonObject })
      .then((r) => {
        setP(r.result as unknown as Preview);
        setErr(undefined);
      })
      .catch((e) => setErr(errorText(e)));
  }, [agentId, name, index]);
  const at = p?.index ?? 0;
  return (
    <details className="pkt-next">
      <summary>
        <h4>Packet</h4>
      </summary>
      {err && <div className="error-box">{err}</div>}
      {p && (
        <>
          <div className="btn-row">
            <button type="button" className="icon-btn" disabled={at === 0} onClick={() => setIndex(at - 1)} aria-label="Previous packet">
              ‹
            </button>
            <span className="mono">packet {at}</span>
            <button type="button" className="icon-btn" onClick={() => setIndex(at + 1)} aria-label="Next packet">
              ›
            </button>
            <span className="mono muted">{p.summary}</span>
          </div>
          {p.note && <div className={p.note.startsWith("next hop unresolved") ? "warn-box small-note" : "muted small-note"}>{p.note}</div>}
          <PacketHex preview={p} />
        </>
      )}
    </details>
  );
}

function PacketStatus(p: { status: JsonObject; agentId?: string; name?: string }) {
  return (
    <>
      <IpFlowView {...p} />
      {p.agentId && p.name && <NextPacket agentId={p.agentId} name={p.name} />}
    </>
  );
}

registerView("packet", { summary: flowSummary, Status: PacketStatus, ConfigEditor: PacketEditor });
