import { createContext, useContext, useEffect, useMemo, useState } from "react";
import { parse as parseYAML, stringify as toYAML } from "yaml";
import { StepInput, fmtStep, isStep } from "./Stepped";

// A JSON schema as published by the agent (invopop/jsonschema output plus
// "default" values and "x-order"), with hints from the Go config's
// jsonschema_extras tags: x-group (a field's group heading), x-advanced
// (kept back until "show all options"), x-common (always shown, even where
// a large form keeps back what isn't set or required), x-summary (an object list's item
// line, e.g. "{address} AS {remote_as}[ · {description}]": bracketed parts
// only when their fields are set) and x-show-if ("type=a|b" or "type!=c":
// the field applies only for those values of a sibling field).
export type Schema = {
  "x-group"?: string;
  "x-advanced"?: boolean | string;
  "x-common"?: boolean | string;
  "x-summary"?: string;
  "x-show-if"?: string;
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

const isAdvanced = (s: Schema) =>
  s["x-advanced"] === true || s["x-advanced"] === "true";
const isCommon = (s: Schema) => s["x-common"] === true || s["x-common"] === "true";

// applies evaluates a field's x-show-if against its siblings (an unset
// sibling counts as its default).
function applies(ps: Schema, parent: Schema, value: Obj): boolean {
  const cond = ps["x-show-if"];
  if (!cond) return true;
  const m = cond.match(/^(\w+)(!?=)(.*)$/);
  if (!m) return true;
  const [, field, op, vals] = m;
  const cur = String(value[field] ?? parent.properties?.[field]?.default ?? "");
  const hit = vals.split("|").includes(cur);
  return op === "=" ? hit : !hit;
}

// The schema under a block: a nested object's own, an object list's items.
const inner = (s: Schema) =>
  isObjectList(s) ? s.items! : isObject(s) ? s : undefined;

// matches: a field's name, group or description mentions q (lower case).
function matches(k: string, s: Schema, q: string): boolean {
  return [label(k), k, s["x-group"] ?? "", s.description ?? ""].some((t) =>
    t.toLowerCase().includes(q),
  );
}

// hits counts the fields matching q in k and below it.
function hits(k: string, s: Schema, q: string): number {
  let n = matches(k, s, q) ? 1 : 0;
  for (const [ck, cs] of Object.entries(inner(s)?.properties ?? {}))
    n += hits(ck, cs, q);
  return n;
}

// errorKeys: the field names an agent error mentions, e.g. "peer 10.0.0.2:
// remote_as is required" → peers, remote_as (singular words name lists).
function errorKeys(s: Schema, text: string): Set<string> {
  const words = new Set(text.toLowerCase().split(/[^a-z0-9_]+/));
  const out = new Set<string>();
  const walk = (s: Schema) => {
    for (const [k, p] of Object.entries(s.properties ?? {})) {
      if (
        [k, k.replace(/ies$/, "y"), k.replace(/s$/, "")].some((w) =>
          words.has(w),
        )
      )
        out.add(k);
      const i = inner(p);
      if (i) walk(i);
    }
  };
  walk(s);
  return out;
}

// errorIn: k or a field below it is one an error mentions.
function errorIn(k: string, s: Schema, keys: Set<string>): boolean {
  if (keys.has(k)) return true;
  return Object.entries(inner(s)?.properties ?? {}).some(([ck, cs]) =>
    errorIn(ck, cs, keys),
  );
}

// mentions: the error names one of an item's values ("peer 10.0.0.2").
function mentions(text: string, item: Obj): boolean {
  return Object.values(item).some((v) => {
    if (typeof v !== "string" || v.length < 3) return false;
    const re = v.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return new RegExp(`(^|[^\\w.])${re}($|[^\\w.])`).test(text);
  });
}

// errorScope: the object lists holding an item the error names by value,
// e.g. peers for "peer 10.0.0.2: …". The error is then about those items
// only, not every field of the same name.
function errorScope(
  schema: Schema,
  value: Obj,
  text: string,
  keys: Set<string>,
) {
  const out = new Set<string>();
  for (const [k, ps] of Object.entries(schema.properties ?? {})) {
    const v = value[k];
    if (keys.has(k) && isObjectList(ps) && Array.isArray(v))
      if (v.some((it) => mentions(text, (it ?? {}) as Obj))) out.add(k);
  }
  return out;
}

// The form's view state, shared by every nested field set. errKeys are
// the field names to mark here: all of allErrKeys, or (with a scope) none
// outside the items the error names.
type FormCtx = {
  query: string; // field filter, lower case
  errText: string;
  errKeys: Set<string>;
  allErrKeys: Set<string>;
  scope: Set<string>;
  // A device group template: number and text fields can step per member.
  stepping: boolean;
};
const none = new Set<string>();
const Ctx = createContext<FormCtx>({
  query: "",
  errText: "",
  errKeys: none,
  allErrKeys: none,
  scope: none,
  stepping: false,
});

// steppable: a field a group template can step (integers, and text that
// may hold an address, prefix or MAC).
const steppable = (s: Schema) => {
  const t = typeOf(s);
  return !s.enum && (t === "integer" || t === "number" || t === "string");
};

// canon is a stable JSON text (keys sorted) for comparing values.
function canon(v: unknown): string {
  if (Array.isArray(v)) return `[${v.map(canon).join(",")}]`;
  if (v && typeof v === "object")
    return `{${Object.keys(v)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${canon((v as Obj)[k])}`)
      .join(",")}}`;
  return JSON.stringify(v) ?? "null";
}

// normalize drops what says nothing: unset values, false booleans that
// default to false and values equal to their default, so configs compare
// by meaning.
export function normalize(schema: Schema, v: Obj): Obj {
  const out: Obj = {};
  for (const [k, x] of Object.entries(v ?? {})) {
    const ps = schema.properties?.[k];
    const n = ps ? normField(ps, x) : x;
    if (n !== undefined) out[k] = n;
  }
  return out;
}

function normField(ps: Schema, x: unknown): unknown {
  if (!isSet(x)) return undefined;
  if (x === false && ps.default !== true) return undefined;
  if (ps.default !== undefined && canon(x) === canon(ps.default))
    return undefined;
  if (isObject(ps) && typeof x === "object" && !Array.isArray(x)) {
    const n = normalize(ps, x as Obj);
    return Object.keys(n).length ? n : undefined;
  }
  if (isObjectList(ps) && Array.isArray(x))
    return x.map((it) => normalize(ps.items!, (it ?? {}) as Obj));
  return x;
}

export type Change = { path: string; from?: string; to?: string };

const show = (x: unknown) =>
  x === undefined ? undefined : typeof x === "string" ? x : JSON.stringify(x);

// diff lists what changed from a to b, by field path ("peers[1].families").
export function diff(schema: Schema, a: Obj, b: Obj): Change[] {
  const out: Change[] = [];
  const walk = (s: Schema, a: Obj, b: Obj, at: string) => {
    const keys = [
      ...order(s),
      ...[...Object.keys(a), ...Object.keys(b)].filter(
        (k) => !s.properties?.[k],
      ),
    ];
    for (const k of new Set(keys)) {
      const ps = s.properties?.[k] ?? {};
      const x = a[k];
      const y = b[k];
      if (canon(x) === canon(y)) continue;
      const path = at + k;
      if (isObject(ps)) {
        walk(ps, (x ?? {}) as Obj, (y ?? {}) as Obj, path + ".");
      } else if (isObjectList(ps)) {
        const xs = (x ?? []) as Obj[];
        const ys = (y ?? []) as Obj[];
        for (let i = 0; i < Math.max(xs.length, ys.length); i++) {
          const p = `${path}[${i}]`;
          if (i >= ys.length)
            out.push({ path: p, from: summary(ps, ps.items!, xs[i]) });
          else if (i >= xs.length)
            out.push({ path: p, to: summary(ps, ps.items!, ys[i]) });
          else walk(ps.items!, xs[i], ys[i], p + ".");
        }
      } else {
        // An unset field shows its default, when the schema has one.
        const dflt = (v: unknown) =>
          v === undefined && ps.default !== undefined
            ? `${show(ps.default)} (default)`
            : show(v);
        out.push({ path, from: dflt(x), to: dflt(y) });
      }
    }
  };
  walk(schema, normalize(schema, a), normalize(schema, b), "");
  return out;
}

// The default a description mentions ("Default: the device's address"),
// for placeholders.
function defaultHint(s: Schema): string {
  if (s.default !== undefined) return String(s.default);
  const m = s.description?.match(/Default:?\s*([^.;]+)/i);
  const hint = m ? m[1].trim() : "";
  // An enum's default is one of its values ("Default mpls (…)" → mpls).
  if (s.enum && hint)
    return String(s.enum.find((x) => hint.startsWith(String(x))) ?? "");
  return hint;
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
      {
        // In a config an unset boolean is its default (a group template
        // leaves defaults out).
        const on =
          value === true ||
          (value === undefined && mode === "config" && schema.default === true);
        return (
          <label className="switch">
            <input
              type="checkbox"
              checked={on}
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
            <span>{on ? "on" : "off"}</span>
          </label>
        );
      }
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
  const ctx = useContext(Ctx);
  const { query, errKeys, stepping } = ctx;
  const req = new Set(schema.required ?? []);
  // Fields that don't apply (x-show-if) aren't offered at all; a filter
  // keeps the fields matching it (or holding fields that do).
  const ks = (keys ?? order(schema)).filter(
    (k) =>
      applies(schema.properties![k], schema, value) &&
      (!query || hits(k, schema.properties![k], query) > 0),
  );
  const adv = (k: string) => isAdvanced(schema.properties![k]);
  // Small forms (most action arguments) show everything but advanced
  // fields; fields an error names always show.
  const shown = (k: string) =>
    !!query ||
    all ||
    req.has(k) ||
    isCommon(schema.properties![k]) ||
    isSet(value[k]) ||
    errorIn(k, schema.properties![k], errKeys) ||
    (ks.length <= 6 && !adv(k));
  let visible = ks.filter(shown);
  // Nothing set yet: show the first few so there is something to fill.
  if (!all && !query && visible.length === 0)
    visible = ks.filter((k) => !adv(k)).slice(0, 4);
  const hidden = ks.length - visible.length;
  const set = (k: string, v: unknown) => {
    const next = { ...value };
    if (v === undefined) delete next[k];
    else next[k] = v;
    onChange(next);
  };
  const scalars = visible.filter((k) => !isBlock(schema.properties![k]));
  const blocks = visible.filter((k) => isBlock(schema.properties![k]));
  // Scalars by x-group, in order of first appearance (ungrouped first).
  const groups: [string, string[]][] = [];
  for (const k of scalars) {
    const g = schema.properties![k]["x-group"] ?? "";
    const at = groups.find(([n]) => n === g);
    if (at) at[1].push(k);
    else if (g === "") groups.unshift([g, [k]]);
    else groups.push([g, [k]]);
  }
  const headed =
    groups.length > 1 || (groups.length === 1 && groups[0][0] !== "");
  return (
    <div className="fieldset">
      {groups.map(([g, gks]) => (
        <div className="field-group" key={g}>
          {headed && g && <div className="field-group-head">{g}</div>}
          <div className="form-grid">
            {gks.map((k) => {
              const ps = schema.properties![k];
              return (
                <div
                  className={`field${ps.items?.enum ? " wide" : ""}${errKeys.has(k) ? " error" : ""}`}
                  key={k}
                >
                  <FieldLabel name={k} schema={ps} required={req.has(k)} />
                  {stepping && steppable(ps) ? (
                    <StepInput
                      value={value[k]}
                      numeric={typeOf(ps) !== "string"}
                      label={label(k)}
                      onChange={(v) => set(k, v)}
                    >
                      <Field
                        name={k}
                        schema={ps}
                        value={value[k]}
                        required={req.has(k)}
                        mode={mode}
                        onChange={(v) => set(k, v)}
                      />
                    </StepInput>
                  ) : (
                    <Field
                      name={k}
                      schema={ps}
                      value={value[k]}
                      required={req.has(k)}
                      mode={mode}
                      onChange={(v) => set(k, v)}
                    />
                  )}
                </div>
              );
            })}
          </div>
        </div>
      ))}
      {blocks.map((k) => {
        const ps = schema.properties![k];
        // A block the filter names shows whole.
        const whole = !!query && matches(k, ps, query);
        return (
          <div
            className={`field-block${errKeys.has(k) ? " error" : ""}`}
            key={k}
          >
            <FieldLabel name={k} schema={ps} required={req.has(k)} />
            <Ctx.Provider value={whole ? { ...ctx, query: "" } : ctx}>
              {isObjectList(ps) ? (
                <ObjectList
                  name={k}
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
            </Ctx.Provider>
          </div>
        );
      })}
      {query && ks.length === 0 && (
        <div className="muted small-note">No fields match.</div>
      )}
      {!query && (hidden > 0 || all) && (
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

const fmt = (x: unknown): string =>
  Array.isArray(x)
    ? x.map(fmt).join(", ")
    : isStep(x)
      ? fmtStep(x)
      : typeof x === "object"
        ? "…"
        : String(x);

// fill renders an x-summary template: {field} is the field's value (a set
// boolean its name), [ … ] a part shown only when its fields are all set.
function fill(tmpl: string, v: Obj): string {
  const val = (k: string) => (v[k] === true ? label(k) : fmt(v[k]));
  const out = tmpl.replace(/\[([^\]]*)\]/g, (_, part: string) => {
    const ks = [...part.matchAll(/\{(\w+)\}/g)].map((m) => m[1]);
    return ks.every((k) => isSet(v[k]) && v[k] !== false)
      ? part.replace(/\{(\w+)\}/g, (_m, k: string) => val(k))
      : "";
  });
  return out
    .replace(/\{(\w+)\}/g, (_m, k: string) => (isSet(v[k]) ? val(k) : ""))
    .replace(/\s+/g, " ")
    .trim();
}

// summary is an object list item's one-line description: the list's
// x-summary, else its required fields, then other set scalars.
function summary(list: Schema, schema: Schema, v: Obj): string {
  if (list["x-summary"]) return fill(list["x-summary"], v) || "(empty)";
  const req = new Set(schema.required ?? []);
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
  name,
  schema,
  value,
  onChange,
}: {
  name: string;
  schema: Schema;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  const items = Array.isArray(value) ? (value as Obj[]) : [];
  const item = schema.items ?? {};
  const [open, setOpen] = useState<number>();
  // Items an error names ("peer 10.0.0.2: …") are marked and the first
  // is opened.
  const ctx = useContext(Ctx);
  const { errText, allErrKeys, scope } = ctx;
  const bad = (it: Obj) =>
    !!errText &&
    (scope.size ? scope.has(name) : errorIn(name, schema, allErrKeys)) &&
    mentions(errText, it ?? {});
  useEffect(() => {
    const i = items.findIndex(bad);
    if (i >= 0) setOpen(i);
  }, [errText]); // eslint-disable-line react-hooks/exhaustive-deps
  const set = (next: Obj[]) => onChange(next.length ? next : undefined);
  return (
    <div className="obj-list">
      {items.length === 0 && <div className="muted small-note">None.</div>}
      {items.map((it, i) => (
        <div
          className={`obj-item${open === i ? " open" : ""}${bad(it) ? " error" : ""}`}
          key={i}
        >
          <div className="obj-item-head">
            <button
              type="button"
              className="obj-item-summary mono"
              onClick={() => setOpen(open === i ? undefined : i)}
              aria-expanded={open === i}
            >
              <span className="caret">{open === i ? "▾" : "▸"}</span>
              <span className="obj-item-text">
                {summary(schema, item, it ?? {})}
              </span>
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
              <Ctx.Provider
                value={bad(it) ? { ...ctx, errKeys: allErrKeys } : ctx}
              >
                <FieldSet
                  schema={item}
                  value={it ?? {}}
                  mode="config"
                  onChange={(v) => set(items.map((x, j) => (j === i ? v : x)))}
                />
              </Ctx.Provider>
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
// object list or nested object, picked in a side list. Each section shows
// its item count (or filter hits) and whether it changed from original or
// has a field an error names.
function Sectioned({
  schema,
  value,
  original,
  mode,
  onChange,
}: {
  schema: Schema;
  value: Obj;
  original?: Obj;
  mode: FormMode;
  onChange: (v: Obj) => void;
}) {
  const ctx = useContext(Ctx);
  const { query, allErrKeys, scope } = ctx;
  const keys = order(schema);
  const general = keys.filter((k) => !isBlock(schema.properties![k]));
  const blocks = keys.filter((k) => isBlock(schema.properties![k]));
  const sections = [...(general.length ? ["general"] : []), ...blocks];
  const [sel, setSel] = useState(sections[0]);
  const keysOf = (k: string) => (k === "general" ? general : [k]);
  const count = (k: string) => {
    if (k === "general") return "";
    const v = value[k];
    if (isObjectList(schema.properties![k]))
      return Array.isArray(v) ? v.length : 0;
    return isSet(v) ? "●" : "";
  };
  const hitsOf = (k: string) =>
    keysOf(k).reduce((n, f) => n + hits(f, schema.properties![f], query), 0);
  const changed = (k: string) =>
    !!original &&
    keysOf(k).some((f) => {
      const ps = schema.properties![f];
      return (
        canon(normField(ps, value[f])) !== canon(normField(ps, original[f]))
      );
    });
  const failed = (k: string) =>
    scope.size
      ? scope.has(k)
      : keysOf(k).some((f) => errorIn(f, schema.properties![f], allErrKeys));
  // Follow the filter or an error to a section that has something.
  useEffect(() => {
    if (query && hitsOf(sel) === 0) {
      const to = sections.find((k) => hitsOf(k) > 0);
      if (to) setSel(to);
    }
  }, [query]); // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => {
    if (allErrKeys.size && !failed(sel)) {
      const to = sections.find(failed);
      if (to) setSel(to);
    }
  }, [allErrKeys]); // eslint-disable-line react-hooks/exhaustive-deps
  const set = (k: string, v: unknown) => {
    const next = { ...value };
    if (v === undefined) delete next[k];
    else next[k] = v;
    onChange(next);
  };
  const ps = sel === "general" ? undefined : schema.properties![sel];
  // A section the filter names shows whole.
  const whole = !!query && !!ps && matches(sel, ps, query);
  return (
    <div className="sectioned">
      <nav className="section-nav" aria-label="Sections">
        {sections.map((k) => {
          const n = query ? hitsOf(k) : count(k);
          const cls = [
            k === sel ? "active" : "",
            query && n === 0 ? "dim" : "",
            failed(k) ? "error" : "",
          ];
          return (
            <button
              type="button"
              key={k}
              className={cls.filter(Boolean).join(" ")}
              onClick={() => setSel(k)}
            >
              <span>{k === "general" ? "general" : label(k)}</span>
              <span className="section-marks">
                {changed(k) && (
                  <span className="section-changed" title="changed">
                    ●
                  </span>
                )}
                {n !== "" && n !== 0 && (
                  <span className="section-count">{n}</span>
                )}
              </span>
            </button>
          );
        })}
      </nav>
      <div className="section-pane">
        {ps?.description && (
          <div className="muted small-note section-desc">{ps.description}</div>
        )}
        <Ctx.Provider value={whole ? { ...ctx, query: "" } : ctx}>
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
              name={sel}
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
        </Ctx.Provider>
      </div>
    </div>
  );
}

const countFields = (s: Schema): number =>
  Object.entries(s.properties ?? {}).reduce(
    (n, [, p]) => n + 1 + (inner(p) ? countFields(inner(p)!) : 0),
    0,
  );

// SchemaForm renders a form for a schema with a raw-YAML escape hatch.
// Schemas with lists of objects or nested objects get sections. Big forms
// get a field filter. original (the config being edited) marks changed
// sections; errorText (the agent's rejection) marks the fields it names.
// SchemaFields is a bare set of fields for an object (no toolbar), for
// editors that lay out several objects of their own.
export function SchemaFields({ schema, value, onChange, mode = "config" }: { schema: Schema; value: Obj; onChange: (v: Obj) => void; mode?: FormMode }) {
  return <FieldSet schema={schema} value={value} mode={mode} onChange={onChange} />;
}

export function SchemaForm({
  schema,
  value,
  onChange,
  mode,
  original,
  errorText = "",
  stepping = false,
}: {
  schema: Schema;
  value: Obj;
  onChange: (v: Obj) => void;
  mode: FormMode;
  original?: Obj;
  errorText?: string;
  stepping?: boolean;
}) {
  const [raw, setRaw] = useState(false);
  const [text, setText] = useState("");
  const [err, setErr] = useState<string>();
  const [query, setQuery] = useState("");
  const empty =
    !schema.properties || Object.keys(schema.properties).length === 0;
  const sectioned =
    !empty &&
    order(schema).filter((k) => isBlock(schema.properties![k])).length > 0;
  const errKeys = useMemo(
    () => errorKeys(schema, errorText),
    [schema, errorText],
  );
  const q = query.trim().toLowerCase();
  const scope = useMemo(
    () => errorScope(schema, value, errorText, errKeys),
    [schema, value, errorText, errKeys],
  );
  const ctx = useMemo(
    () => ({
      query: q,
      errText: errorText,
      errKeys: scope.size ? none : errKeys,
      allErrKeys: errKeys,
      scope,
      stepping,
    }),
    [q, errorText, errKeys, scope, stepping],
  );
  return (
    <div className="schema-form">
      <div className="form-toolbar">
        {!raw && !empty && countFields(schema) > 12 && (
          <input
            className="input filter"
            type="search"
            placeholder="find a field"
            aria-label="Find a field"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        )}
        <button
          type="button"
          className="link"
          onClick={() => {
            if (!raw) setText(Object.keys(value).length ? toYAML(value) : "");
            setErr(undefined);
            setRaw(!raw);
          }}
        >
          {raw ? "form view" : "edit as YAML"}
        </button>
      </div>
      {raw ? (
        <>
          <textarea
            className="input mono code"
            aria-label="Configuration as YAML"
            rows={Math.min(24, Math.max(6, text.split("\n").length + 1))}
            value={text}
            spellCheck={false}
            onChange={(e) => {
              setText(e.target.value);
              try {
                const v = parseYAML(e.target.value) ?? {};
                if (typeof v !== "object" || Array.isArray(v))
                  throw new Error("must be a mapping of fields");
                setErr(undefined);
                onChange(v as Obj);
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
        <Ctx.Provider value={ctx}>
          {sectioned ? (
            <Sectioned
              schema={schema}
              value={value}
              original={original}
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
        </Ctx.Provider>
      )}
    </div>
  );
}
