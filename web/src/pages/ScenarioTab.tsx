import { useEffect, useRef, useState } from "react";
import { client, usePoll } from "../api";
import { agentLabel, useAgents } from "../agents";
import { ErrorBox, useBusy } from "../ui";

export function ScenarioTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const sc = usePoll(() => c.getScenario({}), [agentId]);
  const [text, setText] = useState("");
  const [dirty, setDirty] = useState(false);
  const { busy, run } = useBusy();
  const file = useRef<HTMLInputElement>(null);
  const { agents, info } = useAgents();
  const label = agentLabel(agents.find((a) => a.id === agentId), info[agentId]);

  useEffect(() => {
    if (sc.data && !dirty) setText(sc.data.yaml);
  }, [sc.data, dirty]);

  const apply = (replace: boolean) => {
    if (replace && !confirm("Replace removes every existing device and protocol on this agent before applying. Continue?")) return;
    run(() => c.applyScenario({ yaml: text, replace }), replace ? "Scenario applied (replaced)" : "Scenario applied").then((r) => {
      if (r) {
        setDirty(false);
        sc.refresh();
      }
    });
  };

  return (
    <div className="card">
      <div className="card-head">
        <h2>Scenario</h2>
        <div className="btn-row">
          <button className="btn" onClick={() => { setDirty(false); sc.refresh(); }} disabled={busy}>
            {dirty ? "Discard edits" : "Reload"}
          </button>
          <button className="btn" onClick={() => file.current?.click()}>
            Open file…
          </button>
          <input
            ref={file}
            type="file"
            accept=".yml,.yaml,text/yaml"
            hidden
            onChange={(e) => {
              const f = e.target.files?.[0];
              if (f) f.text().then((t) => { setText(t); setDirty(true); });
              e.target.value = "";
            }}
          />
          <button
            className="btn"
            onClick={() => {
              const a = document.createElement("a");
              a.href = URL.createObjectURL(new Blob([text], { type: "text/yaml" }));
              a.download = `${label}.scenario.yml`;
              a.click();
              URL.revokeObjectURL(a.href);
            }}
          >
            Download
          </button>
          <button className="btn" onClick={() => apply(false)} disabled={busy || !text.trim()}>
            Apply (merge)
          </button>
          <button className="btn primary" onClick={() => apply(true)} disabled={busy || !text.trim()}>
            Apply &amp; replace
          </button>
        </div>
      </div>
      <p className="muted">
        The agent's whole configuration as YAML: the same format that <code>CLABGEN_SCENARIO</code> loads at boot. Merge adds to what's
        there and reports conflicts; replace starts clean.
      </p>
      <ErrorBox error={sc.error} />
      <textarea
        className="input mono code scenario"
        spellCheck={false}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setDirty(true);
        }}
      />
      {dirty && <div className="hint">Edited, not applied yet.</div>}
    </div>
  );
}
