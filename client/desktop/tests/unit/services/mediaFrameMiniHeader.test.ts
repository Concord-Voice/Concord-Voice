// @vitest-environment node
import { describe, it, expect } from 'vitest';
import {
  MEDIA_FRAME_MINI_HEADER_SIZE,
  OBU_MINI_HEADER_SIZE,
  encodeMediaFrameMiniHeader,
  decodeMediaFrameMiniHeader,
  encodeObuMiniHeader,
  decodeObuMiniHeader,
  buildObuIv,
} from '@/renderer/services/mediaFrameMiniHeader';

describe('mediaFrameMiniHeader', () => {
  it('exposes codec-neutral aliases without changing the 22-byte layout', () => {
    expect(MEDIA_FRAME_MINI_HEADER_SIZE).toBe(22);
    const header = encodeMediaFrameMiniHeader({
      iv: buildObuIv(),
      keyId: 0x00ab,
      keyVersion: 0xcdef,
    });
    expect(header).toHaveLength(22);
    expect(header.slice(0, 2)).toEqual(new Uint8Array([0xde, 0xad]));
    expect(decodeMediaFrameMiniHeader(header)).toMatchObject({
      keyId: 0x00ab,
      keyVersion: 0xcdef,
    });
  });

  it('mini-header is exactly 22 bytes', () => {
    expect(OBU_MINI_HEADER_SIZE).toBe(22);
  });

  it('round-trips magic / IV / keyId / keyVersion', () => {
    const iv = buildObuIv();
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
    const mh = encodeObuMiniHeader({ iv: buildObuIv(), keyId: 1, keyVersion: 1 });
    mh[0] = 0x00; // corrupt magic
    expect(decodeObuMiniHeader(mh)).toBeNull();
  });

  it('decode rejects a too-short buffer (returns null, no OOB)', () => {
    expect(decodeObuMiniHeader(new Uint8Array([0xde, 0xad]))).toBeNull();
    expect(decodeObuMiniHeader(new Uint8Array(0))).toBeNull();
  });

  it('uses a fresh full 96-bit random IV for each OBU', () => {
    const a = buildObuIv();
    const b = buildObuIv();
    expect(a).toHaveLength(12);
    expect(b).toHaveLength(12);
    expect(Array.from(a)).not.toEqual(Array.from(b));
  });
});
