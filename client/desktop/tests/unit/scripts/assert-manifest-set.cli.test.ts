// client/desktop/tests/unit/scripts/assert-manifest-set.cli.test.ts
//
// Subprocess-spawn tests for the CLI shim in assert-manifest-set.mjs. The
// in-process unit tests (assert-manifest-set.test.ts) cover the exported
// assertManifestSet function with 100% line/branch coverage, but the CLI
// shim itself is excluded from coverage via `/* istanbul ignore next */`.
// The shim is the production entry point (the build-desktop.yml L2 step
// invokes `node scripts/assert-manifest-set.mjs --dir=... --require=...`),
// so the exit codes, the `::error::` annotation prefix, and the argv
// parsing are all part of the workflow contract. This file exercises that
// contract end-to-end via spawnSync.
//
// Why not just trust the pure-function tests: a refactor of the argv
// parser (e.g., changing --require to --files, or switching to a flag
// library) wouldn't change the unit-test signal, but would silently break
// the workflow step until first real release.

import { spawnSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, it, expect, afterEach, beforeEach } from 'vitest';

const SCRIPT_PATH = path.resolve(__dirname, '../../../scripts/assert-manifest-set.mjs');

describe('assert-manifest-set CLI (issue #1009 — L2 workflow contract)', () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(path.join(tmpdir(), 'assert-manifest-cli-'));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it('exits 0 with OK: stdout on happy path', () => {
    writeFileSync(path.join(dir, 'latest-mac.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux.yml'), 'version: 0.1.36\n');

    const result = spawnSync(
      process.execPath,
      [SCRIPT_PATH, `--dir=${dir}`, '--require=latest-mac.yml,latest.yml,latest-linux.yml'],
      { encoding: 'utf8' }
    );

    expect(result.status).toBe(0);
    expect(result.stdout).toMatch(/^OK: all 3 required manifest\(s\) present in /);
    expect(result.stderr).toBe('');
  });

  it('exits 1 with ::error:: stderr when a required manifest is missing', () => {
    // latest-mac.yml deliberately omitted
    writeFileSync(path.join(dir, 'latest.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux.yml'), 'version: 0.1.36\n');

    const result = spawnSync(
      process.execPath,
      [SCRIPT_PATH, `--dir=${dir}`, '--require=latest-mac.yml,latest.yml,latest-linux.yml'],
      { encoding: 'utf8' }
    );

    expect(result.status).toBe(1);
    // The ::error:: prefix is load-bearing: GitHub Actions parses it into
    // a workflow annotation surfaced on the PR's Checks tab.
    expect(result.stderr).toContain('::error::Required manifest latest-mac.yml is missing');
    // Other (present) manifests should NOT generate error lines.
    expect(result.stderr).not.toContain('latest.yml is missing');
    expect(result.stderr).not.toContain('latest-linux.yml is missing');
  });

  it('exits 2 with Usage: stderr when invoked without arguments', () => {
    const result = spawnSync(process.execPath, [SCRIPT_PATH], { encoding: 'utf8' });

    expect(result.status).toBe(2);
    expect(result.stderr).toContain('Usage: assert-manifest-set.mjs --dir=<path> --require=');
  });

  it('exits 2 with Usage: stderr when --require is empty', () => {
    // Empty --require= splits to [''], filtered to [] by .filter(Boolean),
    // length 0 → hits the usage-error path.
    const result = spawnSync(process.execPath, [SCRIPT_PATH, `--dir=${dir}`, '--require='], {
      encoding: 'utf8',
    });

    expect(result.status).toBe(2);
    expect(result.stderr).toContain('Usage: assert-manifest-set.mjs');
  });
});
