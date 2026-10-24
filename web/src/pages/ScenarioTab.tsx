import { useEffect, useRef, useState } from "react";
import { client, usePoll } from "../api";
import type { ConfigInfo } from "../gen/clabgen/v1/agent_pb";
import { agentLabel, useAgents } from "../agents";
import { ErrorBox, useBusy } from "../ui";

export function ScenarioTab({ agentId }: { agentId: string }) {
  const c = client(agentId);
  const sc = usePoll(() => c.getScenario({}), [agentId]);
  const agentInfo = usePoll(() => c.getInfo({}), [agentId], 5000);
  const cfg = agentInfo.data?.persistence;
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
      {cfg && <SavedConfig cfg={cfg} busy={busy} reset={() => run(() => c.resetConfig({}), "Config reset").then((r) => r && (setDirty(false), sc.refresh(), agentInfo.refresh()))} />}
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

// SavedConfig says where the agent keeps its config across lab restarts,
// and resets it to the topology's scenario.
function SavedConfig({ cfg, busy, reset }: { cfg: ConfigInfo; busy: boolean; reset: () => void }) {
  if (!cfg.file)
    return (
      <div className="muted small-note saved-config">
        Not saved across lab restarts: {cfg.notSaved || "no file"}. See <a href="https://clabgen.dev/configuration/#saved-config" target="_blank" rel="noreferrer">saved config</a>.
      </div>
    );
  const from =
    cfg.source === "saved" ? "restored from it" : cfg.source === "scenario" ? <>started from the topology's scenario (<code>{cfg.scenario}</code>)</> : "started empty";
  const to = cfg.scenario ? "the topology's scenario" : "an empty config";
  return (
    <div className="saved-config card-lite">
      <div>
        Saved to <code>{cfg.file}</code> after every change, and restored when the lab restarts (this start: {from}).
        {cfg.source === "saved" && cfg.scenario && <span className="muted"> Edits to the scenario file apply after a reset.</span>}
      </div>
      {cfg.error && <div className="error-box">Saving failed: {cfg.error}</div>}
      <div className="btn-row end">
        <button
          className="btn"
          disabled={busy}
          onClick={() => confirm(`Reset replaces everything on this agent with ${to}, and saves that instead. Continue?`) && reset()}
          title={`Delete the saved file and load ${to}`}
        >
          Reset to {to}
        </button>
      </div>
    </div>
  );
}
