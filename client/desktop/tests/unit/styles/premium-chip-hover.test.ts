import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

/**
 * The contrast ratchet (contrast-pairs.test.ts) compares declared colours; it cannot
 * see a `filter`, `opacity` or blend mode that re-tints them at render time. Two such
 * re-tints took chip text below 4.5:1: the interactive hover's `filter:
 * brightness(1.08)` (Agency dark 4.59:1 -> 4.02:1, and Pride light / Midnightsky light
 * also fell under) and the label's `opacity: 0.85` (Agency dark 3.76:1). The chip's
 * contrast is what the ratchet measured only if nothing in its stylesheet re-tints it,
 * so the whole file is checked: a second hover rule inside an `@media` block, or a
 * selector list, would slip past a check of one rule.
 */
const CSS = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/common/PremiumChip.css'),
  'utf8'
).replace(/\/\*[\s\S]*?\*\//g, '');

describe('PremiumChip keeps the measured contrast', () => {
  it('still gives the interactive chip a hover rule (an affordance exists to check)', () => {
    expect(CSS).toMatch(/\.premium-chip--interactive:hover\s*\{/);
  });

  it.each(['filter', 'opacity', 'mix-blend-mode'])(
    'declares no %s anywhere in PremiumChip.css',
    (property) => {
      expect(CSS).not.toMatch(new RegExp(`(^|[\\s;{])${property}\\s*:`));
    }
  );
});
