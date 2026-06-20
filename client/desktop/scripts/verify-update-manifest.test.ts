// @vitest-environment node
import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { mkdtempSync, writeFileSync, rmSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { verifyManifest } from './verify-update-manifest.mjs';

function sha512Base64(buf: Buffer): string {
  return createHash('sha512').update(buf).digest('base64');
}

// 88-char base64 string with the right shape for schema validation but a
// value that will not match any real artifact. Used by hash-mismatch and
// missing-artifact tests where we need the schema to pass so the
// downstream hash/lookup check is exercised.
const PLAUSIBLE_WRONG_SHA512 = `${'A'.repeat(86)}==`;

let dir: string;

beforeEach(() => {
  dir = mkdtempSync(path.join(tmpdir(), 'verify-manifest-'));
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe('verifyManifest', () => {
  it('returns ok: true when all artifacts match', () => {
    const contents = Buffer.from('fake installer bytes');
    writeFileSync(path.join(dir, 'Concord-Setup.exe'), contents);
    const manifest = `version: 0.1.21\nfiles:\n  - url: Concord-Setup.exe\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, manifest);

    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(true);
    expect(result.errors).toEqual([]);
  });

  it('returns ok: true for a manifest entry carrying blockMapSize (#1292)', () => {
    const contents = Buffer.from('macos zip bytes');
    writeFileSync(path.join(dir, 'ConcordVoice-0.1.59-macos-arm64.zip'), contents);
    const manifest = `version: 0.1.59\nfiles:\n  - url: ConcordVoice-0.1.59-macos-arm64.zip\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\n    blockMapSize: 1234\nreleaseDate: '2026-06-15T00:00:00.000Z'\n`;
    const manifestPath = path.join(dir, 'latest-mac.yml');
    writeFileSync(manifestPath, manifest);

    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(true);
    expect(result.errors).toEqual([]);
  });

  it('returns ok: false when an artifact hash mismatches', () => {
    const contents = Buffer.from('fake installer bytes');
    writeFileSync(path.join(dir, 'Concord-Setup.exe'), contents);
    const manifest = `version: 0.1.21\nfiles:\n  - url: Concord-Setup.exe\n    sha512: ${PLAUSIBLE_WRONG_SHA512}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, manifest);

    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(false);
    expect(result.errors.some((e) => /sha512 mismatch/.test(e))).toBe(true);
  });

  it('returns ok: false when a referenced artifact is missing on disk', () => {
    const manifest = `version: 0.1.21\nfiles:\n  - url: Missing-Setup.exe\n    sha512: ${PLAUSIBLE_WRONG_SHA512}\n    size: 10\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, manifest);

    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(false);
    expect(result.errors.some((e) => /missing artifact|ENOENT/.test(e))).toBe(true);
  });

  it('returns ok: false on malformed YAML', () => {
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, 'not: valid: yaml: [');
    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(false);
    expect(result.errors.some((e) => /parse|yaml/i.test(e))).toBe(true);
  });

  it('returns ok: false on manifest with no files array (schema rejects)', () => {
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, `version: 0.1.21\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`);
    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(false);
    // Schema-level rejection — message format from ajv error mapping
    expect(result.errors.some((e) => /schema violation|files/i.test(e))).toBe(true);
  });

  it('finds artifacts in nested subdirectories (CI layout)', () => {
    const contents = Buffer.from('nested installer bytes');
    const subdir = path.join(dir, 'concord-voice-windows-x64-v0.1.21');
    mkdirSync(subdir);
    writeFileSync(path.join(subdir, 'Concord-Setup.exe'), contents);
    const manifest = `version: 0.1.21\nfiles:\n  - url: Concord-Setup.exe\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, manifest);
    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(true);
  });

  it('rejects ambiguous artifact names (multiple subdirs contain the same basename)', () => {
    const contents = Buffer.from('fake');
    for (const sub of ['a', 'b']) {
      const subdir = path.join(dir, sub);
      mkdirSync(subdir);
      writeFileSync(path.join(subdir, 'Dup-Setup.exe'), contents);
    }
    const manifest = `version: 0.1.21\nfiles:\n  - url: Dup-Setup.exe\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
    const manifestPath = path.join(dir, 'latest.yml');
    writeFileSync(manifestPath, manifest);
    const result = verifyManifest(manifestPath);
    expect(result.ok).toBe(false);
    expect(result.errors.some((e) => /ambiguous/.test(e))).toBe(true);
  });

  // Schema-validation regression tests (#920 §5.7): catch structural defects
  // before the hash-matching loop runs. The schema is shared with
  // generate-update-manifest.mts, so these regressions also protect the
  // generator from emitting malformed manifests.
  describe('schema validation', () => {
    it('rejects a manifest with an extra top-level property', () => {
      const contents = Buffer.from('bytes');
      writeFileSync(path.join(dir, 'A.exe'), contents);
      const manifest = `version: 0.1.21\nfiles:\n  - url: A.exe\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\nunexpected: oops\n`;
      const manifestPath = path.join(dir, 'latest.yml');
      writeFileSync(manifestPath, manifest);
      const result = verifyManifest(manifestPath);
      expect(result.ok).toBe(false);
      expect(result.errors.some((e) => /schema violation/.test(e))).toBe(true);
    });

    it('rejects a manifest missing version', () => {
      const contents = Buffer.from('bytes');
      writeFileSync(path.join(dir, 'A.exe'), contents);
      const manifest = `files:\n  - url: A.exe\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
      const manifestPath = path.join(dir, 'latest.yml');
      writeFileSync(manifestPath, manifest);
      const result = verifyManifest(manifestPath);
      expect(result.ok).toBe(false);
      expect(result.errors.some((e) => /schema violation|version/i.test(e))).toBe(true);
    });

    it('rejects a manifest with a non-semver version', () => {
      const contents = Buffer.from('bytes');
      writeFileSync(path.join(dir, 'A.exe'), contents);
      const manifest = `version: 0.1\nfiles:\n  - url: A.exe\n    sha512: ${sha512Base64(contents)}\n    size: ${contents.length}\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
      const manifestPath = path.join(dir, 'latest.yml');
      writeFileSync(manifestPath, manifest);
      const result = verifyManifest(manifestPath);
      expect(result.ok).toBe(false);
      expect(result.errors.some((e) => /schema violation/.test(e))).toBe(true);
    });

    it('rejects a manifest with a malformed sha512 (wrong length)', () => {
      const manifest = `version: 0.1.21\nfiles:\n  - url: A.exe\n    sha512: AAAA==\n    size: 10\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
      const manifestPath = path.join(dir, 'latest.yml');
      writeFileSync(manifestPath, manifest);
      const result = verifyManifest(manifestPath);
      expect(result.ok).toBe(false);
      expect(result.errors.some((e) => /schema violation/.test(e))).toBe(true);
    });

    it('rejects a manifest with an empty files array', () => {
      const manifest = `version: 0.1.21\nfiles: []\nreleaseDate: '2026-04-15T00:00:00.000Z'\n`;
      const manifestPath = path.join(dir, 'latest.yml');
      writeFileSync(manifestPath, manifest);
      const result = verifyManifest(manifestPath);
      expect(result.ok).toBe(false);
      expect(result.errors.some((e) => /schema violation/.test(e))).toBe(true);
    });
  });
});
