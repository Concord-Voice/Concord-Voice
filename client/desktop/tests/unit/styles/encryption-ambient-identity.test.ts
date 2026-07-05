import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom does not evaluate stylesheet CSS (getComputedStyle only reflects inline
// styles), so #1041's token-driven values and structural rules are asserted at
// the CSS source level — the same approach as styles/design-tokens.test.ts.

const messageCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Chat/Message.css'),
  'utf8'
);
const layoutCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Layout/AppLayout.css'),
  'utf8'
);

// Isolate a rule block's body (between its `{` and the next `}`).
function ruleBlock(css: string, selector: string): string {
  const start = css.indexOf(selector + ' {');
  if (start === -1) return '';
  const open = css.indexOf('{', start);
  const close = css.indexOf('}', open);
  return css.slice(open + 1, close);
}

describe('#1041 decrypt-failed elevated surface (Message.css)', () => {
  const base = ruleBlock(messageCss, '.decrypt-failed');
  const pending = ruleBlock(messageCss, '.decrypt-failed.pending-keys');

  it('.decrypt-failed is a non-italic bordered surface', () => {
    expect(base).toMatch(/font-style:\s*normal/);
    expect(base).toMatch(/padding:/);
    expect(base).toMatch(/border-radius:\s*var\(--radius-base\)/);
    expect(base).toMatch(/border:\s*1px solid/);
    expect(base).toMatch(/color-mix\(in srgb, var\(--accent-primary\)/);
  });

  it('.decrypt-failed is no longer italic', () => {
    expect(base).not.toMatch(/font-style:\s*italic/);
  });

  it('.pending-keys tints with the encryption-pending token, not #faa61a', () => {
    expect(pending).toMatch(/var\(--state-encryption-pending\)/);
    expect(pending).not.toMatch(/#faa61a/i);
    expect(pending).toMatch(/background:\s*color-mix\(in srgb, var\(--state-encryption-pending\)/);
  });
});

describe('#1041 decrypted-reveal animation (Message.css)', () => {
  it('defines the decrypted-reveal keyframe', () => {
    expect(messageCss).toMatch(/@keyframes\s+decrypted-reveal\s*\{/);
  });

  it('drives the reveal via animation-duration so the global reduced-motion kill-switch zeroes it', () => {
    // styles/index.css:215-222 zeroes animation-duration via `* !important` when
    // [data-reduce-animations='true']; using the `animation:` shorthand (which sets
    // animation-duration) makes the reveal reduced-motion-compliant by construction.
    const block = messageCss.slice(messageCss.indexOf('.message-text.decrypted-reveal'));
    expect(block).toMatch(/animation:\s*decrypted-reveal var\(--motion-duration-base\)/);
  });
});

describe('#1041 app-chrome encryption hairline (AppLayout.css)', () => {
  const before = ruleBlock(layoutCss, '.app-layout::before');

  it('defines a non-interactive top hairline over the accent gradient', () => {
    expect(before).toMatch(/content:\s*''/);
    expect(before).toMatch(/position:\s*absolute/);
    expect(before).toMatch(/height:\s*1px/);
    expect(before).toMatch(/pointer-events:\s*none/);
    expect(before).toMatch(
      /linear-gradient\(90deg, transparent 0%, var\(--accent-primary\) 50%, transparent 100%\)/
    );
    expect(before).toMatch(/opacity:\s*0\.3/);
  });

  it('.app-layout is a positioning context for the pseudo', () => {
    expect(ruleBlock(layoutCss, '.app-layout')).toMatch(/position:\s*relative/);
  });
});
