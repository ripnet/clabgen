export const restartNote = { restart: true, text: "Applying restarts the instance: counters and the remote state are reset." };
// inPlaceNote builds an applyNote for a type that applies some edits in
// place (its agent side's Reconfigure): an edit that only touches the
// fields in inPlace (and in each list item of lists, the fields given
// there) keeps the instance running with message ok; any other edit
// restarts it.
export function inPlaceNote(inPlace, lists, ok) {
    const strip = (c) => {
        const out = { ...c };
        for (const k of inPlace)
            delete out[k];
        for (const [k, fields] of Object.entries(lists))
            if (Array.isArray(out[k]))
                out[k] = out[k].map((it) => {
                    const x = { ...it };
                    for (const f of fields)
                        delete x[f];
                    return x;
                });
        return JSON.stringify(out);
    };
    return (was, now) => (canonical(strip(was)) === canonical(strip(now)) ? { restart: false, text: ok } : restartNote);
}
// canonical is a JSON text with keys sorted at every level.
function canonical(json) {
    const sort = (v) => Array.isArray(v) ? v.map(sort) : v && typeof v === "object" ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, sort(v[k])])) : v;
    return JSON.stringify(sort(JSON.parse(json)));
}
const views = {};
export function registerView(type, v) {
    views[type] = v;
}
export function viewFor(type) {
    return views[type] ?? {};
}
export function summarize(type, state, error, status) {
    if (state === "error")
        return { health: "bad", text: error || "error" };
    if (state !== "running")
        return { health: "idle", text: error || state };
    const s = viewFor(type).summary;
    if (s && status)
        return s(status);
    return { health: "ok", text: "running" };
}
