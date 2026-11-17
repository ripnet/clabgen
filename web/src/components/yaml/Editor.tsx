// The YAML editor (CodeMirror 6): highlighting, folding, search, and help
// from a schema (see schema.ts): completion, hover, problems as you type.
// Loaded on demand through YamlEditor.tsx.

import { useEffect, useImperativeHandle, useRef, forwardRef } from "react";
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap, type CompletionContext, type CompletionResult } from "@codemirror/autocomplete";
import { defaultKeymap, history, historyKeymap, indentWithTab } from "@codemirror/commands";
import {
  HighlightStyle,
  bracketMatching,
  codeFolding,
  foldEffect,
  foldGutter,
  foldKeymap,
  foldedRanges,
  indentOnInput,
  indentUnit,
  syntaxHighlighting,
  unfoldAll,
} from "@codemirror/language";
import { yaml } from "@codemirror/lang-yaml";
import { lintGutter, lintKeymap, linter, type Diagnostic } from "@codemirror/lint";
import { highlightSelectionMatches, search, searchKeymap } from "@codemirror/search";
import { Compartment, EditorState, type Extension } from "@codemirror/state";
import {
  EditorView,
  drawSelection,
  highlightActiveLine,
  highlightActiveLineGutter,
  hoverTooltip,
  keymap,
  lineNumbers,
  placeholder as placeholderExt,
} from "@codemirror/view";
import { tags } from "@lezer/highlight";
import { check, describe, hoverAt, keyOffers, placeAt, resolve, valueOffers, type Ctx } from "./schema";

export type EditorProps = {
  value: string;
  onChange: (v: string) => void;
  // Schema help; none: a plain YAML editor.
  ctx?: Ctx;
  // Mod-s (Ctrl-S, Cmd-S).
  onSave?: () => void;
  // Fold each list item under the top-level keys when the editor opens.
  foldItems?: boolean;
  ariaLabel?: string;
  placeholder?: string;
  minLines?: number;
  maxHeight?: string;
};

export type EditorHandle = { foldItems: () => void; unfoldAll: () => void; focus: () => void };

// Colours come from the page's tokens (styles.css, .cm-yaml-*), so they
// follow the light and dark themes.
const highlight = HighlightStyle.define([
  { tag: tags.definition(tags.propertyName), class: "cm-yaml-key" },
  { tag: tags.string, class: "cm-yaml-string" },
  { tag: tags.special(tags.string), class: "cm-yaml-string" },
  { tag: tags.lineComment, class: "cm-yaml-comment" },
  { tag: [tags.separator, tags.punctuation, tags.meta], class: "cm-yaml-punct" },
  { tag: [tags.brace, tags.squareBracket], class: "cm-yaml-bracket" },
  { tag: [tags.labelName, tags.typeName, tags.keyword], class: "cm-yaml-special" },
]);

// The editor's popups (suggestions, help, problems, search) in the page's
// colours. CodeMirror's base theme styles some of them under its light
// and dark classes (two classes deep); "&.cm-editor" matches that, and a
// theme wins a tie over the base theme.
const both = (sel: string, style: Record<string, string>) => ({ [`&.cm-editor ${sel}`]: style });
const popups = EditorView.theme({
  ...both(".cm-tooltip", {
    backgroundColor: "var(--surface)",
    color: "var(--text)",
    border: "1px solid var(--border-strong)",
    borderRadius: "6px",
    boxShadow: "var(--shadow-lg)",
    fontFamily: "var(--sans)",
    fontSize: "12.5px",
  }),
  ...both(".cm-tooltip-autocomplete ul li[aria-selected]", { background: "var(--accent)", color: "#fff" }),
  ...both(".cm-panels", { backgroundColor: "var(--surface-2)", color: "var(--text)" }),
  ...both(".cm-panels.cm-panels-bottom", { borderTop: "1px solid var(--border)" }),
  ...both(".cm-panels.cm-panels-top", { borderBottom: "1px solid var(--border)" }),
  ".cm-tooltip.cm-tooltip-autocomplete > ul": { fontFamily: "var(--mono)", maxHeight: "16em" },
  ".cm-tooltip.cm-tooltip-autocomplete > ul > li": { padding: "2px 8px" },
  ".cm-tooltip-autocomplete ul li[aria-selected] .cm-completionDetail": { color: "rgba(255, 255, 255, 0.85)" },
  ".cm-completionDetail": { color: "var(--text-3)", fontStyle: "normal", marginLeft: "1em", fontFamily: "var(--sans)", fontSize: "11.5px" },
  ".cm-completionMatchedText": { textDecoration: "none", fontWeight: "600" },
  ".cm-tooltip.cm-completionInfo": { maxWidth: "360px", padding: "6px 10px", lineHeight: "1.45" },
  ".cm-tooltip-lint": { padding: "0" },
  ".cm-diagnostic": { padding: "4px 10px", color: "var(--text)" },
  ".cm-diagnostic-error": { borderLeft: "4px solid var(--bad)" },
  ".cm-tooltip-section:not(:first-child)": { borderTop: "1px solid var(--border)" },
});

// itemRanges: where each list item under a top-level key runs, from the
// end of its first line to the end of its last.
function itemRanges(state: EditorState): { from: number; to: number }[] {
  const out: { from: number; to: number }[] = [];
  const doc = state.doc;
  let open: { from: number; to: number; dash: number } | undefined;
  const flush = () => {
    if (open && open.to > open.from) out.push({ from: open.from, to: open.to });
    open = undefined;
  };
  let inList = false;
  for (let n = 1; n <= doc.lines; n++) {
    const l = doc.line(n);
    const t = l.text;
    if (!t.trim() || t.trimStart().startsWith("#")) continue;
    const ind = t.length - t.trimStart().length;
    if (ind === 0 && !t.startsWith("-")) {
      flush();
      inList = true;
      continue;
    }
    if (inList && t.trimStart().startsWith("- ") && ind <= 2 && (!open || ind === open.dash)) {
      flush();
      open = { from: l.to, to: l.to, dash: ind };
      continue;
    }
    if (open && ind > open.dash) open.to = l.to;
  }
  flush();
  return out;
}

// summary: what a folded block holds, for its placeholder: a list item's
// type and device, else its size.
function summary(state: EditorState, range: { from: number; to: number }): string {
  const text = state.sliceDoc(range.from, range.to);
  const lines = text.split("\n").length - 1;
  const first = state.doc.lineAt(range.from).text;
  const col = first.match(/^(\s*-\s+)/)?.[1].length;
  const field = (k: string) => {
    if (col === undefined) return undefined;
    return text.match(new RegExp(`\\n {${col}}${k}:\\s*([^\\n#]+)`))?.[1]?.trim();
  };
  const bits = [field("type") && `${field("type")}`, field("device") && `on ${field("device")}`, field("port") && `on ${field("port")}`].filter(Boolean);
  return `${bits.length ? bits.join(" ") + " · " : ""}${lines} line${lines === 1 ? "" : "s"}`;
}

const Editor = forwardRef<EditorHandle, EditorProps>(function Editor(props, ref) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView>();
  // Callbacks and schemas change without rebuilding the editor.
  const live = useRef(props);
  live.current = props;
  // The checker, rebuilt when the schemas change so the text is checked
  // again (they often arrive after the editor opens).
  const checker = useRef(new Compartment());
  const makeLint = useRef<() => Extension>(() => []);

  useEffect(() => {
    const ctx = () => live.current.ctx;
    const complete = (c: CompletionContext): CompletionResult | null => {
      const x = ctx();
      if (!x) return null;
      const line = c.state.doc.lineAt(c.pos);
      const p = placeAt(c.state.doc.toString(), line.number - 1, c.pos - line.from);
      // Keys once a letter is typed (or Ctrl-Space); values right after "key: ".
      if (!c.explicit && !p.word && p.kind === "key") return null;
      const text = c.state.doc.toString();
      const at = resolve(x, text, p.path);
      if (!at) return null;
      const offers = p.kind === "key" ? keyOffers(at, p.present) : valueOffers(resolve(x, text, p.path, p.key) ?? at, p.key, text);
      if (!offers.length) return null;
      return {
        from: line.from + p.from,
        options: offers.map((o) => ({ label: o.label, apply: o.apply, detail: o.detail, info: o.info, boost: o.boost, type: p.kind === "key" ? "property" : "enum" })),
        validFor: /^[\w.\-/]*$/,
      };
    };
    const hover = hoverTooltip((v, pos) => {
      const x = ctx();
      if (!x) return null;
      const line = v.state.doc.lineAt(pos);
      const h = hoverAt(x, v.state.doc.toString(), line.number - 1, pos - line.from);
      if (!h) return null;
      return {
        pos: line.from + h.from,
        end: line.from + Math.max(h.to, h.from + 1),
        above: true,
        create: () => {
          const dom = document.createElement("div");
          dom.className = "cm-yaml-hover";
          const d = describe(h.at.schema);
          const head = document.createElement("div");
          head.className = "cm-yaml-hover-head";
          head.textContent = `${h.key} · ${d.facts}`;
          dom.append(head);
          if (d.text) {
            const body = document.createElement("div");
            body.textContent = d.text;
            dom.append(body);
          }
          if (h.at.template) {
            const t = document.createElement("div");
            t.className = "cm-yaml-hover-note";
            t.textContent = "Templates allowed: {i}, {start: …, step: …}";
            dom.append(t);
          }
          return { dom };
        },
      };
    });
    makeLint.current = () => linter(
      (v): Diagnostic[] => {
        const x = ctx();
        const text = v.state.doc.toString();
        if (!x || !text.trim()) return [];
        const len = v.state.doc.length;
        return check(x, text).map((p) => ({ from: Math.min(p.from, len), to: Math.min(p.to, len), severity: "error", message: p.message }));
      },
      { delay: 300 },
    );
    const extensions: Extension[] = [
      lineNumbers(),
      highlightActiveLineGutter(),
      foldGutter({ openText: "▾", closedText: "▸" }),
      codeFolding({
        preparePlaceholder: (state, range) => summary(state, range),
        placeholderDOM: (_v, onclick, prepared: string) => {
          const s = document.createElement("span");
          s.className = "cm-foldPlaceholder";
          s.textContent = `… ${prepared}`;
          s.title = "Unfold";
          s.onclick = onclick;
          return s;
        },
      }),
      history(),
      drawSelection(),
      indentOnInput(),
      indentUnit.of("  "),
      EditorState.tabSize.of(2),
      bracketMatching(),
      closeBrackets(),
      highlightActiveLine(),
      highlightSelectionMatches(),
      // Find at the top: the box is often taller than the window.
      search({ top: true }),
      yaml(),
      syntaxHighlighting(highlight),
      popups,
      autocompletion({ override: [complete], icons: false }),
      hover,
      checker.current.of(makeLint.current()),
      lintGutter(),
      keymap.of([
        { key: "Mod-s", preventDefault: true, run: () => (live.current.onSave?.(), true) },
        ...closeBracketsKeymap,
        ...defaultKeymap,
        ...searchKeymap,
        ...historyKeymap,
        ...foldKeymap,
        ...completionKeymap,
        ...lintKeymap,
        indentWithTab,
      ]),
      EditorView.updateListener.of((u) => {
        if (u.docChanged) live.current.onChange(u.state.doc.toString());
      }),
      EditorView.contentAttributes.of({ "aria-label": props.ariaLabel ?? "YAML", spellcheck: "false" }),
    ];
    if (props.placeholder) extensions.push(placeholderExt(props.placeholder));
    const v = new EditorView({ parent: host.current!, state: EditorState.create({ doc: props.value, extensions }) });
    view.current = v;
    if (props.foldItems) foldAllItems(v);
    return () => v.destroy();
    // The editor is built once; later props arrive through live.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // A new value from outside (reload, opening a file) replaces the text.
  useEffect(() => {
    const v = view.current;
    if (!v || v.state.doc.toString() === props.value) return;
    v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: props.value } });
    if (props.foldItems) foldAllItems(v);
  }, [props.value, props.foldItems]);

  // Schemas arriving late: check again.
  useEffect(() => {
    view.current?.dispatch({ effects: checker.current.reconfigure(makeLint.current()) });
  }, [props.ctx]);

  useImperativeHandle(ref, () => ({
    foldItems: () => view.current && foldAllItems(view.current),
    unfoldAll: () => void (view.current && unfoldAll(view.current)),
    focus: () => view.current?.focus(),
  }));

  const minH = `${(props.minLines ?? 6) * 1.5 + 0.75}em`;
  return <div ref={host} className="yaml-editor" style={{ minHeight: minH, maxHeight: props.maxHeight }} />;
});

function foldAllItems(v: EditorView) {
  const folded = foldedRanges(v.state);
  const effects = itemRanges(v.state)
    .filter((r) => {
      let already = false;
      folded.between(r.from, r.to, (f, t) => void (f === r.from && t === r.to && (already = true)));
      return !already;
    })
    .map((r) => foldEffect.of(r));
  if (effects.length) v.dispatch({ effects });
}

export default Editor;
