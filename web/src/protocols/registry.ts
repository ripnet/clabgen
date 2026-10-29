import type { ComponentType } from "react";
import type { JsonObject } from "@bufbuild/protobuf";
import type { Obj, Schema } from "../components/SchemaForm";

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
  // Config editor replacing the generic form (create and edit), e.g. the
  // packet builder's layers with a live preview from the device.
  ConfigEditor?: ComponentType<ConfigEditorProps>;
  // What applying a config edit does, for types that apply some edits in
  // place (default: the instance restarts).
  applyNote?: (was: Obj, now: Obj) => ApplyNote;
};

export type ApplyNote = { restart: boolean; text: string };

export const restartNote: ApplyNote = { restart: true, text: "Applying restarts the instance: counters and the remote state are reset." };

// inPlaceNote builds an applyNote for a type that applies some edits in
// place (its agent side's Reconfigure): an edit that only touches the
// fields in inPlace (and in each list item of lists, the fields given
// there) keeps the instance running with message ok; any other edit
// restarts it.
export function inPlaceNote(inPlace: string[], lists: Record<string, string[]>, ok: string) {
  const strip = (c: Obj) => {
    const out: Obj = { ...c };
    for (const k of inPlace) delete out[k];
    for (const [k, fields] of Object.entries(lists))
      if (Array.isArray(out[k]))
        out[k] = (out[k] as Obj[]).map((it) => {
          const x: Obj = { ...it };
          for (const f of fields) delete x[f];
          return x;
        });
    return JSON.stringify(out);
  };
  return (was: Obj, now: Obj): ApplyNote => (canonical(strip(was)) === canonical(strip(now)) ? { restart: false, text: ok } : restartNote);
}

// canonical is a JSON text with keys sorted at every level.
function canonical(json: string): string {
  const sort = (v: unknown): unknown =>
    Array.isArray(v) ? v.map(sort) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sort((v as Obj)[k])])) : v;
  return JSON.stringify(sort(JSON.parse(json)));
}

export type ConfigEditorProps = {
  schema: Schema;
  value: Obj;
  onChange: (v: Obj) => void;
  original?: Obj;
  errorText?: string;
  agentId: string;
  device: string;
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
