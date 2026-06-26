// @vitest-environment node
import { describe, it, expect } from 'vitest';
import {
  OBU_MINI_HEADER_SIZE,
  encodeObuMiniHeader,
  decodeObuMiniHeader,
  buildObuIv,
} from '@/renderer/services/mediaFrameMiniHeader';

describe('mediaFrameMiniHeader', () => {
  it('mini-header is exactly 22 bytes', () => {
    expect(OBU_MINI_HEADER_SIZE).toBe(22);
  });

  it('round-trips magic / IV / keyId / keyVersion', () => {
    const iv = buildObuIv(0x01020304, 7);
    const mh = encodeObuMiniHeader({ iv, keyId: 0x00ab, keyVersion: 0xcdef });
    expect(mh).toHaveLength(22);
    // magic leads
    expect(mh[0]).toBe(0xde);
    expect(mh[1]).toBe(0xad);

    const decoded = decodeObuMiniHeader(mh);
    expect(decoded).not.toBeNull();
    expect(Array.from(decoded!.iv)).toEqual(Array.from(iv));
    expect(decoded!.keyId).toBe(0x00ab);
    expect(decoded!.keyVersion).toBe(0xcdef);
  });

  it('decode rejects a buffer without leading magic (returns null)', () => {
    const mh = encodeObuMiniHeader({ iv: buildObuIv(1, 0), keyId: 1, keyVersion: 1 });
    mh[0] = 0x00; // corrupt magic
    expect(decodeObuMiniHeader(mh)).toBeNull();
  });

  it('decode rejects a too-short buffer (returns null, no OOB)', () => {
    expect(decodeObuMiniHeader(new Uint8Array([0xde, 0xad]))).toBeNull();
    expect(decodeObuMiniHeader(new Uint8Array(0))).toBeNull();
  });

  it('IV layout is [frame_counter:4 BE][obu_seq_index:2 BE][random:6]', () => {
    const iv = buildObuIv(0xaabbccdd, 0x0102);
    expect(iv).toHaveLength(12);
    expect(Array.from(iv.slice(0, 4))).toEqual([0xaa, 0xbb, 0xcc, 0xdd]);
    expect(Array.from(iv.slice(4, 6))).toEqual([0x01, 0x02]);
  });

  it('per-OBU IV uniqueness: distinct obu_seq_index → distinct IV prefix', () => {
    const a = buildObuIv(100, 0);
    const b = buildObuIv(100, 1);
    // Same frame_counter, different obu_seq_index → bytes 4-5 differ.
    expect(Array.from(a.slice(0, 6))).not.toEqual(Array.from(b.slice(0, 6)));
  });

  it('random tail differs across two IVs with identical counters (CSPRNG)', () => {
    const a = buildObuIv(1, 0);
    const b = buildObuIv(1, 0);
    // Bytes 6-11 are CSPRNG → astronomically unlikely to match.
    expect(Array.from(a.slice(6))).not.toEqual(Array.from(b.slice(6)));
  });
});
