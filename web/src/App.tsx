import { useState } from "react";
import { agentsApi, type AgentRef } from "./api";
import { agentLabel, useAgents } from "./agents";
import { href, useRoute } from "./router";
import { Dot, ErrorBox, Modal, useBusy, useTheme, type Theme } from "./ui";
import { Overview } from "./pages/Overview";
import { FlowsPage } from "./pages/FlowsPage";
import { displayVersion, skew } from "./version";
import { AgentPage } from "./pages/AgentPage";
import { ProtocolDetail } from "./pages/ProtocolDetail";
import { GroupDetail } from "./pages/GroupsTab";

export function App() {
  const route = useRoute();
  const [theme, setTheme] = useTheme();
  const [navOpen, setNavOpen] = useState(false);
  const current = route.page === "overview" || route.page === "flows" ? undefined : route.agent;

  return (
    <div className={`shell ${navOpen ? "nav-open" : ""}`}>
      <header className="topbar">
        <button className="icon-btn nav-toggle" onClick={() => setNavOpen(!navOpen)} aria-label="Toggle navigation">
          ☰
        </button>
        <a className="brand" href="#/">
          <svg viewBox="0 0 32 32" width="22" height="22" aria-hidden="true">
            <rect width="32" height="32" rx="7" fill="var(--accent)" />
            <path d="M6 20h5l3-9 4 13 3-8h5" fill="none" stroke="white" strokeWidth="2.6" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
          clabgen
        </a>
        <UIVersion />
        <div className="grow" />
        <ThemeSwitch theme={theme} setTheme={setTheme} />
      </header>
      <aside className="sidebar" onClick={(e) => (e.target as HTMLElement).closest("a") && setNavOpen(false)}>
        <Sidebar current={current} page={route.page} />
      </aside>
      <main className="main">
        {route.page === "overview" && <Overview />}
        {route.page === "flows" && <FlowsPage />}
        {route.page === "agent" && <AgentPage key={route.agent} agentId={route.agent} tab={route.tab} />}
        {route.page === "protocol" && <ProtocolDetail key={route.agent + "/" + route.protocol} agentId={route.agent} name={route.protocol} />}
        {route.page === "group" && <GroupDetail key={route.agent + "/" + route.group} agentId={route.agent} name={route.group} />}
      </main>
    </div>
  );
}

// UIVersion shows the version serving the UI, and a newer release.
function UIVersion() {
  const { ui } = useAgents();
  if (!ui?.version) return null;
  return (
    <>
      <a className="brand-version muted" href="https://github.com/ripnet/clabgen" target="_blank" rel="noreferrer" title="clabgen by Tom Young and contributors">
        {displayVersion(ui.version)}
      </a>
      {ui.update_available && ui.latest && (
        <a
          className="update-link"
          href={ui.latest_url}
          target="_blank"
          rel="noreferrer"
          title="Update: docker pull ghcr.io/ripnet/clabgen-ui and recreate the container; redeploy labs to update their agents"
        >
          {displayVersion(ui.latest)} available
        </a>
      )}
    </>
  );
}

function ThemeSwitch({ theme, setTheme }: { theme: Theme; setTheme: (t: Theme) => void }) {
  return (
    <div className="seg small" role="radiogroup" aria-label="Theme">
      {(["auto", "light", "dark"] as Theme[]).map((t) => (
        <button key={t} role="radio" aria-checked={theme === t} className={theme === t ? "seg-btn active" : "seg-btn"} onClick={() => setTheme(t)}>
          {t}
        </button>
      ))}
    </div>
  );
}

function Sidebar({ current, page }: { current?: string; page: string }) {
  const { agents, info, ui, allowManual, error, refresh } = useAgents();
  const [adding, setAdding] = useState(false);
  const groups = new Map<string, AgentRef[]>();
  for (const a of agents) {
    const g = a.source === "self" ? "this agent" : a.lab || "agents";
    groups.set(g, [...(groups.get(g) ?? []), a]);
  }
  return (
    <nav aria-label="Agents">
      <a className={`side-link ${page === "overview" ? "active" : ""}`} href="#/">
        Lab overview
      </a>
      <a className={`side-link ${page === "flows" ? "active" : ""}`} href="#/flows">
        Flows
      </a>
      {error && <ErrorBox error={`Agent list: ${error}`} />}
      {[...groups.entries()].map(([g, list]) => (
        <div key={g} className="side-group">
          <div className="side-group-title">{g}</div>
          {list.map((a) => {
            const i = info[a.id];
            const k = i?.reachable ? skew(i.version, ui?.version) : undefined;
            return (
              <a
                key={a.id}
                className={`side-link ${current === a.id ? "active" : ""}`}
                href={href({ page: "agent", agent: a.id, tab: "protocols" })}
                title={i?.reachable ? `${i.hostname} · ${displayVersion(i.version)} · ${a.url ?? "local"}${k && k.kind !== "differs" ? `\n${k.text}` : ""}` : i?.error}
              >
                <Dot health={i?.reachable ? "ok" : i ? "bad" : "idle"} />
                <span className="side-name">{agentLabel(a, i)}</span>
                {k?.kind === "older" && <span className="side-ver warn">update</span>}
                {k?.kind === "newer" && <span className="side-ver">newer</span>}
                {a.source === "manual" && <RemoveAgent id={a.id} onDone={refresh} />}
              </a>
            );
          })}
        </div>
      ))}
      {allowManual && (
        <button className="btn small side-add" onClick={() => setAdding(true)}>
          Add agent
        </button>
      )}
      {adding && <AddAgent onClose={() => setAdding(false)} onDone={refresh} />}
    </nav>
  );
}

function RemoveAgent({ id, onDone }: { id: string; onDone: () => void }) {
  const { run } = useBusy();
  return (
    <button
      className="icon-btn side-x"
      aria-label={`Remove ${id}`}
      onClick={(e) => {
        e.preventDefault();
        e.stopPropagation();
        run(() => agentsApi.remove(id)).then(onDone);
      }}
    >
      ×
    </button>
  );
}

function AddAgent({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [url, setUrl] = useState("");
  const [name, setName] = useState("");
  const { busy, run } = useBusy();
  const id = (name || url).replace(/^https?:\/\//, "").replace(/[^A-Za-z0-9_.-]/g, "-").replace(/^-+/, "");
  return (
    <Modal title="Add agent" onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          run(() => agentsApi.add({ id, name: name || undefined, url }), `Added ${name || url}`).then((r) => {
            if (r) {
              onDone();
              onClose();
            }
          });
        }}
      >
        <div className="form-grid">
          <div className="field">
            <label className="field-label">
              address<span className="req">*</span>
            </label>
            <input className="input mono" value={url} onChange={(e) => setUrl(e.target.value)} placeholder="172.20.20.5 or host:8080" required autoFocus />
            <div className="hint">Must be reachable from the server hosting this page; calls are proxied through it.</div>
          </div>
          <div className="field">
            <label className="field-label">name</label>
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} placeholder="optional" />
          </div>
        </div>
        <div className="hint">The agent is kept until the server restarts.</div>
        <div className="modal-foot">
          <button type="button" className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={busy || !url || !id}>
            Add
          </button>
        </div>
      </form>
    </Modal>
  );
}
