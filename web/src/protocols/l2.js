import { jsx as _jsx } from "react/jsx-runtime";
import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { registerView } from "./registry";
function L2Status(p) {
    const s = p.status;
    return _jsx(IpFlowView, { ...p, extra: s.frame_size ? ` · ${s.frame_size} B frames` : "" });
}
registerView("l2", { summary: flowSummary, Status: L2Status });
registerView("l2-echo", {
    summary: (st) => {
        const s = st;
        return { health: s.tx_errors ? "warn" : "ok", text: `echoed ${s.sent ?? 0} of ${s.received ?? 0}` };
    },
});
