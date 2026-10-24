// Versions are vX.Y.Z for releases; anything else (a commit hash,
// v0.1.0-3-gabc1234, dev) is a development build.

const releaseRE = /^v?(\d+)\.(\d+)\.(\d+)$/;

export function parseRelease(v: string | undefined): [number, number, number] | undefined {
  const m = v?.match(releaseRE);
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : undefined;
}

// baseRelease is the release a version is (vX.Y.Z) or was built after
// (vX.Y.Z-N-gHASH, vX.Y.Z-dirty, as git describe writes them).
function baseRelease(v: string | undefined): [number, number, number] | undefined {
  const m = v?.match(/^v?(\d+)\.(\d+)\.(\d+)(?:-|$)/);
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : undefined;
}

// cmp orders two versions by release: undefined when either has none, or
// when development builds share a base release (they can't be ordered).
function cmp(a?: string, b?: string): number | undefined {
  const x = baseRelease(a);
  const y = baseRelease(b);
  if (!x || !y) return undefined;
  for (let i = 0; i < 3; i++) if (x[i] !== y[i]) return x[i] - y[i];
  return parseRelease(a) && parseRelease(b) ? 0 : undefined;
}

// displayVersion is how a version reads in the UI.
export const displayVersion = (v?: string) => (!v ? "" : parseRelease(v) ? (v.startsWith("v") ? v : `v${v}`) : `dev ${v}`);

export type Skew = { kind: "older" | "newer" | "differs"; text: string };

// skew compares an agent's version with the UI's: an older or newer
// release, or (development builds) just different.
export function skew(agent?: string, ui?: string): Skew | undefined {
  if (!agent || !ui || agent === ui) return undefined;
  const c = cmp(agent, ui);
  const a = displayVersion(agent);
  const u = displayVersion(ui);
  if (c === undefined) return { kind: "differs", text: `agent ${a}, UI ${u}` };
  if (c < 0) return { kind: "older", text: `agent ${a} is older than the UI (${u}): some features may fail; redeploy the lab with the newer image` };
  if (c > 0) return { kind: "newer", text: `agent ${a} is newer than the UI (${u}): update clabgen-ui to see everything it does` };
  return undefined;
}
