/**
 * Regression test for the no-bare-https-anchor rule.
 *
 * Locks the behaviour of the two `no-restricted-syntax` selectors added to
 * `eslint.config.mjs` for issue #754. If this test breaks, the rule has
 * been weakened or removed — either fix the rule or confirm the weakening
 * is intentional (and update the test).
 *
 * Spec: [internal]specs/2026-04-26-754-externalize-blocked-nav-design.md
 */
import { ESLint } from 'eslint';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const CLIENT_DESKTOP_ROOT = path.resolve(__dirname, '../../..');

async function lintRenderer(
  code: string
): Promise<Array<{ ruleId: string | null; message: string }>> {
  const eslint = new ESLint({
    cwd: CLIENT_DESKTOP_ROOT,
  });
  // Use a fixture path under src/renderer/ so the renderer-scoped rule applies.
  // The fixture path 'src/renderer/__lint-fixture__.tsx' is a virtual file that
  // does not exist on disk. It MUST be enumerated in eslint.config.mjs's
  // `parserOptions.projectService.allowDefaultProject` array (since #530's
  // migration to type-aware linting) or typescript-eslint's project service
  // rejects it with a parsing error. If you add a new lint-rule regression test
  // that uses a different virtual fixture path, update that allowlist as well.
  const [result] = await eslint.lintText(code, {
    filePath: path.join(CLIENT_DESKTOP_ROOT, 'src/renderer/__lint-fixture__.tsx'),
  });
  return result.messages.map((m) => ({ ruleId: m.ruleId, message: m.message }));
}

function hasBareAnchorViolation(
  messages: Array<{ ruleId: string | null; message: string }>
): boolean {
  return messages.some(
    (m) => m.ruleId === 'no-restricted-syntax' && /target="_blank"/.test(m.message)
  );
}

describe('no-restricted-syntax — bare https anchor in renderer', () => {
  it('flags <a href="https://..."> with no target attribute', async () => {
    const messages = await lintRenderer(`
      export const X = () => <a href="https://concordvoice.com/x">click</a>;
    `);
    expect(hasBareAnchorViolation(messages)).toBe(true);
  });

  it('flags <a href={`https://${h}/p`}> template literal with no target', async () => {
    const messages = await lintRenderer(`
      export const Y = ({ h }: { h: string }) => (
        <a href={\`https://\${h}/p\`}>click</a>
      );
    `);
    expect(hasBareAnchorViolation(messages)).toBe(true);
  });

  it('allows <a href="https://..." target="_blank" rel="noopener noreferrer">', async () => {
    const messages = await lintRenderer(`
      export const Z = () => (
        <a href="https://concordvoice.com/x" target="_blank" rel="noopener noreferrer">click</a>
      );
    `);
    expect(hasBareAnchorViolation(messages)).toBe(false);
  });

  it('allows <a href="https://..." target="_blank"> (rule checks target presence only)', async () => {
    const messages = await lintRenderer(`
      export const W = () => <a href="https://concordvoice.com/x" target="_blank">click</a>;
    `);
    expect(hasBareAnchorViolation(messages)).toBe(false);
  });

  it('allows <a href="/relative">', async () => {
    const messages = await lintRenderer(`
      export const R = () => <a href="/internal">click</a>;
    `);
    expect(hasBareAnchorViolation(messages)).toBe(false);
  });

  it('allows <a href="mailto:x@y">', async () => {
    const messages = await lintRenderer(`
      export const M = () => <a href="mailto:x@y.com">email</a>;
    `);
    expect(hasBareAnchorViolation(messages)).toBe(false);
  });

  it('allows <a href={someVar}> (variable href, not statically https)', async () => {
    const messages = await lintRenderer(`
      export const V = ({ link }: { link: string }) => <a href={link}>click</a>;
    `);
    expect(hasBareAnchorViolation(messages)).toBe(false);
  });

  it('allows <a href={DOWNLOAD_URL}> (module constant, not literal)', async () => {
    const messages = await lintRenderer(`
      const DOWNLOAD_URL = "https://concordvoice.com/download";
      export const D = () => <a href={DOWNLOAD_URL}>click</a>;
    `);
    expect(hasBareAnchorViolation(messages)).toBe(false);
  });
});
