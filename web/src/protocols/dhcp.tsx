import type { JsonObject } from "@bufbuild/protobuf";
import { registerView, type Summary } from "./registry";

type Binding = { prefix: string };
type V4 = { state: string; address?: string; gateway?: string; last_nak?: string };
type V6 = { state: string; addresses?: Binding[]; prefixes?: Binding[]; ia_na_status?: string; ia_pd_status?: string };

const leased = (state: string) => state === "bound" || state === "renewing" || state === "rebinding";

// Waiting for a lease is a warning: in a test, a client without one is
// what to look at.
function idle(state: string, why?: string): Summary {
  if (state === "released") return { health: "idle", text: "released" };
  return { health: "warn", text: why ? `${state} · ${why}` : state };
}

function summary4(st: JsonObject): Summary {
  const s = st as unknown as V4;
  if (!leased(s.state) || !s.address) return idle(s.state, s.last_nak !== undefined ? `NAK ${s.last_nak}`.trim() : undefined);
  const tail = s.state === "bound" ? "" : ` · ${s.state}`;
  return { health: "ok", text: `${s.address}${s.gateway ? ` · gw ${s.gateway}` : ""}${tail}` };
}

function summary6(st: JsonObject): Summary {
  const s = st as unknown as V6;
  const got = [...(s.addresses ?? []), ...(s.prefixes ?? [])].map((b) => b.prefix);
  if (!leased(s.state) || !got.length) return idle(s.state);
  const missing = [s.ia_na_status, s.ia_pd_status].filter(Boolean);
  const tail = s.state === "bound" ? "" : ` · ${s.state}`;
  return { health: missing.length ? "warn" : "ok", text: [...got, ...missing].join(" · ") + tail };
}

registerView("dhcp", { summary: summary4 });
registerView("dhcpv6", { summary: summary6 });
