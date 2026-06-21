/**
 * Regression test for the no-raw-err-to-console rule.
 *
 * Locks the behaviour of the `no-restricted-syntax` selector added to
 * `eslint.config.mjs` for issue #706. If this test breaks, the rule has
 * been weakened or removed — either fix the rule or confirm the weakening
 * is intentional (and update the test).
 *
 * Spec: [internal]specs/2026-04-20-706-audit-console-error-leaks-design.md
 */
import { ESLint } from 'eslint';
import path from 'node:path';
import { describe, expect, it } from 'vitest';

const CLIENT_DESKTOP_ROOT = path.resolve(__dirname, '../../..');

async function lint(code: string): Promise<Array<{ ruleId: string | null; message: string }>> {
  const eslint = new ESLint({
    cwd: CLIENT_DESKTOP_ROOT,
  });
  // The fixture path 'src/main/__lint-fixture__.ts' is a virtual file that does
  // not exist on disk. It MUST be enumerated in eslint.config.mjs's
  // `parserOptions.projectService.allowDefaultProject` array (since #530's
  // migration to type-aware linting) or typescript-eslint's project service
  // rejects it with a parsing error. If you add a new lint-rule regression test
  // that uses a different virtual fixture path, update that allowlist as well.
  const [result] = await eslint.lintText(code, {
    filePath: path.join(CLIENT_DESKTOP_ROOT, 'src/main/__lint-fixture__.ts'),
  });
  return result.messages.map((m) => ({ ruleId: m.ruleId, message: m.message }));
}

function hasConsoleRawErrViolation(
  messages: Array<{ ruleId: string | null; message: string }>
): boolean {
  return messages.some(
    (m) => m.ruleId === 'no-restricted-syntax' && /console\.(error|warn)/.test(m.message)
  );
}

describe('no-restricted-syntax — raw err to console.error/warn', () => {
  it('flags console.error with a bare err identifier', async () => {
    const messages = await lint(`
      export function failing(): void {
        try {
          throw new Error('boom');
        } catch (err) {
          console.error('[X] failed:', err);
        }
      }
    `);
    expect(hasConsoleRawErrViolation(messages)).toBe(true);
  });

  it('flags console.warn with a bare err identifier', async () => {
    const messages = await lint(`
      export function failing(): void {
        try {
          throw new Error('boom');
        } catch (err) {
          console.warn('[X] warning:', err);
        }
      }
    `);
    expect(hasConsoleRawErrViolation(messages)).toBe(true);
  });

  it('allows console.error with (err as Error).message', async () => {
    const messages = await lint(`
      export function passing(): void {
        try {
          throw new Error('boom');
        } catch (err) {
          console.error('[X] failed:', (err as Error).message);
        }
      }
    `);
    expect(hasConsoleRawErrViolation(messages)).toBe(false);
  });

  it('allows console.error with a template literal (no bare identifier)', async () => {
    const messages = await lint(`
      export function passing(msg: string): void {
        console.error(\`[X] \${msg}\`);
      }
    `);
    expect(hasConsoleRawErrViolation(messages)).toBe(false);
  });

  it('allows console.error with structured member-expression args (e.g., details.reason)', async () => {
    const messages = await lint(`
      type Details = { reason: string; exitCode: number };
      export function passing(details: Details): void {
        console.error('[X] renderer gone:', details.reason, 'exit:', details.exitCode);
      }
    `);
    expect(hasConsoleRawErrViolation(messages)).toBe(false);
  });

  // Locks the :not(:first-child) selector clause: single-argument forms
  // like `console.error(err)` (where the sole arg is both first- and
  // last-child) are intentionally PERMITTED by the rule. The audit found
  // no such sites in src/main/, and the rule targets the documented
  // failure mode — `console.error('prefix:', err)` — with an explicit
  // prefix message. If this decision ever changes, update both this
  // fixture and the selector block in eslint.config.mjs.
  it('permits single-argument console.error(err) (intentional — see eslint.config.mjs)', async () => {
    const messages = await lint(`
      export function edgeCase(): void {
        try {
          throw new Error('boom');
        } catch (err) {
          console.error(err);
        }
      }
    `);
    expect(hasConsoleRawErrViolation(messages)).toBe(false);
  });
});
