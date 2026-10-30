import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { registerView } from "./registry";
registerView("icmp", { summary: flowSummary, Status: IpFlowView });
