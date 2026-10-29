import { useState } from "react";

// Stepped values in device group templates: {start, step, repeat, cycle}.
// Member i (from 0) gets start + k·step with k = (i / repeat) % cycle;
// start is an integer, an IP address, a prefix or a MAC, step an integer or
// (for addresses and MACs) a value of the same form, e.g. 0.0.0.4. An
// omitted step is 1.
export type Step = { start: number | string; step?: number | string; repeat?: number; cycle?: number };

const stepKeys = new Set(["start", "step", "repeat", "cycle"]);

export function isStep(v: unknown): v is Step {
  if (!v || typeof v !== "object" || Array.isArray(v)) return false;
  const ks = Object.keys(v);
  return ks.includes("start") && ks.every((k) => stepKeys.has(k));
}

// fmtStep is a one-line description, e.g. "101 +1 per member".
export function fmtStep(s: Step): string {
  let out = `${s.start} +${s.step ?? 1}`;
  out += s.repeat && s.repeat > 1 ? ` every ${s.repeat} members` : " per member";
  if (s.cycle) out += `, ${s.cycle} values`;
  return out;
}

// A step typed as text: an integer is a number, anything else (0.0.0.4,
// ::1:0, 00:00:00:00:01:00) a string.
const parseStep = (t: string): number | string | undefined => {
  const s = t.trim();
  if (s === "") return undefined;
  return /^-?\d+$/.test(s) ? Number(s) : s;
};

const posInt = (t: string) => {
  const n = Number(t);
  return t.trim() !== "" && Number.isInteger(n) && n >= 1 ? n : undefined;
};

// StepInput edits a value that may be stepped per member. A plain value
// shows its own input (children) and a "per member" switch; a stepped one
// shows start and step, with repeat and cycle behind "more".
export function StepInput({
  value,
  onChange,
  numeric,
  label,
  children,
}: {
  value: unknown;
  onChange: (v: unknown) => void;
  numeric: boolean; // an integer field: start is a number
  label: string;
  children: React.ReactNode; // the plain input
}) {
  const stepped = isStep(value);
  const [more, setMore] = useState(stepped && (!!value.repeat || !!value.cycle));
  const toggle = (
    <button
      type="button"
      className={`step-toggle${stepped ? " on" : ""}`}
      title={stepped ? "Same value for every member" : "Step per member"}
      aria-label={stepped ? `${label}: same for every member` : `${label}: step per member`}
      aria-pressed={stepped}
      onClick={() => {
        if (stepped) return onChange(value.start === "" ? undefined : value.start);
        const start = value === undefined || value === null || value === "" ? (numeric ? 1 : "") : (value as number | string);
        onChange({ start });
      }}
    >
      ±
    </button>
  );
  if (!stepped)
    return (
      <div className="step-field">
        {children}
        {toggle}
      </div>
    );
  const set = (k: keyof Step, v: number | string | undefined) => {
    const next: Step = { ...value };
    if (v === undefined) delete next[k];
    else (next as Record<string, unknown>)[k] = v;
    onChange(next);
  };
  return (
    <div className="step-field stepped">
      <div className="step-row">
        <input
          className="input mono"
          aria-label={`${label} start`}
          placeholder="start"
          type={numeric ? "number" : "text"}
          value={String(value.start ?? "")}
          onChange={(e) => set("start", numeric ? (e.target.value === "" ? "" : Number(e.target.value)) : e.target.value)}
        />
        <span className="step-plus">+</span>
        <input
          className="input mono step-step"
          aria-label={`${label} step`}
          placeholder="1"
          value={value.step === undefined ? "" : String(value.step)}
          onChange={(e) => set("step", parseStep(e.target.value))}
        />
        {toggle}
      </div>
      {more ? (
        <div className="step-row step-more">
          <label>
            every
            <input
              className="input mono"
              aria-label={`${label} repeat`}
              placeholder="1"
              value={value.repeat ?? ""}
              onChange={(e) => set("repeat", posInt(e.target.value))}
            />
            members
          </label>
          <label>
            wrap after
            <input
              className="input mono"
              aria-label={`${label} cycle`}
              placeholder="never"
              value={value.cycle ?? ""}
              onChange={(e) => set("cycle", posInt(e.target.value))}
            />
            values
          </label>
        </div>
      ) : (
        <button type="button" className="link small" onClick={() => setMore(true)}>
          repeat / wrap…
        </button>
      )}
    </div>
  );
}
