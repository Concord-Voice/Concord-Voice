import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// Asserted against the SOURCE, not rendered behaviour -- #2370 deleted
// MessageInput's own picker-box prediction (the picker now measures and
// places itself via utils/ui/pickerAnchor.ts), and what this guards is an
// absence: no picker width/height literal should ever creep back into this
// file, and getPickerPosition's signature should stay anchor-geometry-only
// (one argument), never widening back into predicting a box.
const src = readFileSync(
  resolve(__dirname, '../../../../src/renderer/components/Chat/MessageInput.tsx'),
  'utf-8'
);

// Strip full-line comments before matching: MessageInput.tsx's own comment
// explaining the removal names the very literals (352/370/520) this test
// proves absent from CODE, so matching raw source text would trip on the
// prose describing the absence rather than a real occurrence.
const codeOnly = src.replace(/^[ \t]*\/\/.*$/gm, '');

describe('MessageInput picker-geometry contract (#2370 A9)', () => {
  it('contains no picker width or height literal (the old 352/370/374/520), and no `pickerHeight` identifier', () => {
    for (const literal of ['352', '370', '374', '520']) {
      expect(
        codeOnly,
        `MessageInput.tsx must not contain the old picker-size literal ${literal} in code`
      ).not.toMatch(new RegExp(`\\b${literal}\\b`));
    }
    expect(codeOnly).not.toMatch(/\bpickerHeight\b/);
  });

  it('getPickerPosition contains no dimension literal at all -- not just the retired four', () => {
    // The blacklist above pins the PAST. A NEW literal (426, 598, ...) sails
    // through it while reintroducing exactly the picker-box prediction #2370
    // deleted. The real property is that this function derives solely from the
    // anchor rect, so the only numbers it may contain are the `0` defaults and
    // the `2` of the centre calculation.
    const fn = /const getPickerPosition = useCallback\([\s\S]*?\n {2}\}, \[\]\);/.exec(codeOnly);
    expect(fn, 'getPickerPosition definition not found -- has it been renamed?').not.toBeNull();
    const numbers = (fn![0].match(/\b\d+(?:\.\d+)?\b/g) ?? []).filter(
      (n) => n !== '0' && n !== '2'
    );
    expect(
      numbers,
      `getPickerPosition must derive only from the anchor; found literal(s): ${numbers.join(', ')}`
    ).toEqual([]);
  });

  it('getPickerPosition takes exactly one argument', () => {
    const m = /const getPickerPosition = useCallback\(\s*\(([^)]*)\)/.exec(codeOnly);
    expect(m, 'getPickerPosition definition not found -- has it been renamed?').not.toBeNull();

    const params = m![1]
      .split(',')
      .map((p) => p.trim())
      .filter((p) => p.length > 0);
    expect(
      params,
      `getPickerPosition must take exactly one argument (anchor geometry only); found: [${params.join(', ')}]`
    ).toHaveLength(1);
  });
});
