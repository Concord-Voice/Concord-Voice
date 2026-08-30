// @vitest-environment node
import { describe, expect, it } from 'vitest';
import {
  findH264SlicePrefixLength,
  parseH264AnnexB,
  stuffH264Bytes,
  unstuffH264Bytes,
} from '@/renderer/services/e2ee/h264NalUnit';

function concat(...parts: Uint8Array[]): Uint8Array {
  const length = parts.reduce((total, part) => total + part.length, 0);
  const result = new Uint8Array(length);
  let offset = 0;
  for (const part of parts) {
    result.set(part, offset);
    offset += part.length;
  }
  return result;
}

function startCode(length: 3 | 4): Uint8Array {
  return length === 3
    ? new Uint8Array([0x00, 0x00, 0x01])
    : new Uint8Array([0x00, 0x00, 0x00, 0x01]);
}

function annexBNal(type: number, delimiterLength: 3 | 4 = 3): Uint8Array {
  return concat(startCode(delimiterLength), new Uint8Array([type & 0x1f, 0x80]));
}

function requireBytes(value: Uint8Array | null): Uint8Array {
  if (value === null) throw new Error('expected byte result');
  return value;
}

function containsSequence(data: Uint8Array, sequence: readonly number[]): boolean {
  for (let offset = 0; offset + sequence.length <= data.length; offset++) {
    let matches = true;
    for (let index = 0; index < sequence.length; index++) {
      if (data[offset + index] !== sequence[index]) {
        matches = false;
        break;
      }
    }
    if (matches) return true;
  }
  return false;
}

function encodeUnsignedExpGolomb(values: readonly number[]): Uint8Array {
  let bits = '';
  for (const value of values) {
    const binary = (value + 1).toString(2);
    bits += `${'0'.repeat(binary.length - 1)}${binary}`;
  }
  bits = bits.padEnd(Math.ceil(bits.length / 8) * 8, '0');

  const rbsp = new Uint8Array(bits.length / 8);
  for (let offset = 0; offset < bits.length; offset += 8) {
    rbsp[offset / 8] = Number.parseInt(bits.slice(offset, offset + 8), 2);
  }

  const ebsp: number[] = [];
  let zeroRun = 0;
  for (const byte of rbsp) {
    if (zeroRun === 2 && byte <= 0x03) {
      ebsp.push(0x03);
      zeroRun = 0;
    }
    ebsp.push(byte);
    zeroRun = byte === 0 ? Math.min(2, zeroRun + 1) : 0;
  }
  return new Uint8Array(ebsp);
}

describe('parseH264AnnexB', () => {
  it('parses mixed three- and four-byte delimiters without splitting escaped payload bytes', () => {
    const accessUnit = concat(
      startCode(4),
      new Uint8Array([0x67, 0x42, 0x00, 0x1f]),
      startCode(3),
      new Uint8Array([0x68, 0xce]),
      startCode(4),
      new Uint8Array([0x65, 0xb2, 0x00, 0x00, 0x03, 0x01])
    );

    expect(parseH264AnnexB(accessUnit)).toEqual([
      {
        startCodeOffset: 0,
        startCodeLength: 4,
        nalOffset: 4,
        nalLength: 4,
        nalType: 7,
        kind: 'clear',
      },
      {
        startCodeOffset: 8,
        startCodeLength: 3,
        nalOffset: 11,
        nalLength: 2,
        nalType: 8,
        kind: 'clear',
      },
      {
        startCodeOffset: 13,
        startCodeLength: 4,
        nalOffset: 17,
        nalLength: 6,
        nalType: 5,
        kind: 'slice',
      },
    ]);
  });

  it('classifies supported clear, opaque-encrypted, and slice NAL units', () => {
    const clearTypes = [7, 8, 9, 10, 11, 13, 15];
    const opaqueTypes = [6, 12];
    const sliceTypes = [1, 5];
    const accessUnit = concat(
      ...[...clearTypes, ...opaqueTypes, ...sliceTypes].map((type) => annexBNal(type))
    );
    const parsed = parseH264AnnexB(accessUnit);

    expect(parsed?.map(({ nalType, kind }) => ({ nalType, kind }))).toEqual([
      ...clearTypes.map((nalType) => ({ nalType, kind: 'clear' })),
      ...opaqueTypes.map((nalType) => ({ nalType, kind: 'opaque' })),
      ...sliceTypes.map((nalType) => ({ nalType, kind: 'slice' })),
    ]);
  });

  it('rejects empty input, missing delimiters, and empty NAL units', () => {
    expect(parseH264AnnexB(new Uint8Array())).toBeNull();
    expect(parseH264AnnexB(new Uint8Array([0x65, 0x80]))).toBeNull();
    expect(parseH264AnnexB(startCode(3))).toBeNull();
    expect(parseH264AnnexB(concat(startCode(3), startCode(4), new Uint8Array([0x67])))).toBeNull();
    expect(parseH264AnnexB(concat(annexBNal(7), startCode(3)))).toBeNull();
  });

  it('rejects a NAL unit whose forbidden_zero_bit is set', () => {
    expect(parseH264AnnexB(concat(startCode(4), new Uint8Array([0x87, 0x80])))).toBeNull();
  });

  it('accepts at most 256 NAL units and rejects a 257th', () => {
    const accepted = concat(...Array.from({ length: 256 }, () => annexBNal(9)));
    const excessive = concat(accepted, annexBNal(9));

    expect(parseH264AnnexB(accepted)).toHaveLength(256);
    expect(parseH264AnnexB(excessive)).toBeNull();
  });

  it('rejects data partitions and unsupported pixel-bearing VCL extensions', () => {
    for (const type of [2, 3, 4, 19, 20, 21]) {
      expect(parseH264AnnexB(annexBNal(type)), `NAL type ${type}`).toBeNull();
    }
  });

  it('rejects reserved, unspecified, and unsupported extension NAL types', () => {
    for (const type of [0, 14, 16, 17, 18, 22, 23, 24, 31]) {
      expect(parseH264AnnexB(annexBNal(type)), `NAL type ${type}`).toBeNull();
    }
  });

  it('never throws on deterministic malformed byte buffers', () => {
    for (let length = 0; length <= 128; length++) {
      const data = Uint8Array.from({ length }, (_, index) => (length * 29 + index * 71) & 0xff);
      expect(() => parseH264AnnexB(data)).not.toThrow();
    }
  });
});

describe('findH264SlicePrefixLength', () => {
  it('returns the raw payload byte boundary covering the first three ue(v) fields', () => {
    // first_mb_in_slice=0 (`1`), slice_type=2 (`011`), pic_parameter_set_id=3 (`00100`).
    expect(findH264SlicePrefixLength(new Uint8Array([0xb2, 0x00, 0xaa]))).toBe(2);
  });

  it('counts existing emulation-prevention bytes in the returned raw boundary', () => {
    const payload = encodeUnsignedExpGolomb([0x7fffff, 0, 0]);

    expect(containsSequence(payload, [0x00, 0x00, 0x03])).toBe(true);
    expect(findH264SlicePrefixLength(payload)).toBe(payload.length);
  });

  it('rejects truncated, malformed, and excessively long ue(v) prefixes', () => {
    expect(findH264SlicePrefixLength(new Uint8Array())).toBeNull();
    expect(findH264SlicePrefixLength(new Uint8Array([0x00]))).toBeNull();
    expect(findH264SlicePrefixLength(new Uint8Array([0x00, 0x00, 0x03]))).toBeNull();
    expect(findH264SlicePrefixLength(encodeUnsignedExpGolomb([0xffffffff, 0, 0]))).toBeNull();
  });
});

describe('H.264 emulation-prevention stuffing', () => {
  it('escapes every 00 00 {00,01,02,03} sequence and round-trips it', () => {
    const plaintext = new Uint8Array([
      0x00, 0x00, 0x00, 0xff, 0x00, 0x00, 0x01, 0xff, 0x00, 0x00, 0x02, 0xff, 0x00, 0x00, 0x03,
    ]);
    const stuffed = requireBytes(stuffH264Bytes(plaintext));

    expect(Array.from(stuffed)).toEqual([
      0x00, 0x00, 0x03, 0x00, 0xff, 0x00, 0x00, 0x03, 0x01, 0xff, 0x00, 0x00, 0x03, 0x02, 0xff,
      0x00, 0x00, 0x03, 0x03,
    ]);
    expect(unstuffH264Bytes(stuffed)).toEqual(plaintext);
  });

  it('uses preceding zero-run context to prevent a start code across the clear boundary', () => {
    const clearPrefix = new Uint8Array([0x65, 0x00, 0x00]);
    const encryptedRegion = new Uint8Array([0x01, 0x02, 0x00, 0x00, 0x03]);
    const stuffed = requireBytes(stuffH264Bytes(encryptedRegion, 2));

    expect(Array.from(stuffed)).toEqual([0x03, 0x01, 0x02, 0x00, 0x00, 0x03, 0x03]);
    expect(containsSequence(concat(clearPrefix, stuffed), [0x00, 0x00, 0x01])).toBe(false);
    expect(containsSequence(concat(clearPrefix, stuffed), [0x00, 0x00, 0x00, 0x01])).toBe(false);
    expect(unstuffH264Bytes(stuffed, 2)).toEqual(encryptedRegion);
  });

  it('handles a one-byte zero run crossing the clear boundary', () => {
    const plaintext = new Uint8Array([0x00, 0x02]);
    const stuffed = requireBytes(stuffH264Bytes(plaintext, 1));

    expect(Array.from(stuffed)).toEqual([0x00, 0x03, 0x02]);
    expect(unstuffH264Bytes(stuffed, 1)).toEqual(plaintext);
  });

  it('proves stuffed encrypted regions contain no accidental Annex-B delimiter', () => {
    const plaintext = new Uint8Array([
      0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x02, 0x00, 0x00, 0x03, 0x00, 0x00, 0x01,
    ]);

    for (const precedingZeroRun of [0, 1, 2]) {
      const stuffed = requireBytes(stuffH264Bytes(plaintext, precedingZeroRun));
      const prefix = new Uint8Array(precedingZeroRun);
      const combined = concat(prefix, stuffed);
      expect(containsSequence(combined, [0x00, 0x00, 0x01])).toBe(false);
      expect(unstuffH264Bytes(stuffed, precedingZeroRun)).toEqual(plaintext);
    }
  });

  it('rejects invalid zero-run context and malformed stuffed data', () => {
    expect(stuffH264Bytes(new Uint8Array([0x01]), -1)).toBeNull();
    expect(stuffH264Bytes(new Uint8Array([0x01]), 3)).toBeNull();
    expect(unstuffH264Bytes(new Uint8Array([0x01]), 3)).toBeNull();
    expect(unstuffH264Bytes(new Uint8Array([0x00, 0x00, 0x01]))).toBeNull();
    expect(unstuffH264Bytes(new Uint8Array([0x00, 0x00, 0x03]))).toBeNull();
    expect(unstuffH264Bytes(new Uint8Array([0x00, 0x00, 0x03, 0x04]))).toBeNull();
  });
});
