// ValueView renders arbitrary JSON readably: scalars as key/value rows,
// arrays of objects as tables, nested objects as sections. It is the default
// view for any protocol's status and action results.

type V = unknown;

const isObj = (v: V): v is Record<string, V> => typeof v === "object" && v !== null && !Array.isArray(v);
const label = (k: string) => k.replace(/_/g, " ");

const tsRE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}/;

// Identifiers read better without digit grouping.
const rawKey = /(^|_)(id|seq|vid|ttl|hop|level|index|test_id|trans_id|mep_id)$/;

// Struct fields arrive sorted by name; lead tables with identifying columns.
const leadCols = ["hop", "seq", "mep_id", "id", "name", "state"];

export function fmtScalar(v: V, key = ""): string {
  if (v === null || v === undefined) return "—";
  if (typeof v === "number" && rawKey.test(key)) return String(v);
  if (typeof v === "number") return Number.isInteger(v) ? v.toLocaleString() : v.toFixed(v < 10 ? 3 : 1);
  if (typeof v === "string" && tsRE.test(v)) {
    const d = new Date(v);
    if (d.getFullYear() < 1971) return "—";
    return `${d.toLocaleTimeString()} (${ago(d)})`;
  }
  return String(v);
}

export function ago(d: Date): string {
  const s = Math.max(0, (Date.now() - d.getTime()) / 1000);
  if (s < 1) return "now";
  if (s < 60) return `${Math.round(s)}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  return `${Math.round(s / 3600)}h ago`;
}

function Scalar({ v, k }: { v: V; k?: string }) {
  if (typeof v === "boolean") return <span className={v ? "pill pill-on" : "pill"}>{String(v)}</span>;
  return <span className="mono">{fmtScalar(v, k)}</span>;
}

export function Table({ rows }: { rows: Record<string, V>[] }) {
  const cols: string[] = [];
  for (const r of rows) for (const k of Object.keys(r)) if (!cols.includes(k)) cols.push(k);
  cols.sort((a, b) => {
    const ia = leadCols.indexOf(a), ib = leadCols.indexOf(b);
    return (ia < 0 ? 99 : ia) - (ib < 0 ? 99 : ib);
  });
  return (
    <div className="table-wrap">
      <table className="table dense">
        <thead>
          <tr>
            {cols.map((c) => (
              <th key={c}>{label(c)}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {cols.map((c) => (
                <td key={c}>{isObj(r[c]) || Array.isArray(r[c]) ? <code>{JSON.stringify(r[c])}</code> : <Scalar v={r[c]} k={c} />}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function ValueView({ value, depth = 0, k }: { value: V; depth?: number; k?: string }) {
  if (!isObj(value)) {
    if (Array.isArray(value)) {
      if (value.length && value.every(isObj)) return <Table rows={value as Record<string, V>[]} />;
      return <span className="mono">{value.length ? value.map((x) => fmtScalar(x)).join(", ") : "—"}</span>;
    }
    return <Scalar v={value} k={k} />;
  }
  const entries = Object.entries(value);
  const simple = entries.filter(([, v]) => !isObj(v) && !(Array.isArray(v) && v.some(isObj)));
  const complex = entries.filter(([, v]) => isObj(v) || (Array.isArray(v) && v.some(isObj)));
  return (
    <div className="value-view">
      {simple.length > 0 && (
        <dl className="kv">
          {simple.map(([k, v]) => (
            <div key={k} className="kv-row">
              <dt>{label(k)}</dt>
              <dd>
                <ValueView value={v} depth={depth + 1} k={k} />
              </dd>
            </div>
          ))}
        </dl>
      )}
      {complex.map(([k, v]) => (
        <section key={k} className="sub">
          <h4>{label(k)}</h4>
          <ValueView value={v} depth={depth + 1} />
        </section>
      ))}
    </div>
  );
}
