// @vitest-environment node
/**
 * Unit tests for the buildtag.json generator (#920 §5.13).
 *
 * Covers the pure helpers (`resolveBuildTag`, `formatBuildtagJson`) exposed
 * by `generate-buildtag.mjs`. The CLI shim at the bottom of the script is
 * `istanbul ignore`d because its side-effects (fs writes, process.cwd) are
 * not productively unit-tested.
 *
 * Naming note: the env-var was renamed from `VITE_BUILD_TAG` →
 * `CONCORD_BUILD_TAG` to avoid the Vite-env leak path (per Copilot review
 * on PR #982). Vite's dotenv loader picks up `VITE_*`-prefixed vars and
 * exposes them via `import.meta.env`; the non-Vite prefix keeps the
 * forensic tag main-process-only.
 */

import { describe, it, expect } from 'vitest';
import { resolveBuildTag, formatBuildtagJson } from './generate-buildtag.mjs';

describe('resolveBuildTag', () => {
  it('prefers explicit process.env.CONCORD_BUILD_TAG when set', () => {
    const env = { CONCORD_BUILD_TAG: 'pr-smoke-from-env' };
    expect(resolveBuildTag(env, null)).toBe('pr-smoke-from-env');
  });

  it('falls back to .env file when process.env is empty', () => {
    const envFile = 'VITE_API_HOST=api.example.com\nCONCORD_BUILD_TAG=pr-smoke-from-file\n';
    expect(resolveBuildTag({}, envFile)).toBe('pr-smoke-from-file');
  });

  it("returns 'unknown' when neither source provides the tag", () => {
    expect(resolveBuildTag({}, null)).toBe('unknown');
  });

  it("returns 'unknown' when both sources are empty strings", () => {
    expect(resolveBuildTag({ CONCORD_BUILD_TAG: '' }, '')).toBe('unknown');
  });

  it('prefers process.env even when .env has a different value (CI override)', () => {
    const env = { CONCORD_BUILD_TAG: 'ci-override' };
    const envFile = 'CONCORD_BUILD_TAG=local-dev-value\n'; // pragma: allowlist secret
    expect(resolveBuildTag(env, envFile)).toBe('ci-override');
  });

  it("returns 'unknown' when .env has CONCORD_BUILD_TAG= with empty value", () => {
    const envFile = 'CONCORD_BUILD_TAG=\nVITE_API_HOST=example.com\n';
    expect(resolveBuildTag({}, envFile)).toBe('unknown');
  });

  it('trims whitespace from the .env value', () => {
    const envFile = 'CONCORD_BUILD_TAG=   pr-smoke-trimmed   \n';
    expect(resolveBuildTag({}, envFile)).toBe('pr-smoke-trimmed');
  });

  it('ignores unrelated env-file keys with similar names', () => {
    const envFile = 'NOT_CONCORD_BUILD_TAG=decoy\nCONCORD_BUILD_TAGS=plural-decoy\n'; // pragma: allowlist secret  (decoy env-keys for regex specificity test; no secret material)
    expect(resolveBuildTag({}, envFile)).toBe('unknown');
  });

  it('does NOT pick up legacy VITE_BUILD_TAG (regression guard for the rename)', () => {
    // The historical name was VITE_BUILD_TAG. After rename, the generator
    // MUST read only CONCORD_BUILD_TAG so a leftover VITE_BUILD_TAG (e.g.,
    // a stale shell or untracked .env) doesn't silently revive the
    // Vite-env exposure path that the rename was designed to close.
    const env = { VITE_BUILD_TAG: 'legacy-leak' };
    const envFile = 'VITE_BUILD_TAG=legacy-leak-in-file\n';
    expect(resolveBuildTag(env, envFile)).toBe('unknown');
  });
});

describe('formatBuildtagJson', () => {
  it('produces a parseable JSON document with tag field', () => {
    const output = formatBuildtagJson('pr-smoke-12345');
    expect(JSON.parse(output)).toEqual({ tag: 'pr-smoke-12345' });
  });

  it('appends a trailing newline (POSIX-friendly)', () => {
    expect(formatBuildtagJson('x').endsWith('\n')).toBe(true);
  });

  it('handles the unknown fallback tag', () => {
    const output = formatBuildtagJson('unknown');
    expect(JSON.parse(output)).toEqual({ tag: 'unknown' });
  });
});
