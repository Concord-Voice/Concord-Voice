import { describe, it, expect } from 'vitest';
// Relative import (not @/ alias) so Istanbul coverage instrumentation tracks the file correctly.
import {
  QUANTUM_BYTES,
  HEADER_BYTES,
  decodeQuantumHeader,
  encodeQuantumHeader,
  isAudiocapHello,
  sanitizeDiagnostic,
} from '../../../src/shared/audiocapProtocol';

/**
 * Unit tests for the audiocap shared wire protocol (#3195): the 32-byte
 * quantum header codec and the `parentPort` `hello` narrowing predicate.
 *
 * Per spec §4c, "No field selects a code path other than accept-or-drop" —
 * `decodeQuantumHeader` must return `null`, never a partially-populated
 * object, for any input that fails a single field check. The property test
 * below is what pins that: it is the test that would catch a decoder that
 * half-fills its result before discovering a later field is wrong.
 */

const goodHeader = (): ArrayBuffer => {
  const buf = new ArrayBuffer(QUANTUM_BYTES);
  encodeQuantumHeader(buf, { seq: 7, captureTimestampNs: 123n, overrunTotal: 4 });
  return buf;
};

describe('decodeQuantumHeader', () => {
  it('round-trips an encoded header', () => {
    // "Obeyed", not merely "passed": the decoded object's fields equal what
    // encodeQuantumHeader was asked to write, not merely that decode
    // returned non-null.
    expect(decodeQuantumHeader(goodHeader())).toEqual({
      seq: 7,
      captureTimestampNs: 123n,
      overrunTotal: 4,
    });
  });

  // Every one of these means "close the port" -- decode returns null, never a partial.
  it.each([
    ['wrong byteLength (short)', () => new ArrayBuffer(QUANTUM_BYTES - 1)],
    ['wrong byteLength (long)', () => new ArrayBuffer(QUANTUM_BYTES + 1)],
    [
      'bad magic',
      () => {
        const b = goodHeader();
        new DataView(b).setUint16(0, 0x0000, true);
        return b;
      },
    ],
    [
      'unknown version',
      () => {
        const b = goodHeader();
        new DataView(b).setUint8(2, 2);
        return b;
      },
    ],
    [
      'non-zero flags',
      () => {
        const b = goodHeader();
        new DataView(b).setUint8(3, 1);
        return b;
      },
    ],
    [
      'wrong sampleRate',
      () => {
        const b = goodHeader();
        new DataView(b).setUint32(8, 44100, true);
        return b;
      },
    ],
    [
      'wrong channels',
      () => {
        const b = goodHeader();
        new DataView(b).setUint16(12, 1, true);
        return b;
      },
    ],
    [
      'wrong frameCount',
      () => {
        const b = goodHeader();
        new DataView(b).setUint16(14, 960, true);
        return b;
      },
    ],
    [
      'reserved not 0',
      () => {
        const b = goodHeader();
        new DataView(b).setUint32(28, 9, true);
        return b;
      },
    ],
  ])('returns null for %s', (_label, make) => {
    expect(decodeQuantumHeader(make())).toBeNull();
  });

  // The "accept-or-drop, no partial" claim of spec §4c needs BOTH arms below.
  //
  // An earlier revision of this test randomized all 32 header bytes and put every
  // assertion inside `if (out !== null)`, with no else. A random header is accepted
  // only if it hits magic (2^-16) AND version (2^-8) AND flags (2^-8) AND sampleRate
  // (2^-32) AND channels (2^-16) AND frameCount (2^-16) AND reserved (2^-32) -- about
  // 2^-128. The body therefore never executed, and nothing asserted rejection either,
  // so it passed against ANY implementation, a partial-returning one included. It
  // asserted the property in its name and tested nothing. Do not restore that shape.
  it('rejects arbitrary bytes outright -- never a partial', () => {
    for (let i = 0; i < 2000; i++) {
      const b = new ArrayBuffer(QUANTUM_BYTES);
      const u8 = new Uint8Array(b);
      for (let j = 0; j < HEADER_BYTES; j++) u8[j] = Math.floor(Math.random() * 256);
      // The assertion is unconditional: arbitrary bytes are REJECTED, full stop.
      expect(decodeQuantumHeader(b)).toBeNull();
    }
  });

  it('fully populates every field when only the variable fields vary', () => {
    for (let i = 0; i < 2000; i++) {
      // Start from a header the decoder MUST accept, then randomize only the three
      // fields §4c leaves unconstrained. This arm actually reaches the success path,
      // which is what the vacuous version never did.
      const seq = Math.floor(Math.random() * 0x1_0000_0000);
      const overrunTotal = Math.floor(Math.random() * 0x1_0000_0000);
      const captureTimestampNs = BigInt(Math.floor(Math.random() * Number.MAX_SAFE_INTEGER));
      const b = new ArrayBuffer(QUANTUM_BYTES);
      encodeQuantumHeader(b, { seq, captureTimestampNs, overrunTotal });

      const out = decodeQuantumHeader(b);
      expect(out).not.toBeNull();
      // Exact values, not just types -- a decoder reading the wrong offset would
      // still produce a number of the right type at every field.
      expect(out).toEqual({ seq, captureTimestampNs, overrunTotal });
      // No undefined/missing field slipped through under a truthy check.
      expect(Object.keys(out!).sort()).toEqual(['captureTimestampNs', 'overrunTotal', 'seq']);
    }
  });
});

describe('isAudiocapHello', () => {
  const hello = {
    kind: 'hello',
    protocol: 1,
    capability: { platform: 'darwin', osVersion: '14.4', perProcessAudio: true, reason: '' },
    resourcesPathPresent: true,
    envKeys: ['CONCORD_AUDIOCAP_PATH'],
  };

  it('accepts a conforming hello', () => {
    expect(isAudiocapHello(hello)).toBe(true);
  });

  it('rejects a truthy non-boolean perProcessAudio', () =>
    expect(
      isAudiocapHello({ ...hello, capability: { ...hello.capability, perProcessAudio: 1 } })
    ).toBe(false));

  it('rejects a protocol mismatch', () =>
    expect(isAudiocapHello({ ...hello, protocol: 2 })).toBe(false));

  it('rejects control chars in a diagnostic string', () =>
    // Control char written as an escape, never a literal byte in the file.
    expect(
      isAudiocapHello({ ...hello, capability: { ...hello.capability, reason: 'a\u0007b' } })
    ).toBe(false));

  it('rejects an over-length diagnostic string', () =>
    expect(
      isAudiocapHello({ ...hello, capability: { ...hello.capability, reason: 'x'.repeat(65) } })
    ).toBe(false));
});

describe('sanitizeDiagnostic', () => {
  it('strips control characters', () => {
    // "Passed" (input carries a control char) paired with "obeyed" (the
    // control char is actually absent from the output, not merely that the
    // function returned something).
    const out = sanitizeDiagnostic('a\u0007b', 200);
    expect(out).toBe('ab');
    expect(/[\x00-\x1f\x7f]/.test(out)).toBe(false);
  });

  it('truncates to max', () => expect(sanitizeDiagnostic('z'.repeat(300), 200)).toHaveLength(200));

  // #3245 audit: the pre-existing coverage was ONE character (BEL) plus a
  // truncation case. `sanitizeDiagnostic` is the only thing between a hostile
  // child's chosen string and a log sink (CWE-117), so the class is enumerated
  // rather than sampled. Every one below was run against the shipped
  // implementation and stripped; this locks that in.
  //
  // Built with `String.fromCharCode` on purpose -- a literal control character
  // in a source file is the same hazard the function removes, and some tooling
  // refuses to handle it.
  it.each([
    ['LF', 0x0a],
    ['CR', 0x0d],
    ['TAB', 0x09],
    ['NUL', 0x00],
    ['BEL', 0x07],
    ['ESC (ANSI driver)', 0x1b],
    ['DEL', 0x7f],
    ['C1 NEL', 0x85],
    ['C1 CSI', 0x9b],
    ['LINE SEPARATOR U+2028', 0x2028],
    ['PARAGRAPH SEPARATOR U+2029', 0x2029],
    ['RTL OVERRIDE U+202E', 0x202e],
    ['ZERO WIDTH SPACE U+200B', 0x200b],
  ])('strips %s, so a forged record cannot be started', (_name, code) => {
    const out = sanitizeDiagnostic(
      `a${String.fromCharCode(code as number)}level=ERROR forged`,
      200
    );
    const bad = Array.from(out)
      .map((ch) => ch.codePointAt(0) as number)
      .filter(
        (c) =>
          c < 0x20 ||
          c === 0x7f ||
          (c >= 0x80 && c <= 0x9f) ||
          c === 0x2028 ||
          c === 0x2029 ||
          c === 0x202e ||
          c === 0x200b
      );
    expect(bad).toEqual([]);
  });

  // `max` is caller-supplied. These pin what a wrong value does rather than
  // leaving it to `String.prototype.slice`'s coercion rules.
  it('tolerates a hostile max without widening the cap', () => {
    expect(sanitizeDiagnostic('abcdefghij', 5)).toBe('abcde');
    expect(sanitizeDiagnostic('abcdefghij', 0)).toBe('');
    // Negative and NaN both floor to zero rather than slicing from the end.
    expect(sanitizeDiagnostic('abcdefghij', -1)).toBe('');
    expect(sanitizeDiagnostic('abcdefghij', NaN)).toBe('');
    // Fractional truncates toward zero; it must never round up.
    expect(sanitizeDiagnostic('abcdefghij', 1.9)).toBe('a');
    expect(sanitizeDiagnostic('abcdefghij', Infinity)).toBe('abcdefghij');
  });

  it('takes only a string, so Error.cause has no path to a sink', () => {
    const e = new Error('outer');
    (e as Error & { cause?: unknown }).cause = new Error('secret');
    // Obeyed: the sanitized output is exactly the message string, with no
    // trace of the cause's message anywhere in it.
    const out = sanitizeDiagnostic(e.message, 200);
    expect(out).toBe('outer');
    expect(out).not.toContain('secret');
  });
});
