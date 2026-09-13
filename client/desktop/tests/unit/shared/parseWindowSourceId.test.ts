import { describe, expect, it } from 'vitest';

import { parseWindowSourceId } from '../../../src/shared/parseWindowSourceId';

describe('parseWindowSourceId', () => {
  it('accepts a canonical Electron window id and returns the handle', () => {
    expect(parseWindowSourceId('window:1:0')).toBe(1);
    expect(parseWindowSourceId('window:132458:0')).toBe(132458);
    expect(parseWindowSourceId('window:4294967295:0')).toBe(4294967295);
  });

  it('accepts any disambiguator, including zero and multi-digit, in canonical form', () => {
    expect(parseWindowSourceId('window:12:0')).toBe(12);
    expect(parseWindowSourceId('window:12:99')).toBe(12);
  });

  // THE D6 FENCE. A screen id must never parse as a window target, because the
  // per-process rung is the only rung a window may reach and a screen id reaching
  // it would be #2161 restated.
  it.each([
    ['screen id', 'screen:0:0'],
    ['screen id, high index', 'screen:12:1'],
  ])('refuses a %s', (_label, input) => {
    expect(parseWindowSourceId(input)).toBeNull();
  });

  // Hostile and malformed input. Every one of these fails CLOSED — the contract is
  // "a handle or nothing", never "a best guess".
  it.each([
    ['null', null],
    ['undefined', undefined],
    ['empty string', ''],
    ['handle zero', 'window:0:0'],
    ['leading zero (non-canonical duplicate of 7)', 'window:007:0'],
    ['negative handle', 'window:-1:0'],
    ['exponent notation', 'window:1e3:0'],
    ['hex notation', 'window:0x10:0'],
    ['above u32', 'window:4294967296:0'],
    ['far above u32', 'window:99999999999999999999:0'],
    ['float handle', 'window:1.5:0'],
    ['missing disambiguator', 'window:12'],
    ['extra segment', 'window:12:0:5'],
    ['leading space', ' window:12:0'],
    ['inner space', 'window: 12:0'],
    ['trailing newline', 'window:12:0\n'],
    ['wrong case', 'WINDOW:12:0'],
    ['prefix only', 'window:'],
    ['no prefix', '12:0'],
    ['unknown prefix', 'display:12:0'],
    ['non-canonical index (leading zero)', 'window:5:00'],
    ['non-canonical index (all zeros)', 'window:5:000'],
  ])('refuses %s', (_label, input) => {
    expect(parseWindowSourceId(input as string | null | undefined)).toBeNull();
  });

  // NON-ASCII DIGITS. JavaScript's `\d` is exactly `[0-9]` — it does NOT match
  // Arabic-Indic, fullwidth or Devanagari digits, and `Number()` returns NaN for them
  // anyway, so two independent layers refuse. Pinned because that is currently a
  // property of the LANGUAGE rather than of anything this file does deliberately: a
  // refactor to `\p{Nd}` with the `u` flag would silently open it, and nothing else
  // in this suite would notice.
  it.each([
    ['Arabic-Indic', 'window:\u0661\u0662:0'],
    ['fullwidth', 'window:\uFF11\uFF12:0'],
    ['Devanagari', 'window:\u0967\u0968:0'],
    ['ASCII digits then NBSP', 'window:12\u00A0:0'],
    ['zero-width joiner inside the handle', 'window:1\u200D2:0'],
  ])('refuses %s digits', (_label, input) => {
    expect(parseWindowSourceId(input)).toBeNull();
  });

  // This one is refused by THREE independent branches -- the length cap, `isSafeInteger`
  // and `> HANDLE_MAX` -- so it pins none of them, and the name asserts a PERFORMANCE
  // property ("without scanning all of it") that no assertion here checks. Kept as a DoS
  // smoke case; the discriminating fixture is the one below.
  it('refuses an absurdly long string', () => {
    expect(parseWindowSourceId(`window:${'1'.repeat(100_000)}:0`)).toBeNull();
  });

  // PINS `MAX_SOURCE_ID_CHARS` AND NOTHING ELSE. Measured in the #3198 Phase-8 review:
  // deleting the cap left all 37 of this file's inputs unchanged -- the mutant survived
  // the whole suite. This id is over the 64-char cap while every field is canonical and
  // the handle is comfortably inside u32, so the cap is the only branch that can refuse
  // it. Verified: with the cap removed it returns 12.
  it('refuses an over-length id even when every field is canonical and in range', () => {
    const id = `window:12:1${'0'.repeat(54)}`;
    expect(id.length).toBeGreaterThan(64);
    expect(parseWindowSourceId(id)).toBeNull();
  });

  it('refuses a non-string', () => {
    expect(parseWindowSourceId(12 as unknown as string)).toBeNull();
    expect(parseWindowSourceId({} as unknown as string)).toBeNull();
  });
});
