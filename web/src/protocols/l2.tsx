import type { JsonObject } from "@bufbuild/protobuf";
import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { registerView, type Summary } from "./registry";

function L2Status(p: { status: JsonObject; agentId?: string; name?: string }) {
  const s = p.status as { frame_size?: number };
  return <IpFlowView {...p} extra={s.frame_size ? ` · ${s.frame_size} B frames` : ""} />;
}

registerView("l2", { summary: flowSummary, Status: L2Status });

registerView("l2-echo", {
  summary: (st: JsonObject): Summary => {
    const s = st as { received?: number; sent?: number; tx_errors?: number };
    return { health: s.tx_errors ? "warn" : "ok", text: `echoed ${s.sent ?? 0} of ${s.received ?? 0}` };
  },
});
