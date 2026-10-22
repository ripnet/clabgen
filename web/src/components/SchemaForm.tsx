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
  return Array.isArray(s.type)
    ? (s.type.find((t) => t !== "null") ?? "string")
    : (s.type ?? "string");
}

// Kinds of fields: scalars (and scalar lists) sit in a grid; lists of
// objects and nested objects get a block (a section at the top level).
const isObjectList = (s: Schema) =>
  typeOf(s) === "array" &&
  typeOf(s.items ?? {}) === "object" &&
  !!s.items?.properties;
const isObject = (s: Schema) => typeOf(s) === "object" && !!s.properties;
const isBlock = (s: Schema) => isObjectList(s) || isObject(s);

// isSet: the value says something (empty lists and objects don't).
function isSet(v: unknown): boolean {
  if (v === undefined || v === null || v === "") return false;
  if (Array.isArray(v)) return v.length > 0;
  if (typeof v === "object") return Object.values(v as Obj).some(isSet);
  return true;
}

// The default a description mentions ("Default: the device's address"),
// for placeholders.
function defaultHint(s: Schema): string {
  if (s.default !== undefined) return String(s.default);
  const m = s.description?.match(/Default:?\s*([^.;]+)/i);
  return m ? m[1].trim() : "";
}

// "config" forms edit a whole object, so unset booleans are false. "args"
// forms send only what the user set: optional booleans are tri-state and
// empty fields are omitted. Objects inside lists are always edited as
// config: they are whole items.
export type FormMode = "config" | "args";

type FieldProps = {
  name: string;
  schema: Schema;
  value: unknown;
  required: boolean;
  mode: FormMode;
  onChange: (v: unknown) => void;
};

// EnumListField picks values of a list whose items have an enum.
function EnumListField({ schema, value, onChange }: FieldProps) {
  const cur = Array.isArray(value) ? (value as unknown[]) : [];
  const opts = schema.items!.enum!;
  return (
    <div className="check-list">
      {opts.map((o) => (
        <label className="switch small" key={String(o)}>
          <input
            type="checkbox"
            checked={cur.includes(o)}
            onChange={(e) => {
              const next = e.target.checked
                ? opts.filter((x) => x === o || cur.includes(x))
                : cur.filter((x) => x !== o);
              onChange(next.length ? next : undefined);
            }}
          />
          <span className="mono">{String(o)}</span>
        </label>
      ))}
    </div>
  );
}

function ListField({ schema, value, onChange }: FieldProps) {
  const itemType = typeOf(schema.items ?? {});
  const toText = (v: unknown) => (Array.isArray(v) ? v.join(", ") : "");
  const [text, setText] = useState(toText(value));
  useEffect(() => setText(toText(value)), [JSON.stringify(value)]); // eslint-disable-line react-hooks/exhaustive-deps
  return (
    <input
      className="input mono"
      value={text}
      placeholder={
        defaultHint(schema) || (itemType === "string" ? "a, b, c" : "1, 2, 3")
      }
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

function Field(p: FieldProps) {
  const { schema, value, mode, required, onChange } = p;
  const t = typeOf(schema);
  if (schema.enum) {
    return (
      <select
        className="input"
        value={value === undefined ? "" : String(value)}
        onChange={(e) => {
          const raw = e.target.value;
          if (raw === "") return onChange(undefined);
          onChange(schema.enum!.find((x) => String(x) === raw));
        }}
      >
        {(!required || value === undefined) && (
          <option value="">
            {mode === "args"
              ? "— unchanged —"
              : defaultHint(schema)
                ? `default (${defaultHint(schema)})`
                : "—"}
          </option>
        )}
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
          <select
            className="input"
            value={value === undefined ? "" : String(value)}
            onChange={(e) =>
              onChange(
                e.target.value === "" ? undefined : e.target.value === "true",
              )
            }
          >
            <option value="">— unchanged —</option>
            <option value="true">true</option>
            <option value="false">false</option>
          </select>
        );
      }
      return (
        <label className="switch">
          <input
            type="checkbox"
            checked={value === true}
            onChange={(e) =>
              onChange(
                e.target.checked
                  ? true
                  : mode === "config" && schema.default === true
                    ? false
                    : undefined,
              )
            }
          />
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
          placeholder={defaultHint(schema)}
          onChange={(e) =>
            onChange(e.target.value === "" ? undefined : Number(e.target.value))
          }
        />
      );
    case "array":
      return schema.items?.enum ? (
        <EnumListField {...p} />
      ) : (
        <ListField {...p} />
      );
    default:
      return (
        <input
          className="input mono"
          value={value === undefined || value === null ? "" : String(value)}
          placeholder={defaultHint(schema)}
          onChange={(e) =>
            onChange(e.target.value === "" ? undefined : e.target.value)
          }
        />
      );
  }
}

function FieldLabel({
  name,
  schema,
  required,
}: {
  name: string;
  schema: Schema;
  required: boolean;
}) {
  return (
    <label className="field-label">
      {label(name)}
      {required && <span className="req">*</span>}
      {schema.description && (
        <span
          className="info"
          title={schema.description}
          aria-label={schema.description}
        >
          ?
        </span>
      )}
      {schema.minimum !== undefined && schema.maximum !== undefined && (
        <span className="range">
          {schema.minimum}–{schema.maximum}
        </span>
      )}
    </label>
  );
}

// FieldSet edits an object's fields: by default only the set and required
// ones, the rest behind "show all options".
function FieldSet({
  schema,
  value,
  mode,
  onChange,
  keys,
}: {
  schema: Schema;
  value: Obj;
  mode: FormMode;
  onChange: (v: Obj) => void;
  keys?: string[];
}) {
  const [all, setAll] = useState(false);
  const req = new Set(schema.required ?? []);
  const ks = keys ?? order(schema);
  // Small forms (most action arguments) show everything.
  const shown = (k: string) =>
    all || ks.length <= 6 || req.has(k) || isSet(value[k]);
  let visible = ks.filter(shown);
  // Nothing set yet: show the first few so there is something to fill.
  if (!all && visible.length === 0)
    visible = ks.slice(0, Math.min(4, ks.length));
  const hidden = ks.length - visible.length;
  const set = (k: string, v: unknown) => {
    const next = { ...value };
    if (v === undefined) delete next[k];
    else next[k] = v;
    onChange(next);
  };
  const scalars = visible.filter((k) => !isBlock(schema.properties![k]));
  const blocks = visible.filter((k) => isBlock(schema.properties![k]));
  return (
    <div className="fieldset">
      {scalars.length > 0 && (
        <div className="form-grid">
          {scalars.map((k) => {
            const ps = schema.properties![k];
            return (
              <div className={ps.items?.enum ? "field wide" : "field"} key={k}>
                <FieldLabel name={k} schema={ps} required={req.has(k)} />
                <Field
                  name={k}
                  schema={ps}
                  value={value[k]}
                  required={req.has(k)}
                  mode={mode}
                  onChange={(v) => set(k, v)}
                />
              </div>
            );
          })}
        </div>
      )}
      {blocks.map((k) => {
        const ps = schema.properties![k];
        return (
          <div className="field-block" key={k}>
            <FieldLabel name={k} schema={ps} required={req.has(k)} />
            {isObjectList(ps) ? (
              <ObjectList
                schema={ps}
                value={value[k]}
                onChange={(v) => set(k, v)}
              />
            ) : (
              <div className="nested-box">
                <FieldSet
                  schema={ps}
                  value={(value[k] as Obj) ?? {}}
                  mode={mode}
                  onChange={(v) => set(k, isSet(v) ? v : undefined)}
                />
              </div>
            )}
          </div>
        );
      })}
      {(hidden > 0 || all) && (
        <button
          type="button"
          className="link small show-all"
          onClick={() => setAll(!all)}
        >
          {all ? "show set options only" : `show all options (${hidden} more)`}
        </button>
      )}
    </div>
  );
}

// summary is an object list item's one-line description: its required
// fields, then other set scalars.
function summary(schema: Schema, v: Obj): string {
  const req = new Set(schema.required ?? []);
  const fmt = (x: unknown) =>
    Array.isArray(x) ? x.join(", ") : typeof x === "object" ? "…" : String(x);
  const parts: string[] = [];
  for (const k of order(schema)) {
    if (req.has(k) && isSet(v[k])) parts.push(fmt(v[k]));
  }
  for (const k of order(schema)) {
    const ps = schema.properties![k];
    if (req.has(k) || !isSet(v[k]) || isObjectList(ps)) continue;
    parts.push(v[k] === true ? label(k) : `${label(k)} ${fmt(v[k])}`);
  }
  return parts.join(" · ") || "(empty)";
}

// ObjectList edits a list of objects: one line per item, expanded to edit.
function ObjectList({
  schema,
  value,
  onChange,
}: {
  schema: Schema;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const items = Array.isArray(value) ? (value as Obj[]) : [];
  const item = schema.items ?? {};
  const [open, setOpen] = useState<number>();
  const set = (next: Obj[]) => onChange(next.length ? next : undefined);
  return (
    <div className="obj-list">
      {items.length === 0 && <div className="muted small-note">None.</div>}
      {items.map((it, i) => (
        <div className={`obj-item${open === i ? " open" : ""}`} key={i}>
          <div className="obj-item-head">
            <button
              type="button"
              className="obj-item-summary mono"
              onClick={() => setOpen(open === i ? undefined : i)}
              aria-expanded={open === i}
            >
              <span className="caret">{open === i ? "▾" : "▸"}</span>
              <span className="obj-item-text">{summary(item, it ?? {})}</span>
            </button>
            <button
              type="button"
              className="icon-btn"
              title="Duplicate"
              aria-label="Duplicate item"
              onClick={() => {
                set([
                  ...items.slice(0, i + 1),
                  structuredClone(it),
                  ...items.slice(i + 1),
                ]);
                setOpen(i + 1);
              }}
            >
              <svg
                width="13"
                height="13"
                viewBox="0 0 16 16"
                aria-hidden="true"
              >
                <rect
                  x="5"
                  y="5"
                  width="9"
                  height="9"
                  rx="1.5"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                />
                <path
                  d="M3.5 11H3a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1h7a1 1 0 0 1 1 1v.5"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth="1.5"
                />
              </svg>
            </button>
            <button
              type="button"
              className="icon-btn"
              title="Remove"
              aria-label="Remove item"
              onClick={() => {
                set(items.filter((_, j) => j !== i));
                setOpen(undefined);
              }}
            >
              ×
            </button>
          </div>
          {open === i && (
            <div className="obj-item-body">
              <FieldSet
                schema={item}
                value={it ?? {}}
                mode="config"
                onChange={(v) => set(items.map((x, j) => (j === i ? v : x)))}
              />
            </div>
          )}
        </div>
      ))}
      <button
        type="button"
        className="btn small"
        onClick={() => {
          set([...items, defaults(item)]);
          setOpen(items.length);
        }}
      >
        Add
      </button>
    </div>
  );
}

// Sectioned edits a big object: General (the scalars) and one section per
// object list or nested object, picked in a side list.
function Sectioned({
  schema,
  value,
  mode,
  onChange,
}: {
  schema: Schema;
  value: Obj;
  mode: FormMode;
  onChange: (v: Obj) => void;
}) {
  const keys = order(schema);
  const general = keys.filter((k) => !isBlock(schema.properties![k]));
  const blocks = keys.filter((k) => isBlock(schema.properties![k]));
  const sections = [...(general.length ? ["general"] : []), ...blocks];
  const [sel, setSel] = useState(sections[0]);
  const count = (k: string) => {
    const v = value[k];
    if (isObjectList(schema.properties![k]))
      return Array.isArray(v) ? v.length : 0;
    return isSet(v) ? "●" : "";
  };
  const set = (k: string, v: unknown) => {
    const next = { ...value };
    if (v === undefined) delete next[k];
    else next[k] = v;
    onChange(next);
  };
  const ps = sel === "general" ? undefined : schema.properties![sel];
  return (
    <div className="sectioned">
      <nav className="section-nav" aria-label="Sections">
        {sections.map((k) => (
          <button
            type="button"
            key={k}
            className={k === sel ? "active" : ""}
            onClick={() => setSel(k)}
          >
            <span>{k === "general" ? "general" : label(k)}</span>
            {k !== "general" && count(k) !== "" && count(k) !== 0 && (
              <span className="section-count">{count(k)}</span>
            )}
          </button>
        ))}
      </nav>
      <div className="section-pane">
        {ps?.description && (
          <div className="muted small-note section-desc">{ps.description}</div>
        )}
        {sel === "general" ? (
          <FieldSet
            key="general"
            schema={schema}
            value={value}
            mode={mode}
            onChange={onChange}
            keys={general}
          />
        ) : isObjectList(ps!) ? (
          <ObjectList
            key={sel}
            schema={ps!}
            value={value[sel]}
            onChange={(v) => set(sel, v)}
          />
        ) : (
          <FieldSet
            key={sel}
            schema={ps!}
            value={(value[sel] as Obj) ?? {}}
            mode={mode}
            onChange={(v) => set(sel, isSet(v) ? v : undefined)}
          />
        )}
      </div>
    </div>
  );
}

// SchemaForm renders a form for a schema with a raw-JSON escape hatch.
// Schemas with lists of objects or nested objects get sections.
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
  const empty =
    !schema.properties || Object.keys(schema.properties).length === 0;
  const sectioned =
    !empty &&
    order(schema).filter((k) => isBlock(schema.properties![k])).length > 0;
  return (
    <div className="schema-form">
      <div className="form-toolbar">
        <button
          type="button"
          className="link"
          onClick={() => {
            if (!raw) setText(JSON.stringify(value, null, 2));
            setErr(undefined);
            setRaw(!raw);
          }}
        >
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
                if (typeof v !== "object" || Array.isArray(v) || v === null)
                  throw new Error("must be a JSON object");
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
      ) : sectioned ? (
        <Sectioned
          schema={schema}
          value={value}
          mode={mode}
          onChange={onChange}
        />
      ) : (
        <FieldSet
          schema={schema}
          value={value}
          mode={mode}
          onChange={onChange}
        />
      )}
    </div>
  );
}
