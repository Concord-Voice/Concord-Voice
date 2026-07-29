import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const layoutCss = readFileSync(
  resolve(__dirname, '../../../src/renderer/components/Layout/AppLayout.css'),
  'utf8'
);

describe('#1750 sidebar controls', () => {
  it('keeps the full-size actions-only pin clear of two adjacent header actions', () => {
    const rule = layoutCss.match(/\.dock-shell__header--actions-only\s*\{[^}]+\}/)?.[0];

    expect(rule).toContain('right: calc(56px * var(--sp, 1))');
  });
});
