// client/desktop/tests/unit/scripts/assert-binary-asset-set.test.ts
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { describe, it, expect, afterEach, beforeEach } from 'vitest';
import { assertBinaryAssetSet } from '../../../scripts/assert-binary-asset-set.mjs';

const REQUIRE = ['*arm64*.zip', '*arm64*.dmg', '*x64*.zip', '*x64*.dmg'];

describe('assertBinaryAssetSet (#643 — per-arch binary-set guard)', () => {
  let dir: string;
  beforeEach(() => {
    dir = mkdtempSync(path.join(tmpdir(), 'assert-binary-asset-set-'));
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  function writeBinaries(root: string) {
    // nested per-leg bundle dirs, mirroring downloaded artifacts/ layout
    mkdirSync(path.join(root, 'concord-voice-mac-arm64-v1/zip/darwin/arm64'), { recursive: true });
    mkdirSync(path.join(root, 'concord-voice-mac-x64-v1/zip/darwin/x64'), { recursive: true });
    writeFileSync(
      path.join(
        root,
        'concord-voice-mac-arm64-v1/zip/darwin/arm64/Concord Voice-darwin-arm64-1.0.0.zip'
      ),
      'z'
    );
    writeFileSync(path.join(root, 'concord-voice-mac-arm64-v1/Concord Voice-1.0.0-arm64.dmg'), 'd');
    writeFileSync(
      path.join(root, 'concord-voice-mac-x64-v1/zip/darwin/x64/Concord Voice-darwin-x64-1.0.0.zip'),
      'z'
    );
    writeFileSync(path.join(root, 'concord-voice-mac-x64-v1/Concord Voice-1.0.0-x64.dmg'), 'd');
  }

  it('returns ok when every required arch+ext pattern matches a nested file', () => {
    writeBinaries(dir);
    const result = assertBinaryAssetSet({ dir, require: REQUIRE });
    expect(result.ok).toBe(true);
    expect(result.errors).toEqual([]);
  });

  it('fails when the arm64 dmg is missing', () => {
    writeBinaries(dir);
    rmSync(path.join(dir, 'concord-voice-mac-arm64-v1/Concord Voice-1.0.0-arm64.dmg'));
    const result = assertBinaryAssetSet({ dir, require: REQUIRE });
    expect(result.ok).toBe(false);
    expect(result.errors).toContain('No artifact matches required pattern *arm64*.dmg');
  });

  it('fails with deterministic ordering when all patterns are unmatched', () => {
    const result = assertBinaryAssetSet({ dir, require: REQUIRE });
    expect(result.ok).toBe(false);
    expect(result.errors).toEqual([
      'No artifact matches required pattern *arm64*.zip',
      'No artifact matches required pattern *arm64*.dmg',
      'No artifact matches required pattern *x64*.zip',
      'No artifact matches required pattern *x64*.dmg',
    ]);
  });

  it('fails when target directory does not exist', () => {
    const missing = path.join(dir, 'nope');
    const result = assertBinaryAssetSet({ dir: missing, require: ['*arm64*.zip'] });
    expect(result.ok).toBe(false);
    expect(result.errors[0]).toContain(`Target directory ${missing} does not exist`);
  });

  it('fails when target path is not a directory', () => {
    const filePath = path.join(dir, 'a-file');
    writeFileSync(filePath, 'x');
    const result = assertBinaryAssetSet({ dir: filePath, require: ['*arm64*.zip'] });
    expect(result.ok).toBe(false);
    expect(result.errors[0]).toContain('exists but is not a directory');
  });

  it('does not false-match the .sha512 sidecars as binaries', () => {
    mkdirSync(path.join(dir, 'leg'), { recursive: true });
    writeFileSync(path.join(dir, 'leg/Concord Voice-darwin-arm64-1.0.0.zip.sha512'), 'hash');
    const result = assertBinaryAssetSet({ dir, require: ['*arm64*.zip'] });
    expect(result.ok).toBe(false); // sidecar ends in .sha512, not .zip
  });
});
