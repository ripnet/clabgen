// A minimal pcapng writer (as the agent's internal/pcapng): a section
// header, one Ethernet interface per port, a block per frame with its
// direction. Wireshark opens the result.

export type PcapFrame = { port: string; direction: string; timeNs: bigint; data: Uint8Array; length: number };

class Buf {
  parts: Uint8Array[] = [];
  block(type: number, body: Uint8Array) {
    const n = 12 + body.length;
    const b = new Uint8Array(n);
    const v = new DataView(b.buffer);
    v.setUint32(0, type, true);
    v.setUint32(4, n, true);
    b.set(body, 8);
    v.setUint32(n - 4, n, true);
    this.parts.push(b);
  }
}

const pad4 = (n: number) => (n + 3) & ~3;

function options(opts: [number, Uint8Array][]): Uint8Array {
  const len = opts.reduce((a, [, v]) => a + 4 + pad4(v.length), 4);
  const b = new Uint8Array(len);
  const v = new DataView(b.buffer);
  let off = 0;
  for (const [code, val] of opts) {
    v.setUint16(off, code, true);
    v.setUint16(off + 2, val.length, true);
    b.set(val, off + 4);
    off += 4 + pad4(val.length);
  }
  return b; // ends with opt_endofopt (zeros)
}

const text = (s: string) => new TextEncoder().encode(s);
const cat = (...a: Uint8Array[]) => {
  const out = new Uint8Array(a.reduce((n, x) => n + x.length, 0));
  let off = 0;
  for (const x of a) {
    out.set(x, off);
    off += x.length;
  }
  return out;
};

export function pcapng(frames: PcapFrame[]): Blob {
  const w = new Buf();
  const shb = new Uint8Array(16);
  const sv = new DataView(shb.buffer);
  sv.setUint32(0, 0x1a2b3c4d, true);
  sv.setUint16(4, 1, true);
  sv.setBigInt64(8, -1n, true);
  w.block(0x0a0d0d0a, cat(shb, options([[4, text("clabgen web UI")]])));
  const ports = [...new Set(frames.map((f) => f.port))].sort();
  for (const p of ports) {
    const idb = new Uint8Array(8);
    const iv = new DataView(idb.buffer);
    iv.setUint16(0, 1, true); // Ethernet
    iv.setUint32(4, 262144, true);
    w.block(1, cat(idb, options([[2, text(p)], [9, new Uint8Array([9])]])));
  }
  for (const f of frames) {
    const hdr = new Uint8Array(20);
    const hv = new DataView(hdr.buffer);
    hv.setUint32(0, ports.indexOf(f.port), true);
    hv.setUint32(4, Number(f.timeNs >> 32n), true);
    hv.setUint32(8, Number(f.timeNs & 0xffffffffn), true);
    hv.setUint32(12, f.data.length, true);
    hv.setUint32(16, Math.max(f.length, f.data.length), true);
    const data = new Uint8Array(pad4(f.data.length));
    data.set(f.data);
    const flags = new Uint8Array(4);
    new DataView(flags.buffer).setUint32(0, f.direction === "tx" ? 2 : 1, true);
    w.block(6, cat(hdr, data, options([[2, flags]])));
  }
  return new Blob(w.parts as BlobPart[], { type: "application/x-pcapng" });
}
