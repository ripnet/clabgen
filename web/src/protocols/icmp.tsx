import { flowSummary } from "../components/Flow";
import { IpFlowView } from "../components/FlowViews";
import { RoutePicker } from "../components/RoutePicker";
import { registerView } from "./registry";

registerView("icmp", { summary: flowSummary, Status: IpFlowView, ConfigExtra: RoutePicker });
