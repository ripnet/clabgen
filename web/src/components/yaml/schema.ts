// Schema help for the YAML editor, without the editor: where the cursor is
// (a path of keys and list items from the top of the document), which
// schema applies there, what to offer, and what's wrong with a document.
//
// Schemas are the agent's (see SchemaForm's Schema). Two hints come from
// the scenario schema: "x-config-of": "type" (a protocol's config takes the
// schema of the protocol type its sibling type field names) and
// "x-template" (a device group: values may be templates, {i} in text and
// {start, step, ...} for numbers and addresses).

import { isMap, isScalar, isSeq, parseDocument, type Node as YNode, type Pair } from "yaml";
import type { Schema } from "../SchemaForm";

export type Ctx = {
  root: Schema;
  // Protocol type name → its config schema.
  types?: Record<string, Schema>;
  // Values are templates throughout (a group's protocol edited on its own).
  template?: boolean;
};

// The schema at a node, and whether templates apply there.
export type At = { schema: Schema; template: boolean };

const typeOf = (s: Schema): string =>
  Array.isArray(s.type) ? (s.type.find((t) => t !== "null") ?? "string") : (s.type ?? "");

// child: the schema of key k (or a list item, k a number) under at.
// sibling reads another field of the same mapping (for x-config-of).
export function child(ctx: Ctx, at: At, k: string | number, sibling: (key: string) => unknown): At | undefined {
  const s = at.schema;
  const template = at.template || !!s["x-template"];
  if (typeof k === "number") return s.items ? { schema: s.items, template } : undefined;
  let c = s.properties?.[k];
  if (!c && s.additionalProperties && typeof s.additionalProperties === "object") c = s.additionalProperties;
  if (!c) return undefined;
  const by = c["x-config-of"];
  if (by) {
    const t = sibling(by);
    const ts = typeof t === "string" ? ctx.types?.[t] : undefined;
    if (ts) return { schema: { ...ts, description: c.description }, template };
  }
  return { schema: c, template: template || !!c["x-template"] };
}

// ---------------------------------------------------------------------------
// Where the cursor is, from the text alone: a document being typed often
// doesn't parse, so this reads indentation, dashes and colons.

type Line = { dashes: number[]; key?: string; keyAt: number; opens: boolean; text: string; blank: boolean };

// readLine: a line's list dashes ("- - x" has two), its key if it starts
// "key:" (at column keyAt, after the dashes), and whether the key opens a
// block (nothing after the colon).
function readLine(text: string): Line {
  let i = 0;
  while (text[i] === " ") i++;
  const dashes: number[] = [];
  while (text[i] === "-" && (text[i + 1] === " " || i + 1 === text.length)) {
    dashes.push(i);
    i += 2;
    while (text[i] === " ") i++;
  }
  const body = text.slice(i);
  const blank = !dashes.length && (body.trim() === "" || body.startsWith("#"));
  const m = body.match(/^("[^"]*"|'[^']*'|[^\s:#{}[\],'"][^:#{}]*?)\s*:(?:\s+(.*))?$/);
  if (!m) return { dashes, keyAt: i, opens: false, text, blank };
  const rest = (m[2] ?? "").replace(/(^|\s+)#.*$/, "").trim();
  return { dashes, key: unquote(m[1]), keyAt: i, opens: rest === "" || rest === "|" || rest === ">", text, blank };
}

const unquote = (s: string) => s.replace(/^["']|["']$/g, "");

// A path step: a key, or a list item: its index, the line it starts on and
// the column its fields start at (so its other fields can be read).
export type Step = { key: string } | { item: number; line: number; col: number };

// blockPath: the steps to the mapping that holds column col of line at.
// On line at itself only its dashes count.
function blockPath(lines: Line[], at: number, col: number): Step[] {
  const out: Step[] = [];
  // Right after an item, the list's key may be at the dash's own column
  // (go-yaml writes lists unindented).
  let eq = false;
  let item: { line: number; col: number; dash: number; n: number } | undefined;
  const close = () => {
    if (item) out.unshift({ item: item.n, line: item.line, col: item.col });
    item = undefined;
  };
  for (let n = at; n >= 0 && (col > 0 || eq); n--) {
    const l = lines[n];
    if (l.blank) continue;
    if (n !== at && l.key !== undefined && l.opens && (l.keyAt < col || (eq && l.keyAt === col && !l.dashes.length))) {
      close();
      out.unshift({ key: l.key });
      col = l.keyAt;
      eq = false;
    }
    for (let d = l.dashes.length - 1; d >= 0; d--) {
      const dash = l.dashes[d];
      if (item && eq && dash === col) {
        item.n++; // an earlier item of the same list
        break;
      }
      if (dash >= col) continue;
      close();
      item = { line: n, col: d + 1 < l.dashes.length ? l.dashes[d + 1] : l.keyAt, dash, n: 0 };
      col = dash;
      eq = true;
    }
  }
  close();
  return out;
}

// itemField reads field k of the list item that starts on line n with its
// fields at column col: a "k: v" line of the item, or k in a flow mapping
// on its first line.
function itemField(lines: Line[], n: number, col: number, k: string): string | undefined {
  for (let m = n; m < lines.length; m++) {
    const l = lines[m];
    if (m > n && !l.blank && (l.keyAt < col || (l.dashes.length > 0 && l.dashes[0] < col))) break;
    if (l.keyAt === col && l.key === k) return scalarAfter(l) || undefined;
  }
  const f = lines[n].text.match(new RegExp(`[{,]\\s*${k}\\s*:\\s*["']?([^,}\\s"']+)`));
  return f?.[1];
}

const scalarAfter = (l: Line) => unquote(l.text.slice(l.text.indexOf(":", l.keyAt) + 1).replace(/\s+#.*$/, "").trim());

// flowPath reads the flow collections still open before the cursor:
// "{name: a, config: {rate: 1, " → collections {…} and config {…}, at a key.
type Open = { key?: string; brace: boolean; present: string[] };
function flowPath(prefix: string): { stack: Open[]; atKey: boolean; key?: string } {
  const stack: Open[] = [];
  let key: string | undefined;
  let word = "";
  let quote = "";
  let atKey = false;
  for (const ch of prefix) {
    if (quote) {
      if (ch === quote) quote = "";
      word += ch;
    } else if (ch === '"' || ch === "'") {
      quote = ch;
      word += ch;
    } else if (ch === "{" || ch === "[") {
      stack.push({ key, brace: ch === "{", present: [] });
      key = undefined;
      word = "";
      atKey = ch === "{";
    } else if (ch === "}" || ch === "]") {
      stack.pop();
      word = "";
      key = undefined;
      atKey = false;
    } else if (ch === ":" && atKey) {
      key = unquote(word.trim());
      stack.at(-1)?.present.push(key);
      word = "";
      atKey = false;
    } else if (ch === ",") {
      word = "";
      key = undefined;
      atKey = !!stack.at(-1)?.brace;
    } else word += ch;
  }
  return { stack, atKey, key };
}

export type Place = {
  // Steps to the mapping (or list) the cursor is in.
  path: Step[];
  // At a key (offer the mapping's keys) or at the value of key.
  kind: "key" | "value";
  key?: string;
  // The word being typed and where it starts on the line.
  word: string;
  from: number;
  // Keys already in the mapping.
  present: string[];
};

// placeAt: what the cursor at (line, column), both from 0, is on.
export function placeAt(text: string, line: number, column: number): Place {
  const lines = text.split("\n").map(readLine);
  const prefix = lines[line].text.slice(0, column);
  const word = prefix.match(/[\w.\-/]*$/)![0];
  const from = column - word.length;
  const pre = readLine(prefix);
  lines[line] = pre;
  const fl = flowPath(prefix.slice(pre.keyAt));
  const path = blockPath(lines, line, pre.keyAt);
  if (fl.stack.length) {
    // "key: {…" (or a dashed "- {…" item, whose step is in path already).
    const own = prefix.slice(pre.keyAt).match(/^([^\s:#{}[\],'"][^:#{}]*?)\s*:\s*[{[]/);
    if (own) path.push({ key: unquote(own[1]) });
    fl.stack.forEach((o, i) => {
      if (i === 0) return;
      const up = fl.stack[i - 1];
      path.push(up.brace ? { key: o.key ?? "" } : { item: 0, line, col: -1 });
    });
    const top = fl.stack.at(-1)!;
    if (!top.brace) {
      path.push({ item: 0, line, col: -1 });
      return { path, kind: "value", key: fl.stack.length > 1 ? fl.stack.at(-1)?.key : own?.[1], word, from, present: [] };
    }
    if (fl.atKey) return { path, kind: "key", word, from, present: top.present.filter((p) => p !== word) };
    return { path, kind: "value", key: fl.key, word, from, present: top.present };
  }
  const present = siblings(lines, line, pre.keyAt);
  if (pre.key !== undefined && /:\s/.test(prefix.slice(pre.keyAt))) return { path, kind: "value", key: pre.key, word, from, present };
  return { path, kind: "key", word, from, present: present.filter((p) => p !== word) };
}

// siblings lists the keys of the mapping whose keys sit at column col
// around line n.
function siblings(lines: Line[], n: number, col: number): string[] {
  const out: string[] = [];
  const left = (l: Line) => l.keyAt < col || (l.dashes.length > 0 && l.dashes[0] < col);
  // Upward, unless line n starts a list item (its first field).
  if (!(lines[n].dashes.length && lines[n].keyAt === col))
    for (let m = n - 1; m >= 0; m--) {
      const l = lines[m];
      if (l.blank) continue;
      if (l.keyAt === col && l.key !== undefined) out.push(l.key);
      if (l.dashes.length && l.keyAt === col) break; // the item's first line
      if (left(l)) break;
    }
  for (let m = n + 1; m < lines.length; m++) {
    const l = lines[m];
    if (l.blank) continue;
    if (left(l)) break;
    if (l.keyAt === col && l.key !== undefined) out.push(l.key);
  }
  return out;
}

// resolve walks the schema along a path from placeAt, and on to key if
// given.
export function resolve(ctx: Ctx, text: string, path: Step[], key?: string): At | undefined {
  const lines = text.split("\n").map(readLine);
  let at: At | undefined = { schema: ctx.root, template: !!ctx.template };
  let item: { line: number; col: number } | undefined;
  const steps: Step[] = key === undefined ? path : [...path, { key }];
  for (const st of steps) {
    if (!at) return undefined;
    if ("item" in st) {
      at = child(ctx, at, st.item, () => undefined);
      item = st.col >= 0 ? { line: st.line, col: st.col } : undefined;
      continue;
    }
    const it = item;
    at = child(ctx, at, st.key, (k) => (it ? itemField(lines, it.line, it.col, k) : topField(lines, k)));
    item = undefined;
  }
  return at;
}

const topField = (lines: Line[], k: string) => {
  const l = lines.find((l) => l.keyAt === 0 && !l.dashes.length && l.key === k);
  return l ? scalarAfter(l) : undefined;
};

// hoverAt: the field whose key a position is on, with its schema. (Over
// a value, the editor shows that value's problems instead.)
export function hoverAt(ctx: Ctx, text: string, line: number, column: number): { key: string; at: At; from: number; to: number } | undefined {
  const l = readLine(text.split("\n")[line] ?? "");
  if (l.key === undefined) return undefined;
  const to = l.keyAt + l.text.slice(l.keyAt).indexOf(":");
  if (column < l.keyAt || column > to) return undefined;
  const p = placeAt(text, line, l.keyAt);
  const at = resolve(ctx, text, p.path, l.key);
  if (!at) return undefined;
  return { key: l.key, at, from: l.keyAt, to };
}

// ---------------------------------------------------------------------------
// Offers.

export type Offer = { label: string; apply: string; detail?: string; info?: string; boost?: number };

export const brief = (s: Schema): string => {
  const t = typeOf(s);
  if (s.enum) return s.enum.join(" | ");
  if (t === "array") return s.items ? `list of ${typeOf(s.items) === "object" ? "entries" : typeOf(s.items) || "values"}` : "list";
  if (t === "object") return s.properties ? "fields" : "mapping";
  return t;
};

// describe: a field's help: its description, then type, default and range.
export function describe(s: Schema): { text?: string; facts: string } {
  const facts: string[] = [brief(s)];
  const d = s.default;
  if (d !== undefined && d !== "" && !(Array.isArray(d) && !d.length) && typeof d !== "object") facts.push(`default ${d}`);
  const lo = s.minimum;
  const hi = s.maximum;
  if (lo !== undefined && hi !== undefined) facts.push(`${lo}–${hi}`);
  else if (hi !== undefined) facts.push(`≤ ${hi}`);
  else if (lo !== undefined) facts.push(`≥ ${lo}`);
  return { text: s.description, facts: facts.filter(Boolean).join(" · ") };
}

const isFlag = (s: Schema | undefined, f: "x-common" | "x-advanced") => s?.[f] === true || s?.[f] === "true";

// keyOffers: the keys a mapping takes, those already there left out;
// required and common ones rank first, advanced ones last.
export function keyOffers(at: At, present: string[]): Offer[] {
  const s = at.schema.items && typeOf(at.schema) === "array" ? at.schema.items : at.schema;
  const req = new Set(s.required ?? []);
  const keys = s["x-order"] ?? Object.keys(s.properties ?? {});
  return keys
    .filter((k) => s.properties?.[k] && !present.includes(k))
    .map((k, i) => {
      const p = s.properties![k];
      const d = describe(p);
      return {
        label: k,
        apply: `${k}: `,
        detail: (req.has(k) ? "required · " : "") + d.facts,
        info: d.text,
        boost: (req.has(k) ? 40 : isFlag(p, "x-common") ? 20 : isFlag(p, "x-advanced") ? -20 : 0) - i / 100,
      };
    });
}

// valueOffers: the values a field takes: its enum, true/false, its default,
// and for a protocol's device the devices in the document.
export function valueOffers(at: At, key: string | undefined, text: string): Offer[] {
  const s = at.schema.items && typeOf(at.schema) === "array" ? at.schema.items : at.schema;
  const out: Offer[] = [];
  const t = typeOf(s);
  const def = (v: unknown) => (s.default !== undefined && String(s.default) === String(v) ? "default" : undefined);
  if (s.enum) for (const v of s.enum) out.push({ label: String(v), apply: String(v), detail: def(v) });
  else if (t === "boolean") for (const v of ["true", "false"]) out.push({ label: v, apply: v, detail: def(v) });
  else if (s.default !== undefined && typeof s.default !== "object" && s.default !== "") out.push({ label: String(s.default), apply: String(s.default), detail: "default" });
  if (key === "device" && t === "string") for (const n of devicesIn(text)) out.push({ label: n, apply: n, detail: "device" });
  return out;
}

// devicesIn: the names in a scenario's devices list.
function devicesIn(text: string): string[] {
  try {
    const v = parseDocument(text).toJS() as { devices?: { name?: unknown }[] } | null;
    return (v?.devices ?? []).map((d) => d?.name).filter((n): n is string => typeof n === "string");
  } catch {
    return [];
  }
}

// ---------------------------------------------------------------------------
// Checking a whole document.

export type Problem = { from: number; to: number; message: string };

const isStepObject = (n: YNode) =>
  isMap(n) &&
  n.items.length > 0 &&
  n.items.every((p) => isScalar(p.key) && ["start", "step", "repeat", "cycle"].includes(String(p.key.value))) &&
  n.items.some((p) => isScalar(p.key) && p.key.value === "start");

// check: YAML errors, and against the schema: unknown fields (with the
// nearest known name), missing required ones, wrong types, values outside
// an enum or a range. Templates (in device groups) pass where a value is
// expected.
export function check(ctx: Ctx, text: string): Problem[] {
  const doc = parseDocument(text, { prettyErrors: false, uniqueKeys: true });
  const out: Problem[] = [];
  for (const e of doc.errors) out.push({ from: e.pos[0], to: Math.max(e.pos[1], e.pos[0] + 1), message: e.message.split("\n")[0] });
  if (doc.errors.length) return out;
  const lineEnd = (at: number) => {
    const e = text.indexOf("\n", at);
    return e < 0 ? text.length : e;
  };
  const walk = (n: YNode | null | undefined, at: At, keyRange: [number, number]) => {
    if (!n) return;
    const s = at.schema;
    const t = typeOf(s);
    const from = n.range?.[0] ?? keyRange[0];
    const to = n.range?.[1] ?? keyRange[1];
    const bad = (m: string, f = from, e = to) => out.push({ from: f, to: Math.max(e, f + 1), message: m });
    if (at.template && (isStepObject(n) || (isScalar(n) && typeof n.value === "string" && n.value.includes("{")))) return;
    if (isMap(n)) {
      if (t && t !== "object") return bad(`should be ${brief(s)}, not fields`, keyRange[0], keyRange[1]);
      const js = n.toJSON() as Record<string, unknown>;
      const keys = new Set<string>();
      for (const p of n.items as Pair<YNode, YNode>[]) {
        if (!isScalar(p.key)) continue;
        const k = String(p.key.value);
        keys.add(k);
        const kr: [number, number] = [p.key.range?.[0] ?? from, p.key.range?.[1] ?? from + 1];
        const c = child(ctx, at, k, (sk) => js[sk]);
        if (!c) {
          if (s.properties) bad(`unknown field ${k}${nearest(k, Object.keys(s.properties))}`, kr[0], kr[1]);
          continue;
        }
        walk(p.value as YNode, c, kr);
      }
      const missing = (s.required ?? []).filter((r) => !keys.has(r));
      if (missing.length) bad(`${missing.join(", ")} ${missing.length > 1 ? "are" : "is"} required`, from, Math.min(to, lineEnd(from)));
      return;
    }
    if (isSeq(n)) {
      if (t && t !== "array") return bad(`should be ${brief(s)}, not a list`, keyRange[0], keyRange[1]);
      const c = child(ctx, at, 0, () => undefined);
      if (c) for (const it of n.items as YNode[]) walk(it, c, keyRange);
      return;
    }
    if (!isScalar(n) || n.value === null) return;
    const v = n.value;
    if (s.enum) {
      if (!s.enum.map(String).includes(String(v))) bad(`${String(v)} is not one of ${s.enum.join(", ")}${nearest(String(v), s.enum.map(String))}`);
      return;
    }
    switch (t) {
      case "integer":
      case "number":
        if (typeof v !== "number") return bad(`${String(v)}: should be a number`);
        if (t === "integer" && !Number.isInteger(v)) return bad("should be a whole number");
        if (s.minimum !== undefined && v < s.minimum) return bad(`should be at least ${s.minimum}`);
        if (s.maximum !== undefined && v > s.maximum) return bad(`should be at most ${s.maximum}`);
        return;
      case "boolean":
        if (typeof v !== "boolean") bad("should be true or false");
        return;
      case "object":
        if (s.properties) bad("should be fields (key: value)");
        return;
      case "array":
        bad("should be a list");
        return;
    }
  };
  walk(doc.contents as YNode, { schema: ctx.root, template: !!ctx.template }, [0, 1]);
  return out;
}

// nearest names the closest known word, for typos.
function nearest(w: string, known: string[]): string {
  let best = "";
  let bd = Infinity;
  for (const k of known) {
    const d = distance(w.toLowerCase(), k.toLowerCase());
    if (d < bd) [best, bd] = [k, d];
  }
  return best && bd <= Math.max(1, Math.floor(w.length / 3)) ? ` (did you mean ${best}?)` : "";
}

// distance: edits from a to b, a swap of neighbours counting as one.
function distance(a: string, b: string): number {
  const d = Array.from({ length: a.length + 1 }, (_, i) => Array.from({ length: b.length + 1 }, (_, j) => (i === 0 ? j : j === 0 ? i : 0)));
  for (let i = 1; i <= a.length; i++)
    for (let j = 1; j <= b.length; j++) {
      d[i][j] = Math.min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + (a[i - 1] === b[j - 1] ? 0 : 1));
      if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1]) d[i][j] = Math.min(d[i][j], d[i - 2][j - 2] + 1);
    }
  return d[a.length][b.length];
}
