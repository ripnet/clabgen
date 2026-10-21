import { useEffect, useState } from "react";

// A JSON schema as published by the agent (invopop/jsonschema output plus
// "default" values and "x-order").
export type Schema = {
  type?: string | string[];
  properties?: Record<string, Schema>;
  required?: string[];
  enum?: (string | number)[];
  default?: unknown;
  description?: string;
  minimum?: number;
  maximum?: number;
  items?: Schema;
  "x-order"?: string[];
};

export type Obj = Record<string, unknown>;

export function parseSchema(s: string | undefined): Schema {
  if (!s) return {};
  try {
    return JSON.parse(s) as Schema;
  } catch {
    return {};
  }
}

// defaults builds an object holding every default value in the schema.
export function defaults(s: Schema): Obj {
  const out: Obj = {};
  for (const [k, p] of Object.entries(s.properties ?? {})) {
    if (p.type === "object" && p.properties) {
      const d = defaults(p);
      if (Object.keys(d).length) out[k] = d;
    } else if (p.default !== undefined) out[k] = p.default;
  }
  return out;
}

function order(s: Schema): string[] {
  const keys = Object.keys(s.properties ?? {});
  const o = s["x-order"]?.filter((k) => keys.includes(k)) ?? [];
  return [...o, ...keys.filter((k) => !o.includes(k)).sort()];
}

const label = (k: string) => k.replace(/_/g, " ");

function typeOf(s: Schema): string {
  return Array.isArray(s.type) ? s.type.find((t) => t !== "null") ?? "string" : s.type ?? "string";
}

// "config" forms edit a whole object, so unset booleans are false. "args"
// forms send only what the user set: optional booleans are tri-state and
// empty fields are omitted.
export type FormMode = "config" | "args";

type FieldProps = {
  name: string;
  schema: Schema;
  value: unknown;
  required: boolean;
  mode: FormMode;
  onChange: (v: unknown) => void;
};

function ListField({ schema, value, onChange }: FieldProps) {
  const itemType = typeOf(schema.items ?? {});
  const toText = (v: unknown) => (Array.isArray(v) ? v.join(", ") : "");
  const [text, setText] = useState(toText(value));
  useEffect(() => setText(toText(value)), [JSON.stringify(value)]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <input
      className="input mono"
      value={text}
      placeholder={itemType === "string" ? "a, b, c" : "1, 2, 3"}
      onChange={(e) => {
        setText(e.target.value);
        const parts = e.target.value.split(/[\s,]+/).filter(Boolean);
        if (!parts.length) return onChange(undefined);
        if (itemType === "integer" || itemType === "number") {
          const nums = parts.map(Number);
          if (nums.every((n) => !Number.isNaN(n))) onChange(nums);
        } else onChange(parts);
      }}
    />
  );
}

// ObjectListField edits an array of objects: one sub-form per item.
function ObjectListField({ schema, value, onChange }: FieldProps) {
  const items = Array.isArray(value) ? (value as Obj[]) : [];
  const item = schema.items ?? {};
  const set = (next: Obj[]) => onChange(next.length ? next : undefined);
  return (
    <div className="obj-list">
      {items.map((it, i) => (
        <div className="obj-list-row" key={i}>
          <SchemaFields schema={item} value={it ?? {}} mode="args" onChange={(v) => set(items.map((x, j) => (j === i ? v : x)))} nested />
          <button type="button" className="icon-btn" aria-label="Remove item" title="Remove" onClick={() => set(items.filter((_, j) => j !== i))}>
            ×
          </button>
        </div>
      ))}
      <button type="button" className="btn small" onClick={() => set([...items, defaults(item)])}>
        Add
      </button>
    </div>
  );
}

function Field(p: FieldProps) {
  const { schema, value, mode, required, onChange } = p;
  const t = typeOf(schema);
  if (schema.enum) {
    return (
      <select className="input" value={value === undefined ? "" : String(value)} onChange={(e) => {
        const raw = e.target.value;
        if (raw === "") return onChange(undefined);
        const match = schema.enum!.find((x) => String(x) === raw);
        onChange(match);
      }}>
        {(!required || value === undefined) && <option value="">{mode === "args" ? "— unchanged —" : "—"}</option>}
        {schema.enum.map((x) => (
          <option key={String(x)} value={String(x)}>
            {String(x)}
          </option>
        ))}
      </select>
    );
  }
  switch (t) {
    case "boolean":
      if (mode === "args" && schema.default === undefined && !required) {
        return (
          <select className="input" value={value === undefined ? "" : String(value)}
            onChange={(e) => onChange(e.target.value === "" ? undefined : e.target.value === "true")}>
            <option value="">— unchanged —</option>
            <option value="true">true</option>
            <option value="false">false</option>
          </select>
        );
      }
      return (
        <label className="switch">
          <input type="checkbox" checked={value === true} onChange={(e) => onChange(e.target.checked)} />
          <span>{value === true ? "on" : "off"}</span>
        </label>
      );
    case "integer":
    case "number":
      return (
        <input
          className="input mono"
          type="number"
          min={schema.minimum}
          max={schema.maximum}
          step={t === "integer" ? 1 : "any"}
          value={value === undefined || value === null ? "" : String(value)}
          placeholder={schema.default !== undefined ? String(schema.default) : ""}
          onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
        />
      );
    case "array":
      if (typeOf(schema.items ?? {}) === "object" && schema.items?.properties) return <ObjectListField {...p} />;
      return <ListField {...p} />;
    case "object":
      return (
        <SchemaFields schema={schema} value={(value as Obj) ?? {}} mode={mode} onChange={(v) => onChange(v)} nested />
      );
    default:
      return (
        <input
          className="input mono"
          value={value === undefined || value === null ? "" : String(value)}
          placeholder={schema.default !== undefined ? String(schema.default) : ""}
          onChange={(e) => onChange(e.target.value === "" && mode === "args" ? undefined : e.target.value)}
        />
      );
  }
}

function SchemaFields({
  schema,
  value,
  mode,
  onChange,
  nested,
}: {
  schema: Schema;
  value: Obj;
  mode: FormMode;
  onChange: (v: Obj) => void;
  nested?: boolean;
}) {
  const req = new Set(schema.required ?? []);
  return (
    <div className={nested ? "form-grid nested" : "form-grid"}>
      {order(schema).map((k) => {
        const ps = schema.properties![k];
        return (
          <div className="field" key={k}>
            <label className="field-label" title={ps.description}>
              {label(k)}
              {req.has(k) && <span className="req">*</span>}
              {ps.minimum !== undefined && ps.maximum !== undefined && (
                <span className="range">
                  {ps.minimum}–{ps.maximum}
                </span>
              )}
            </label>
            <Field
              name={k}
              schema={ps}
              value={value[k]}
              required={req.has(k)}
              mode={mode}
              onChange={(v) => {
                const next = { ...value };
                if (v === undefined) delete next[k];
                else next[k] = v;
                onChange(next);
              }}
            />
            {ps.description && <div className="hint">{ps.description}</div>}
          </div>
        );
      })}
    </div>
  );
}

// SchemaForm renders a form for a schema with a raw-JSON escape hatch.
export function SchemaForm({
  schema,
  value,
  onChange,
  mode,
}: {
  schema: Schema;
  value: Obj;
  onChange: (v: Obj) => void;
  mode: FormMode;
}) {
  const [raw, setRaw] = useState(false);
  const [text, setText] = useState("");
  const [err, setErr] = useState<string>();
  const empty = !schema.properties || Object.keys(schema.properties).length === 0;
  return (
    <div className="schema-form">
      <div className="form-toolbar">
        <button type="button" className="link" onClick={() => {
          if (!raw) setText(JSON.stringify(value, null, 2));
          setErr(undefined);
          setRaw(!raw);
        }}>
          {raw ? "form view" : "edit as JSON"}
        </button>
      </div>
      {raw ? (
        <>
          <textarea
            className="input mono code"
            rows={Math.min(24, Math.max(6, text.split("\n").length + 1))}
            value={text}
            spellCheck={false}
            onChange={(e) => {
              setText(e.target.value);
              try {
                const v = JSON.parse(e.target.value || "{}");
                if (typeof v !== "object" || Array.isArray(v) || v === null) throw new Error("must be a JSON object");
                setErr(undefined);
                onChange(v);
              } catch (x) {
                setErr((x as Error).message);
              }
            }}
          />
          {err && <div className="hint error-text">{err}</div>}
        </>
      ) : empty ? (
        <div className="muted">No parameters.</div>
      ) : (
        <SchemaFields schema={schema} value={value} mode={mode} onChange={onChange} />
      )}
    </div>
  );
}
