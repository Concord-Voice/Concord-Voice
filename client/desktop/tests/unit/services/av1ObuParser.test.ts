// client/desktop/tests/unit/services/av1ObuParser.test.ts
// @vitest-environment node
import { describe, it, expect } from 'vitest';
import { parseAv1Obus, AV1_OBU_FRAME, AV1_OBU_TILE_GROUP } from '@/renderer/services/av1ObuParser';

/** Build a single low-overhead OBU: header + optional ext + leb128 size + payload. */
function obu(opts: {
  type: number;
  payload: Uint8Array;
  ext?: number; // present → extension_flag=1, this is the extension byte
  hasSize?: boolean; // default true
}): Uint8Array {
  const { type, payload, ext, hasSize = true } = opts;
  const header =
    ((type & 0x0f) << 3) | ((ext !== undefined ? 1 : 0) << 2) | ((hasSize ? 1 : 0) << 1);
  const head: number[] = [header];
  if (ext !== undefined) head.push(ext & 0xff);
  const size: number[] = [];
  if (hasSize) {
    let v = payload.length;
    do {
      let b = v & 0x7f;
      v >>>= 7;
      if (v) b |= 0x80;
      size.push(b);
    } while (v);
  }
  return new Uint8Array([...head, ...size, ...payload]);
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const total = parts.reduce((n, p) => n + p.length, 0);
  const out = new Uint8Array(total);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

describe('av1ObuParser', () => {
  describe('happy path', () => {
    it('parses TD + SEQUENCE_HEADER + FRAME and reports payload spans', () => {
      const td = obu({ type: 2, payload: new Uint8Array(0) });
      const seq = obu({ type: 1, payload: new Uint8Array([1, 2, 3]) });
      const frame = obu({ type: AV1_OBU_FRAME, payload: new Uint8Array([9, 8, 7, 6]) });
      const data = concat(td, seq, frame);

      const result = parseAv1Obus(data);
      expect(result).not.toBeNull();
      const obus = result!;
      expect(obus).toHaveLength(3);
      expect(obus[2].obuType).toBe(AV1_OBU_FRAME);
      // The FRAME payload [9,8,7,6] must be located byte-exactly.
      expect(
        Array.from(data.slice(obus[2].payloadOffset, obus[2].payloadOffset + obus[2].payloadLen))
      ).toEqual([9, 8, 7, 6]);
    });

    it('handles an extension header (extension_flag=1) before the size field', () => {
      const frame = obu({ type: AV1_OBU_FRAME, ext: 0x20, payload: new Uint8Array([5, 5]) });
      const obus = parseAv1Obus(frame)!;
      expect(obus[0].obuType).toBe(AV1_OBU_FRAME);
      expect(
        Array.from(frame.slice(obus[0].payloadOffset, obus[0].payloadOffset + obus[0].payloadLen))
      ).toEqual([5, 5]);
    });

    it('handles a no-size last OBU (payload runs to end)', () => {
      const td = obu({ type: 2, payload: new Uint8Array(0) });
      const tail = obu({
        type: AV1_OBU_TILE_GROUP,
        payload: new Uint8Array([1, 2, 3, 4, 5]),
        hasSize: false,
      });
      const data = concat(td, tail);
      const obus = parseAv1Obus(data)!;
      expect(obus).toHaveLength(2);
      expect(obus[1].obuType).toBe(AV1_OBU_TILE_GROUP);
      expect(obus[1].payloadLen).toBe(5);
      expect(obus[1].payloadOffset + obus[1].payloadLen).toBe(data.length);
    });

    it('parses multiple TILE_GROUP OBUs in one temporal unit', () => {
      const tg1 = obu({ type: AV1_OBU_TILE_GROUP, payload: new Uint8Array([1, 1]) });
      const tg2 = obu({ type: AV1_OBU_TILE_GROUP, payload: new Uint8Array([2, 2, 2]) });
      const obus = parseAv1Obus(concat(tg1, tg2))!;
      expect(obus.filter((o) => o.obuType === AV1_OBU_TILE_GROUP)).toHaveLength(2);
    });
  });

  describe('fail-closed on malformed input (returns null, never OOB-reads)', () => {
    it('empty input → null', () => {
      expect(parseAv1Obus(new Uint8Array(0))).toBeNull();
    });

    it('truncated header-only (size field missing) → null', () => {
      // header says has_size_field=1 but no size byte follows
      expect(parseAv1Obus(new Uint8Array([(6 << 3) | 0b10]))).toBeNull();
    });

    it('truncated extension header → null', () => {
      // extension_flag=1 but no extension byte
      expect(parseAv1Obus(new Uint8Array([(6 << 3) | 0b110]))).toBeNull();
    });

    it('forbidden bit set → null', () => {
      const frame = obu({ type: AV1_OBU_FRAME, payload: new Uint8Array([1]) });
      frame[0] |= 0x80; // set forbidden bit
      expect(parseAv1Obus(frame)).toBeNull();
    });

    it('leb128 continuation-forever (all 0xFF) → null', () => {
      // header + 8x 0xFF (never terminates) — must hit the 8-byte cap and fail
      const data = new Uint8Array([
        (6 << 3) | 0b10,
        0xff,
        0xff,
        0xff,
        0xff,
        0xff,
        0xff,
        0xff,
        0xff,
      ]);
      expect(parseAv1Obus(data)).toBeNull();
    });

    it('leb128 size > remaining bytes → null', () => {
      // claims a 100-byte payload but provides 2
      const data = new Uint8Array([(6 << 3) | 0b10, 100, 1, 2]);
      expect(parseAv1Obus(data)).toBeNull();
    });

    it('leb128 size > 2^31 → null', () => {
      // 5-byte leb128 encoding 0xFFFFFFFF (> 2^31)
      const data = new Uint8Array([(6 << 3) | 0b10, 0xff, 0xff, 0xff, 0xff, 0x0f, 0]);
      expect(parseAv1Obus(data)).toBeNull();
    });

    it('OBU count > 256 → null (fail-closed cap still holds)', () => {
      // 257 zero-length TD OBUs (each [hdr, size=0]) overruns the new cap
      const one = obu({ type: 2, payload: new Uint8Array(0) });
      const many: Uint8Array[] = [];
      for (let i = 0; i < 257; i++) many.push(one);
      expect(parseAv1Obus(concat(...many))).toBeNull();
    });

    // FIX 3 (Gitar #1): cap raised to 256 to cover SVC/multi-tile temporal units.
    it('~100 OBUs in a valid temporal unit parse successfully (SVC/multi-tile coverage)', () => {
      // 100 small TILE_GROUP OBUs — a valid SVC temporal unit with many tiles.
      const one = obu({ type: AV1_OBU_TILE_GROUP, payload: new Uint8Array([1, 2]) });
      const many: Uint8Array[] = [];
      for (let i = 0; i < 100; i++) many.push(one);
      const result = parseAv1Obus(concat(...many));
      expect(result).not.toBeNull();
      expect(result!).toHaveLength(100);
    });

    it('fuzz: 200 random byte buffers never throw and never OOB', () => {
      for (let i = 0; i < 200; i++) {
        const len = (Math.random() * 200) | 0;
        const buf = new Uint8Array(len);
        crypto.getRandomValues(buf);
        // Must return null OR a well-formed array — never throw, never index past end.
        const r = parseAv1Obus(buf);
        if (r !== null) {
          for (const o of r) {
            expect(o.payloadOffset).toBeGreaterThanOrEqual(0);
            expect(o.payloadOffset + o.payloadLen).toBeLessThanOrEqual(buf.length);
          }
        }
      }
    });
  });
});
