// @vitest-environment node
import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { mkdtempSync, writeFileSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { parse as parseYaml } from 'yaml';
import {
  buildManifest,
  renderManifestYaml,
  validateManifest,
  ManifestSchemaError,
  sha512Base64,
  type Manifest,
} from './generate-update-manifest.mts';

function makeFile(dir: string, name: string, contents: Buffer): string {
  const p = path.join(dir, name);
  writeFileSync(p, contents);
  return p;
}

function realSha512(buf: Buffer): string {
  return createHash('sha512').update(buf).digest('base64');
}

const FROZEN_DATE = '2026-05-11T12:34:56.000Z';
const PLAUSIBLE_SHA512 = `${'A'.repeat(86)}==`;

let dir: string;

beforeEach(() => {
  dir = mkdtempSync(path.join(tmpdir(), 'gen-manifest-'));
});

afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe('sha512Base64', () => {
  it('matches Node crypto base64 SHA-512 of the file bytes', () => {
    const buf = Buffer.from('hello world');
    const file = makeFile(dir, 'a.bin', buf);
    expect(sha512Base64(file)).toBe(realSha512(buf));
  });
});

describe('validateManifest — schema enforcement (Draft 2020-12)', () => {
  // Helper to construct a baseline valid manifest. Tests mutate copies.
  function valid(): Manifest {
    return {
      version: '0.1.36',
      files: [{ url: 'concord-0.1.36-x64.exe', sha512: PLAUSIBLE_SHA512, size: 12345 }],
      releaseDate: FROZEN_DATE,
    };
  }

  it('accepts a well-formed manifest', () => {
    expect(() => validateManifest(valid())).not.toThrow();
  });

  it('rejects missing version', () => {
    const m = valid();
    // @ts-expect-error — testing schema rejection of malformed input
    delete m.version;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects non-semver version', () => {
    const m = { ...valid(), version: '0.1' };
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects sha512 that does not match the base64-88 pattern', () => {
    const m = valid();
    m.files[0].sha512 = 'tooShort=='; // not 88 chars
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects empty files array', () => {
    const m = { ...valid(), files: [] };
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects extra top-level property (additionalProperties: false)', () => {
    const m = { ...valid(), unexpected: 'oops' } as unknown;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects extra property inside a files entry', () => {
    const m = valid();
    (m.files[0] as unknown as Record<string, unknown>).extra = 'oops';
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('accepts a files entry with a valid integer blockMapSize (#1292)', () => {
    const m = valid();
    m.files[0].blockMapSize = 140_000;
    expect(() => validateManifest(m)).not.toThrow();
  });

  it('rejects blockMapSize of zero (minimum: 1)', () => {
    const m = valid();
    m.files[0].blockMapSize = 0;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects non-integer blockMapSize', () => {
    const m = valid();
    m.files[0].blockMapSize = 1.5;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('still rejects an unknown extra property when blockMapSize is allowed (additionalProperties intact)', () => {
    const m = valid();
    (m.files[0] as unknown as Record<string, unknown>).bogus = 'oops';
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects zero or negative size', () => {
    const m = valid();
    m.files[0].size = 0;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects non-ISO releaseDate', () => {
    const m = { ...valid(), releaseDate: 'yesterday' };
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects pre-release version suffix (e.g. 0.1.36-rc.1)', () => {
    const m = { ...valid(), version: '0.1.36-rc.1' };
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects v-prefixed version (e.g. v0.1.36)', () => {
    const m = { ...valid(), version: 'v0.1.36' };
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects sha512 longer than 88 chars (length boundary)', () => {
    const m = valid();
    m.files[0].sha512 = `${'A'.repeat(88)}==`;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects sha512 with URL-safe base64 characters (-, _)', () => {
    const m = valid();
    m.files[0].sha512 = `${'A'.repeat(85)}_==`;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });

  it('rejects sha512 with single = padding (must be exactly ==)', () => {
    const m = valid();
    m.files[0].sha512 = `${'A'.repeat(87)}=`;
    expect(() => validateManifest(m)).toThrow(ManifestSchemaError);
  });
});

describe('buildManifest — end-to-end manifest assembly', () => {
  it('produces a schema-valid manifest from a Windows .exe fixture', () => {
    const buf = Buffer.from('windows installer bytes');
    const file = makeFile(dir, 'ConcordVoice-0.1.36-windows-x64-Setup.exe', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    expect(m.version).toBe('0.1.36');
    expect(m.files).toHaveLength(1);
    expect(m.files[0].url).toBe('ConcordVoice-0.1.36-windows-x64-Setup.exe');
    expect(m.files[0].sha512).toBe(realSha512(buf));
    expect(m.files[0].size).toBe(buf.length);
    expect(m.releaseDate).toBe(FROZEN_DATE);
  });

  it('produces a schema-valid manifest from a macOS .zip fixture', () => {
    const buf = Buffer.from('macos zip bytes — different');
    const file = makeFile(dir, 'ConcordVoice-0.1.36-macos-arm64.zip', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    expect(m.files[0].url).toBe('ConcordVoice-0.1.36-macos-arm64.zip');
    expect(m.files[0].sha512).toBe(realSha512(buf));
  });

  it('produces a schema-valid manifest from a Linux .AppImage fixture', () => {
    const buf = Buffer.from('linux appimage bytes');
    const file = makeFile(dir, 'ConcordVoice-0.1.36-linux-x64.AppImage', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    expect(m.files[0].url).toBe('ConcordVoice-0.1.36-linux-x64.AppImage');
    expect(m.files[0].sha512).toBe(realSha512(buf));
  });

  it('handles multiple files in a single manifest', () => {
    const a = makeFile(dir, 'A.zip', Buffer.from('a'));
    const b = makeFile(dir, 'B.zip', Buffer.from('bb'));
    const m = buildManifest({
      version: '0.1.36',
      files: [a, b],
      releaseDate: FROZEN_DATE,
    });
    expect(m.files).toHaveLength(2);
    expect(m.files.map((f) => f.url)).toEqual(['A.zip', 'B.zip']);
  });

  it('defaults releaseDate to current ISO timestamp when unspecified', () => {
    const buf = Buffer.from('x');
    const file = makeFile(dir, 'X.zip', buf);
    const before = new Date().toISOString();
    const m = buildManifest({ version: '0.1.36', files: [file] });
    const after = new Date().toISOString();
    expect(m.releaseDate >= before && m.releaseDate <= after).toBe(true);
  });

  it('throws ManifestSchemaError if the schema rejects (defense-in-depth)', () => {
    const file = makeFile(dir, 'X.zip', Buffer.from('x'));
    expect(() =>
      buildManifest({ version: 'not-semver', files: [file], releaseDate: FROZEN_DATE })
    ).toThrow(ManifestSchemaError);
  });

  it('adds blockMapSize when a sibling .blockmap exists (macOS path) (#1292)', () => {
    const zip = makeFile(
      dir,
      'ConcordVoice-0.1.59-macos-arm64.zip',
      Buffer.from('macos zip bytes')
    );
    const blockMap = Buffer.from('gzipped-blockmap-stub-bytes');
    makeFile(dir, 'ConcordVoice-0.1.59-macos-arm64.zip.blockmap', blockMap);
    const m = buildManifest({ version: '0.1.59', files: [zip], releaseDate: FROZEN_DATE });
    expect(m.files[0].blockMapSize).toBe(blockMap.length);
  });

  it('omits blockMapSize when no sibling .blockmap exists (Windows/Linux path) (#1292)', () => {
    const file = makeFile(
      dir,
      'ConcordVoice-0.1.59-windows-x64-Setup.exe',
      Buffer.from('exe bytes')
    );
    const m = buildManifest({ version: '0.1.59', files: [file], releaseDate: FROZEN_DATE });
    expect(m.files[0].blockMapSize).toBeUndefined();
  });

  it('renders blockMapSize into the YAML and round-trips (#1292)', () => {
    const zip = makeFile(dir, 'ConcordVoice-0.1.59-macos-arm64.zip', Buffer.from('z'));
    makeFile(dir, 'ConcordVoice-0.1.59-macos-arm64.zip.blockmap', Buffer.from('bm'));
    const m = buildManifest({ version: '0.1.59', files: [zip], releaseDate: FROZEN_DATE });
    const parsed = parseYaml(renderManifestYaml(m)) as Manifest;
    expect(parsed.files[0].blockMapSize).toBe(2);
  });

  it('throws ManifestSchemaError when a sibling .blockmap is empty (0 bytes — spec §8 fail-loud) (#1292)', () => {
    // Exercises the generator's STAT-DERIVED path (existsSync → statSync → assign →
    // validate), distinct from the object-injection schema-reject tests above: a
    // truncated/empty sidecar yields blockMapSize: 0, which the schema (minimum: 1)
    // rejects — fail-loud rather than shipping a malformed manifest.
    const zip = makeFile(
      dir,
      'ConcordVoice-0.1.59-macos-arm64.zip',
      Buffer.from('macos zip bytes')
    );
    makeFile(dir, 'ConcordVoice-0.1.59-macos-arm64.zip.blockmap', Buffer.alloc(0));
    expect(() =>
      buildManifest({ version: '0.1.59', files: [zip], releaseDate: FROZEN_DATE })
    ).toThrow(ManifestSchemaError);
  });
});

describe('renderManifestYaml — output shape', () => {
  it('produces YAML that round-trips back to the same manifest', () => {
    const buf = Buffer.from('y');
    const file = makeFile(dir, 'Y.zip', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    const yaml = renderManifestYaml(m);
    const parsed = parseYaml(yaml) as Manifest;
    expect(parsed).toEqual(m);
  });

  it('does not emit any unknown top-level keys', () => {
    const buf = Buffer.from('z');
    const file = makeFile(dir, 'Z.zip', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    const parsed = parseYaml(renderManifestYaml(m)) as Record<string, unknown>;
    expect(Object.keys(parsed).sort()).toEqual(['files', 'releaseDate', 'version']);
  });
});

describe('schema file — concord:update-manifest', () => {
  it('is loadable as JSON from schemas/update-manifest.json', () => {
    const schemaPath = path.resolve(__dirname, '../schemas/update-manifest.json');
    const schema = JSON.parse(readFileSync(schemaPath, 'utf-8'));
    expect(schema.$id).toBe('concord:update-manifest');
    expect(schema.$schema).toBe('https://json-schema.org/draft/2020-12/schema');
    expect(schema.required).toEqual(['version', 'files', 'releaseDate']);
  });
});

// End-to-end integration: the generator writes a YAML manifest to disk, and
// the (separately-implemented) verify-update-manifest.mjs reads it back and
// confirms hash + schema. This is the central contract of #920 §5.7 — the
// shared schema asserts that both ends agree on shape.
describe('generator ↔ verifier round-trip', () => {
  it('produces a YAML that the verifier accepts as ok', async () => {
    const { verifyManifest } = await import('./verify-update-manifest.mjs');
    const buf = Buffer.from('round-trip installer bytes');
    const file = makeFile(dir, 'Concord-0.1.36-x64-Setup.exe', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    const yamlPath = path.join(dir, 'latest.yml');
    writeFileSync(yamlPath, renderManifestYaml(m));

    const result = verifyManifest(yamlPath);
    expect(result.ok).toBe(true);
    expect(result.errors).toEqual([]);
  });

  it('verifier rejects a YAML where the hash was tampered post-generation', async () => {
    const { verifyManifest } = await import('./verify-update-manifest.mjs');
    const buf = Buffer.from('tampering test bytes');
    const file = makeFile(dir, 'Concord-0.1.36-x64-Setup.exe', buf);
    const m = buildManifest({
      version: '0.1.36',
      files: [file],
      releaseDate: FROZEN_DATE,
    });
    // Replace the real sha512 with a plausible-but-wrong one (still schema-valid).
    m.files[0].sha512 = `${'A'.repeat(86)}==`;
    const yamlPath = path.join(dir, 'latest.yml');
    writeFileSync(yamlPath, renderManifestYaml(m));

    const result = verifyManifest(yamlPath);
    expect(result.ok).toBe(false);
    expect(result.errors.some((e: string) => /sha512 mismatch/.test(e))).toBe(true);
  });
});
