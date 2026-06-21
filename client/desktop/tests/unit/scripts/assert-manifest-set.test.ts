// client/desktop/tests/unit/scripts/assert-manifest-set.test.ts
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, it, expect, afterEach, beforeEach } from 'vitest';
import { assertManifestSet } from '../../../scripts/assert-manifest-set.mjs';

describe('assertManifestSet (issue #1009 — L2 fail-loud guard)', () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(path.join(tmpdir(), 'assert-manifest-set-'));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it('returns ok when all required manifests exist in target dir', () => {
    writeFileSync(path.join(dir, 'latest-mac.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux.yml'), 'version: 0.1.36\n');

    const result = assertManifestSet({
      dir,
      require: ['latest-mac.yml', 'latest.yml', 'latest-linux.yml'],
    });

    expect(result.ok).toBe(true);
    expect(result.errors).toEqual([]);
  });

  it('fails when latest-mac.yml is missing', () => {
    writeFileSync(path.join(dir, 'latest.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux.yml'), 'version: 0.1.36\n');

    const result = assertManifestSet({
      dir,
      require: ['latest-mac.yml', 'latest.yml', 'latest-linux.yml'],
    });

    expect(result.ok).toBe(false);
    expect(result.errors).toContain('Required manifest latest-mac.yml is missing');
  });

  it('fails when latest.yml is missing', () => {
    writeFileSync(path.join(dir, 'latest-mac.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux.yml'), 'version: 0.1.36\n');

    const result = assertManifestSet({
      dir,
      require: ['latest-mac.yml', 'latest.yml', 'latest-linux.yml'],
    });

    expect(result.ok).toBe(false);
    expect(result.errors).toContain('Required manifest latest.yml is missing');
  });

  it('fails when latest-linux.yml is missing', () => {
    writeFileSync(path.join(dir, 'latest-mac.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest.yml'), 'version: 0.1.36\n');

    const result = assertManifestSet({
      dir,
      require: ['latest-mac.yml', 'latest.yml', 'latest-linux.yml'],
    });

    expect(result.ok).toBe(false);
    expect(result.errors).toContain('Required manifest latest-linux.yml is missing');
  });

  it('fails with deterministic ordering when all three required manifests are missing', () => {
    const result = assertManifestSet({
      dir,
      require: ['latest-mac.yml', 'latest.yml', 'latest-linux.yml'],
    });

    expect(result.ok).toBe(false);
    // Errors must appear in the same order as the require[] input so log
    // output is reproducible across runs (the verify step prints all errors).
    expect(result.errors).toEqual([
      'Required manifest latest-mac.yml is missing',
      'Required manifest latest.yml is missing',
      'Required manifest latest-linux.yml is missing',
    ]);
  });

  it('returns ok when target dir contains extra manifests beyond the required set', () => {
    writeFileSync(path.join(dir, 'latest-mac.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux.yml'), 'version: 0.1.36\n');
    writeFileSync(path.join(dir, 'latest-linux-arm64.yml'), 'version: 0.1.36\n');

    const result = assertManifestSet({
      dir,
      require: ['latest-mac.yml', 'latest.yml', 'latest-linux.yml'],
    });

    expect(result.ok).toBe(true);
    expect(result.errors).toEqual([]);
  });

  it('fails when target directory does not exist', () => {
    const missing = path.join(dir, 'nonexistent-subdir');

    const result = assertManifestSet({
      dir: missing,
      require: ['latest-mac.yml'],
    });

    expect(result.ok).toBe(false);
    expect(result.errors[0]).toContain(`Target directory ${missing} does not exist`);
  });

  it('fails when target path exists but is not a directory', () => {
    const filePath = path.join(dir, 'not-a-dir');
    writeFileSync(filePath, 'content\n');

    const result = assertManifestSet({
      dir: filePath,
      require: ['latest-mac.yml'],
    });

    expect(result.ok).toBe(false);
    expect(result.errors[0]).toContain(`Target ${filePath} exists but is not a directory`);
  });
});
