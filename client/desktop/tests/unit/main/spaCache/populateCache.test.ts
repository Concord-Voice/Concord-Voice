import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {
  buildSignedManifest,
  makeKeypair,
  sha256Hex,
  signBytes,
  type EphemeralKeypair,
} from './manifestTestUtils';
import { SPA_CACHE_MAX_FILE_BYTES } from '@/main/spaCache/manifestSchema';

const KP: EphemeralKeypair = makeKeypair();
let userDataDir = '';

// vi.hoisted so the mock fn exists before the hoisted vi.mock factory runs.
const { fetchMock } = vi.hoisted(() => ({ fetchMock: vi.fn() }));

vi.mock('electron', () => ({
  app: {
    getPath: vi.fn((name: string) => {
      if (name === 'userData') return userDataDir;
      return os.tmpdir();
    }),
  },
  net: { fetch: fetchMock },
}));

vi.mock('@/main/spaCache/spaManifestPublicKey', () => ({
  get SPA_MANIFEST_PUBLIC_KEY_PEM() {
    return KP.publicKeyPem;
  },
  isSpaManifestKeyConfigured: () => true,
}));

vi.mock('@/main/ipcContract', () => ({ IPC_CONTRACT_VERSION: 16 }));

import { populateCacheFromRemote, __resetInFlightForTests } from '@/main/spaCache/populateCache';
import { getLiveDir, readLiveManifest } from '@/main/spaCache/cacheStore';
import { SPA_MANIFEST_FILENAME, SPA_MANIFEST_SIG_FILENAME } from '@/main/spaCache/manifestSchema';

const BASE = 'https://spa.concordvoice.chat/';

/** A response stub that honors arrayBuffer(). */
function okResponse(bytes: Buffer): Response {
  return {
    ok: true,
    status: 200,
    arrayBuffer: vi
      .fn()
      .mockResolvedValue(bytes.buffer.slice(bytes.byteOffset, bytes.byteOffset + bytes.byteLength)),
  } as unknown as Response;
}

function notFoundResponse(): Response {
  return {
    ok: false,
    status: 404,
    arrayBuffer: vi.fn().mockResolvedValue(new ArrayBuffer(0)),
  } as unknown as Response;
}

/**
 * Wire the net.fetch mock to a URL→bytes map. Any URL not in the map resolves to
 * a 404 response so missing-file paths are exercised honestly.
 */
function routeFetch(map: Record<string, Buffer>): void {
  fetchMock.mockImplementation((url: string) => {
    const bytes = map[url];
    if (bytes) return Promise.resolve(okResponse(bytes));
    return Promise.resolve(notFoundResponse());
  });
}

interface ScenarioFiles {
  entry: { path: string; bytes: Buffer };
  assets: { path: string; bytes: Buffer }[];
}

function defaultFiles(): ScenarioFiles {
  return {
    entry: { path: 'index.html', bytes: Buffer.from('<!doctype html><html>app</html>') },
    assets: [
      { path: 'assets/app-abc.js', bytes: Buffer.from('console.log(1)') },
      { path: 'assets/style-xyz.css', bytes: Buffer.from('body{}') },
    ],
  };
}

beforeEach(() => {
  userDataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-cache-populate-'));
  fetchMock.mockReset();
  __resetInFlightForTests();
});

afterEach(() => {
  if (userDataDir && fs.existsSync(userDataDir)) {
    fs.rmSync(userDataDir, { recursive: true, force: true });
  }
});

describe('populateCacheFromRemote (#1870)', () => {
  it('promotes when every file verifies; live manifest is readable after', async () => {
    const files = defaultFiles();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, files);
    routeFetch({
      [new URL(SPA_MANIFEST_FILENAME, BASE).href]: manifestBytes,
      [new URL(SPA_MANIFEST_SIG_FILENAME, BASE).href]: Buffer.from(signatureBase64, 'utf8'),
      [new URL(files.entry.path, BASE).href]: files.entry.bytes,
      [new URL(files.assets[0].path, BASE).href]: files.assets[0].bytes,
      [new URL(files.assets[1].path, BASE).href]: files.assets[1].bytes,
    });

    const result = await populateCacheFromRemote(BASE);
    expect(result.populated).toBe(true);

    const live = readLiveManifest();
    expect(live).not.toBeNull();
    expect(live?.manifestBytes.equals(manifestBytes)).toBe(true);
    expect(fs.existsSync(path.join(getLiveDir(), 'index.html'))).toBe(true);
    expect(fs.existsSync(path.join(getLiveDir(), 'assets', 'app-abc.js'))).toBe(true);
  });

  it('does NOT promote when a single asset hash mismatches; live is unchanged', async () => {
    const files = defaultFiles();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, files);
    // Serve corrupted bytes for one asset — its sha256 will not match.
    routeFetch({
      [new URL(SPA_MANIFEST_FILENAME, BASE).href]: manifestBytes,
      [new URL(SPA_MANIFEST_SIG_FILENAME, BASE).href]: Buffer.from(signatureBase64, 'utf8'),
      [new URL(files.entry.path, BASE).href]: files.entry.bytes,
      [new URL(files.assets[0].path, BASE).href]: Buffer.from('CORRUPTED'),
      [new URL(files.assets[1].path, BASE).href]: files.assets[1].bytes,
    });

    const result = await populateCacheFromRemote(BASE);
    expect(result.populated).toBe(false);
    expect(result.reason).toMatch(/sha256 mismatch/);
    // Live cache was never created (no prior promote).
    expect(readLiveManifest()).toBeNull();
  });

  it('does NOT promote when an asset exceeds the per-file size cap', async () => {
    const oversizedBytes = Buffer.alloc(SPA_CACHE_MAX_FILE_BYTES + 1, 0x41);
    // The manifest schema caps the DECLARED size at SPA_CACHE_MAX_FILE_BYTES, so
    // a manifest declaring an oversize file would be rejected at verify time.
    // To exercise the SERVED-bytes per-file cap, sign a manifest with a small
    // declared size but serve oversize bytes — fetchBytes now bounds the read and
    // rejects (response exceeds cap) before the bytes are staged (Gitar fix).
    const entryBytes = Buffer.from('small');
    const obj = {
      schemaVersion: 1,
      buildId: 'test-build-1',
      spaIpcContract: 1,
      generatedAt: new Date().toISOString(),
      entry: { path: 'index.html', sha256: sha256Hex(entryBytes), size: entryBytes.length },
      assets: [
        {
          path: 'assets/big.bin',
          sha256: sha256Hex(oversizedBytes),
          // Declared size within the schema cap, but served bytes are oversize.
          size: SPA_CACHE_MAX_FILE_BYTES,
        },
      ],
      totalSize: entryBytes.length + SPA_CACHE_MAX_FILE_BYTES,
    };
    const manifestBytes = Buffer.from(JSON.stringify(obj), 'utf8');
    const signatureBase64 = signBytes(manifestBytes, KP.privateKeyPem);
    routeFetch({
      [new URL(SPA_MANIFEST_FILENAME, BASE).href]: manifestBytes,
      [new URL(SPA_MANIFEST_SIG_FILENAME, BASE).href]: Buffer.from(signatureBase64, 'utf8'),
      [new URL('index.html', BASE).href]: entryBytes,
      [new URL('assets/big.bin', BASE).href]: oversizedBytes,
    });

    const result = await populateCacheFromRemote(BASE);
    expect(result.populated).toBe(false);
    expect(result.reason).toMatch(/exceeds .*bytes/);
    expect(readLiveManifest()).toBeNull();
  });

  it('does NOT fetch assets when the manifest signature is invalid', async () => {
    const files = defaultFiles();
    const { manifestBytes } = buildSignedManifest(KP, files);
    // Serve a garbage signature.
    routeFetch({
      [new URL(SPA_MANIFEST_FILENAME, BASE).href]: manifestBytes,
      [new URL(SPA_MANIFEST_SIG_FILENAME, BASE).href]: Buffer.from('bm90LWEtc2ln', 'utf8'),
      [new URL(files.entry.path, BASE).href]: files.entry.bytes,
    });

    const result = await populateCacheFromRemote(BASE);
    expect(result.populated).toBe(false);
    expect(result.reason).toMatch(/manifest rejected/);
    expect(readLiveManifest()).toBeNull();

    // Only the manifest + sig were fetched — no asset fetch occurred.
    const fetchedUrls = fetchMock.mock.calls.map((c) => c[0] as string);
    expect(fetchedUrls).toContain(new URL(SPA_MANIFEST_FILENAME, BASE).href);
    expect(fetchedUrls).not.toContain(new URL(files.entry.path, BASE).href);
  });

  it('does NOT promote when an asset fetch fails (404)', async () => {
    const files = defaultFiles();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, files);
    // Omit one asset from the route map → 404.
    routeFetch({
      [new URL(SPA_MANIFEST_FILENAME, BASE).href]: manifestBytes,
      [new URL(SPA_MANIFEST_SIG_FILENAME, BASE).href]: Buffer.from(signatureBase64, 'utf8'),
      [new URL(files.entry.path, BASE).href]: files.entry.bytes,
      [new URL(files.assets[0].path, BASE).href]: files.assets[0].bytes,
      // assets[1] intentionally absent → 404.
    });

    const result = await populateCacheFromRemote(BASE);
    expect(result.populated).toBe(false);
    expect(result.reason).toMatch(/asset fetch failed/);
    expect(readLiveManifest()).toBeNull();
  });

  it('returns populated=false (not throw) when the manifest itself 404s', async () => {
    routeFetch({}); // everything 404s
    const result = await populateCacheFromRemote(BASE);
    expect(result.populated).toBe(false);
    expect(result.reason).toMatch(/manifest fetch failed/);
  });

  it('is single-flight: concurrent calls share one in-flight promise', async () => {
    const files = defaultFiles();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(KP, files);
    routeFetch({
      [new URL(SPA_MANIFEST_FILENAME, BASE).href]: manifestBytes,
      [new URL(SPA_MANIFEST_SIG_FILENAME, BASE).href]: Buffer.from(signatureBase64, 'utf8'),
      [new URL(files.entry.path, BASE).href]: files.entry.bytes,
      [new URL(files.assets[0].path, BASE).href]: files.assets[0].bytes,
      [new URL(files.assets[1].path, BASE).href]: files.assets[1].bytes,
    });

    const p1 = populateCacheFromRemote(BASE);
    const p2 = populateCacheFromRemote(BASE);
    expect(p1).toBe(p2); // same promise instance — deduped.
    const [r1, r2] = await Promise.all([p1, p2]);
    expect(r1.populated).toBe(true);
    expect(r2.populated).toBe(true);

    // The manifest was fetched once, not twice (single-flight).
    const manifestFetches = fetchMock.mock.calls.filter(
      (c) => c[0] === new URL(SPA_MANIFEST_FILENAME, BASE).href
    );
    expect(manifestFetches).toHaveLength(1);
  });
});
