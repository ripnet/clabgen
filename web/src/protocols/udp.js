import { jsx as _jsx } from "react/jsx-runtime";
import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { registerView } from "./registry";
function UdpStatus(p) {
    const s = p.status;
    return _jsx(IpFlowView, { ...p, extra: ` · ports ${s.src_ports} → ${s.dst_port}` });
}
registerView("udp", { summary: flowSummary, Status: UdpStatus });
