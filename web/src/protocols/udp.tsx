import type { JsonObject } from "@bufbuild/protobuf";
import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { registerView } from "./registry";

function UdpStatus(p: { status: JsonObject; agentId?: string; name?: string }) {
  const s = p.status as { src_ports?: string; dst_port?: number };
  return <IpFlowView {...p} extra={` · ports ${s.src_ports} → ${s.dst_port}`} />;
}

registerView("udp", { summary: flowSummary, Status: UdpStatus });
