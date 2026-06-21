import { describe, it, expect } from 'vitest';
import { readFileSync } from 'fs';
import { resolve } from 'path';

/**
 * SPA Deploy Contract — Build Invariant (ADR-0001)
 *
 * vite.config.ts must keep `base: './'` so that all chunks emit relative URLs.
 * The bundle is path-independent at build time, which is what makes the
 * /spa/<sha>/ handler-path swap safe. Changing `base` to anything else
 * couples the bundle hash to the handler path and breaks the deploy contract.
 *
 * If this test fails, read [internal]0001-spa-deploy-contract.md before
 * "fixing" it.
 */
describe('vite.config.ts — SPA deploy contract', () => {
  it("keeps base: './' so chunks emit relative URLs (ADR-0001)", () => {
    const configPath = resolve(__dirname, '../../../vite.config.ts');
    const content = readFileSync(configPath, 'utf8');
    // Tolerate whitespace and either quote style; reject any other base value.
    expect(content).toMatch(/base:\s*['"]\.\/['"]/);
  });

  it('does not set an absolute base (no leading slash)', () => {
    const configPath = resolve(__dirname, '../../../vite.config.ts');
    const content = readFileSync(configPath, 'utf8');
    expect(content).not.toMatch(/base:\s*['"]\/.+['"]/);
  });
});
