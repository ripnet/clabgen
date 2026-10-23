import { Fragment, useMemo, useState } from "react";

// A packet preview from the agent (protocols/packet PreviewResult).
export type PreviewField = { name: string; bit_offset: number; bits: number; value: string };
export type PreviewLayer = { layer: number; title: string; offset: number; length: number; fields?: PreviewField[] };
export type Preview = {
  hex: string;
  length: number;
  summary: string;
  index: number;
  note?: string;
  layers?: PreviewLayer[];
  frames?: { index: number; length: number; summary: string }[];
  decoded?: Record<string, unknown>[];
};

// fieldKeys names each preview layer's fields the way modifiers address
// them ("ipv4.src", "vlan[2].vid"), from the config's layer kinds.
export function fieldKeys(p: Preview, kinds: string[]): Map<PreviewLayer, string> {
  const out = new Map<PreviewLayer, string>();
  const count: Record<string, number> = {};
  const seen = new Map<number, string>();
  for (const l of p.layers ?? []) {
    if (l.layer === -2) continue; // the device's tags
    let prefix = seen.get(l.layer);
    if (prefix === undefined) {
      const kind = l.layer < 0 ? "ethernet" : kinds[l.layer];
      if (!kind) continue;
      count[kind] = (count[kind] ?? 0) + 1;
      prefix = count[kind] === 1 ? kind : `${kind}[${count[kind]}]`;
      seen.set(l.layer, prefix);
    }
    out.set(l, prefix);
  }
  return out;
}

type Hover = { layer?: number; field?: [number, number] }; // index in layers; bit range

export function PacketHex({
  preview,
  kinds,
  activeLayer,
  onLayer,
  onAddModifier,
}: {
  preview: Preview;
  kinds?: string[]; // config layer kinds, for field names
  activeLayer?: number; // a config layer to highlight (hovered in the editor)
  onLayer?: (configLayer: number | undefined) => void;
  onAddModifier?: (field: string) => void;
}) {
  const [hover, setHover] = useState<Hover>({});
  const bytes = useMemo(() => {
    const h = preview.hex ?? "";
    const out: number[] = [];
    for (let i = 0; i + 1 < h.length; i += 2) out.push(parseInt(h.slice(i, i + 2), 16));
    return out;
  }, [preview.hex]);
  const layers = preview.layers ?? [];
  const keys = useMemo(() => (kinds ? fieldKeys(preview, kinds) : new Map<PreviewLayer, string>()), [preview, kinds]);
  // Each byte's layer (index into layers), for its colour.
  const layerOf = useMemo(() => {
    const m = new Array<number>(bytes.length).fill(-1);
    layers.forEach((l, i) => {
      for (let b = l.offset; b < l.offset + l.length && b < m.length; b++) m[b] = i;
    });
    return m;
  }, [layers, bytes.length]);

  const lit = (b: number) => {
    if (hover.field) {
      const [pos, bits] = hover.field;
      return b >= Math.floor(pos / 8) && b < Math.ceil((pos + bits) / 8);
    }
    const li = hover.layer ?? (activeLayer !== undefined ? layers.findIndex((l) => l.layer === activeLayer) : -1);
    return li >= 0 && layerOf[b] === li;
  };
  const enterLayer = (i: number | undefined, f?: PreviewField) => {
    setHover({ layer: i, field: f ? [f.bit_offset, f.bits] : undefined });
    onLayer?.(i === undefined ? undefined : layers[i].layer);
  };

  // Colours follow the config's layers (as the editor's cards do); the
  // device's tags and an implied Ethernet header are neutral.
  const tint = (li: number) => (li < 0 || layers[li].layer < 0 ? "tn" : `t${layers[li].layer % 6}`);
  const rows = [];
  for (let off = 0; off < bytes.length; off += 16) {
    const cells = [];
    let ascii = "";
    for (let b = off; b < off + 16; b++) {
      if (b >= bytes.length) {
        cells.push(<span key={b} className={`hex-b hex-pad${b % 8 === 7 ? " gap" : ""}`}>{"\u00a0\u00a0"}</span>);
        continue;
      }
      const li = layerOf[b];
      const c = bytes[b];
      ascii += c >= 0x20 && c < 0x7f ? String.fromCharCode(c) : ".";
      cells.push(
        <span
          key={b}
          className={`hex-b ${tint(li)}${lit(b) ? " lit" : ""}${b % 8 === 7 ? " gap" : ""}`}
          onMouseEnter={() => enterLayer(li < 0 ? undefined : li, fieldAt(layers[li], b))}
          onMouseLeave={() => enterLayer(undefined)}
        >
          {c.toString(16).padStart(2, "0")}
        </span>,
      );
    }
    rows.push(
      <div className="hex-row" key={off}>
        <span className="hex-off">{off.toString(16).padStart(4, "0")}</span>
        <span className="hex-bytes">{cells}</span>
        <span className="hex-ascii">{ascii}</span>
      </div>,
    );
  }

  return (
    <div className="pkt-hex">
      <div className="hex-dump mono" aria-label="Packet bytes">
        {rows}
      </div>
      <table className="table dense pkt-fields">
        <tbody>
          {layers.map((l, i) => (
            <Fragment key={i}>
              <tr className={`pkt-layer-row ${tint(i)}`} onMouseEnter={() => enterLayer(i)} onMouseLeave={() => enterLayer(undefined)}>
                <th colSpan={3}>
                  {l.title} <span className="muted">· {l.offset}–{l.offset + l.length - 1}</span>
                </th>
              </tr>
              {(l.fields ?? []).map((f) => {
                const key = keys.get(l) ? `${keys.get(l)}.${f.name}` : "";
                return (
                  <tr key={f.name} onMouseEnter={() => enterLayer(i, f)} onMouseLeave={() => enterLayer(undefined)} className={hover.field?.[0] === f.bit_offset ? "lit" : ""}>
                    <td className="mono">{f.name}</td>
                    <td className="mono">{f.value}</td>
                    <td className="num">
                      {onAddModifier && key && (
                        <button type="button" className="link small" title={`Change ${key} from packet to packet`} onClick={() => onAddModifier(key)}>
                          + modifier
                        </button>
                      )}
                    </td>
                  </tr>
                );
              })}
            </Fragment>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function fieldAt(l: PreviewLayer | undefined, b: number): PreviewField | undefined {
  return l?.fields?.find((f) => b >= Math.floor(f.bit_offset / 8) && b < Math.ceil((f.bit_offset + f.bits) / 8));
}
