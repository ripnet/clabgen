import type { ComponentType } from "react";
import type { JsonObject } from "@bufbuild/protobuf";

export type Health = "ok" | "warn" | "bad" | "idle";

export type Summary = { health: Health; text: string };

// ProtocolView customises how one protocol type is shown. Everything is
// optional: types without a view get the generic schema form and value view,
// so new protocols work in the UI without any frontend changes.
export type ProtocolView = {
  // Compact one-line state for tables.
  summary?: (status: JsonObject) => Summary;
  // Rich status panel for the detail page.
  // agentId and name let a view run the instance's actions.
  Status?: ComponentType<{ status: JsonObject; agentId?: string; name?: string }>;
};

const views: Record<string, ProtocolView> = {};

export function registerView(type: string, v: ProtocolView) {
  views[type] = v;
}

export function viewFor(type: string): ProtocolView {
  return views[type] ?? {};
}

export function summarize(type: string, state: string, error: string, status?: JsonObject): Summary {
  if (state === "error") return { health: "bad", text: error || "error" };
  if (state !== "running") return { health: "idle", text: error || state };
  const s = viewFor(type).summary;
  if (s && status) return s(status);
  return { health: "ok", text: "running" };
}
