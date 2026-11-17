// YamlEditor: the YAML editor, loaded when first shown so other pages
// don't carry it. Until then, the text shows read-only.

import { Suspense, forwardRef, lazy } from "react";
import type { EditorHandle, EditorProps } from "./Editor";

export type { EditorHandle, EditorProps } from "./Editor";
export type { Ctx as YamlSchema } from "./schema";

const Editor = lazy(() => import("./Editor"));

export const YamlEditor = forwardRef<EditorHandle, EditorProps>(function YamlEditor(props, ref) {
  return (
    <Suspense fallback={<pre className="yaml-editor yaml-editor-loading">{props.value}</pre>}>
      <Editor ref={ref} {...props} />
    </Suspense>
  );
});
