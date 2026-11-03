import { useEffect, useState } from "react";
import { client } from "../api";
import type { ConfigEditorProps } from "../protocols/registry";
import { RoutePicker } from "./RoutePicker";

type LSP = { name: string; to: string; instance: string };

// LSPPicker lets a flow send into an LSP an RSVP instance on its device
// signals. It's hidden when the device has none.
export function LSPPicker({ value, onChange, agentId, device }: ConfigEditorProps) {
  const [lsps, setLsps] = useState<LSP[]>([]);
  useEffect(() => {
    let live = true;
    client(agentId)
      .listProtocols({}, { timeoutMs: 4000 })
      .then((r) => {
        if (!live) return;
        const out: LSP[] = [];
        for (const p of r.protocols) {
          if (p.type !== "rsvp" || p.device !== device) continue;
          const cfg = (p.config ?? {}) as unknown as { lsps?: { name: string; to: string }[] };
          for (const l of cfg.lsps ?? []) out.push({ name: l.name, to: l.to, instance: p.name });
        }
        setLsps(out);
      })
      .catch(() => setLsps([]));
    return () => {
      live = false;
    };
  }, [agentId, device]);
  if (!lsps.length && !value.lsp) return null;
  const pick = (name: string) => {
    const next = { ...value };
    if (name) {
      next.lsp = name;
      const l = lsps.find((x) => x.name === name);
      if (l && !next.destination && !(next.destinations as unknown[] | undefined)?.length) next.destination = l.to;
    } else {
      delete next.lsp;
    }
    onChange(next);
  };
  return (
    <label className="lsp-picker">
      <span className="field-label">Send into an LSP</span>
      <select className="input" value={(value.lsp as string) ?? ""} onChange={(e) => pick(e.target.value)} aria-label="Send into an LSP">
        <option value="">No: routed as usual</option>
        {lsps.map((l) => (
          <option key={l.name} value={l.name}>
            {l.name} to {l.to} ({l.instance})
          </option>
        ))}
        {!!value.lsp && !lsps.some((l) => l.name === value.lsp) && <option value={value.lsp as string}>{value.lsp as string}</option>}
      </select>
    </label>
  );
}

// FlowExtras is what an IP flow's form shows above the fields.
export function FlowExtras(p: ConfigEditorProps) {
  return (
    <>
      <LSPPicker {...p} />
      <RoutePicker {...p} />
    </>
  );
}
