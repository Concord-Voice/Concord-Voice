// client/desktop/tests/unit/scripts/assert-binary-asset-set.cli.test.ts
//
// Subprocess-spawn tests for the CLI shim in assert-binary-asset-set.mjs.
// The in-process unit tests (assert-binary-asset-set.test.ts) cover the
// exported assertBinaryAssetSet function; the CLI shim itself is excluded
// from coverage via `/* istanbul ignore next */`. The shim is the production
// entry point (the build-desktop.yml "Assert required binary asset set" step
// invokes `node scripts/assert-binary-asset-set.mjs --dir=... --require=...`),
// so the exit codes, the `::error::` annotation prefix, and the argv parsing
// are part of the workflow contract. Mirrors assert-manifest-set.cli.test.ts
// — uses __dirname + process.execPath (NOT import.meta.url, which the repo's
// vitest transform does not expose; that mismatch broke this file in #1396).

import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, it, expect, afterEach, beforeEach } from 'vitest';

const SCRIPT_PATH = path.resolve(__dirname, '../../../scripts/assert-binary-asset-set.mjs');

describe('assert-binary-asset-set CLI (#643 — release-asset workflow contract)', () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(path.join(tmpdir(), 'abas-cli-'));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it('exits 0 with OK: stdout when all patterns match', () => {
    mkdirSync(path.join(dir, 'leg'), { recursive: true });
    writeFileSync(path.join(dir, 'leg/App-darwin-arm64-1.0.0.zip'), 'z');

    const result = spawnSync(
      process.execPath,
      [SCRIPT_PATH, `--dir=${dir}`, '--require=*arm64*.zip'],
      {
        encoding: 'utf8',
      }
    );

    expect(result.status).toBe(0);
    expect(result.stdout).toContain('OK:');
  });

  it('exits 1 with ::error:: stderr when a pattern is unmatched', () => {
    const result = spawnSync(
      process.execPath,
      [SCRIPT_PATH, `--dir=${dir}`, '--require=*arm64*.zip'],
      {
        encoding: 'utf8',
      }
    );

    expect(result.status).toBe(1);
    // The ::error:: prefix is load-bearing: GitHub Actions parses it into a
    // workflow annotation surfaced on the PR's Checks tab.
    expect(result.stderr).toContain('::error::No artifact matches required pattern *arm64*.zip');
  });

  it('exits 2 with Usage: stderr when invoked without --require', () => {
    const result = spawnSync(process.execPath, [SCRIPT_PATH, `--dir=${dir}`], { encoding: 'utf8' });

    expect(result.status).toBe(2);
    expect(result.stderr).toContain('Usage: assert-binary-asset-set.mjs');
  });
});
