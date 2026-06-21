import { describe, it, expect } from 'vitest';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join, resolve } from 'node:path';

const CLIENT_DESKTOP = resolve(__dirname, '../..');
const SRC = join(CLIENT_DESKTOP, 'src');

function* walk(dir: string): Generator<string> {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      yield* walk(full);
    } else if (/\.(ts|tsx|js|mjs|cjs)$/.test(entry)) {
      yield full;
    }
  }
}

describe('no-sentry-imports regression guard', () => {
  // Patterns to detect any module-loading form that pulls in @sentry/*.
  // Each pattern targets a distinct ECMAScript loader form so a future
  // re-introduction can't slip in via an under-tested syntax. Per Copilot
  // review on PR #793, the original `from`-only check missed side-effect
  // imports, dynamic imports, and re-exports.
  const SENTRY_IMPORT_PATTERNS: ReadonlyArray<RegExp> = [
    // `import x from '@sentry/...'`, `export * from '@sentry/...'`,
    // `export { x } from '@sentry/...'`
    /from\s+['"]@sentry\//,
    // `require('@sentry/...')`
    /require\s*\(\s*['"]@sentry\//,
    // `import '@sentry/...'` (side-effect import — no `from` clause)
    /import\s+['"]@sentry\//,
    // `import('@sentry/...')` (dynamic import; previously used by
    // tokenManager.ts:464 prior to #757 strip)
    /import\s*\(\s*['"]@sentry\//,
  ];

  it('no source file under client/desktop/src imports @sentry/* (any module-loading form)', () => {
    const offenders: string[] = [];
    for (const file of walk(SRC)) {
      const content = readFileSync(file, 'utf8');
      if (SENTRY_IMPORT_PATTERNS.some((p) => p.test(content))) {
        offenders.push(file.replace(CLIENT_DESKTOP + '/', ''));
      }
    }
    expect(
      offenders,
      `Files importing @sentry/* (none expected post-#757):\n${offenders.join('\n')}`
    ).toEqual([]);
  });

  it('package.json contains no @sentry/* dependencies', () => {
    const pkg = JSON.parse(readFileSync(join(CLIENT_DESKTOP, 'package.json'), 'utf8')) as {
      dependencies?: Record<string, string>;
      devDependencies?: Record<string, string>;
      peerDependencies?: Record<string, string>;
    };
    const allDeps = {
      ...(pkg.dependencies ?? {}),
      ...(pkg.devDependencies ?? {}),
      ...(pkg.peerDependencies ?? {}),
    };
    const sentryDeps = Object.keys(allDeps).filter((k) => k.startsWith('@sentry/'));
    expect(sentryDeps).toEqual([]);
  });
});
