// @vitest-environment node
/**
 * Signer ↔ verifier byte-parity guard for the signed SPA LKG cache (#1870).
 *
 * This is the producer/consumer cross-check (analogous to the age-claim A↔B
 * fixture): the PRODUCER is `client/desktop/scripts/sign-spa-manifest.mjs`
 * (CI/deploy), the CONSUMER is `src/main/spaCache/verifyManifest.ts` (Electron
 * main). The signature is detached over the RAW manifest bytes, so any drift in
 * serialization, algorithm parameters, or path/hash computation between the two
 * sides would surface here as a verification failure.
 *
 * Keys are EPHEMERAL (generated in-test) and never written to disk / committed.
 */

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { generateKeyPairSync } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  buildManifest,
  signManifestBytes,
  signSpaDist,
  SPA_MANIFEST_FILENAME,
  SPA_MANIFEST_SIG_FILENAME,
} from '../../../../scripts/sign-spa-manifest.mjs';
import { verifyManifest } from '@/main/spaCache/verifyManifest';

// Absolute path to the signer CLI, resolved relative to THIS test file (no cwd
// assumptions) so the subprocess fail-safe tests below work from any runner cwd.
const SIGNER_SCRIPT = fileURLToPath(
  new URL('../../../../scripts/sign-spa-manifest.mjs', import.meta.url)
);

const IPC_CONTRACT = 16; // stamped value (mirrors ipcContract.ts IPC_CONTRACT_VERSION)
const ENTRY_HTML = '<!doctype html><html><body><div id="root"></div></body></html>';
const ASSET_JS = 'console.log("concord spa bundle");\n';
const ASSET_CSS = ':root{--x:1}\n';

let distDir: string;
let privateKeyPem: string;
let publicKeyPem: string;

beforeEach(() => {
  // Ephemeral RSA-4096 keypair — never persisted.
  const { privateKey, publicKey } = generateKeyPairSync('rsa', {
    modulusLength: 4096,
    publicKeyEncoding: { type: 'spki', format: 'pem' },
    privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
  });
  privateKeyPem = privateKey as string;
  publicKeyPem = publicKey as string;

  // Temp dist resembling a Vite build: index.html at root + a couple assets.
  distDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-signer-crosscheck-'));
  fs.writeFileSync(path.join(distDir, 'index.html'), ENTRY_HTML);
  fs.mkdirSync(path.join(distDir, 'assets'), { recursive: true });
  fs.writeFileSync(path.join(distDir, 'assets', 'x-abc123.js'), ASSET_JS);
  fs.writeFileSync(path.join(distDir, 'assets', 'x-def456.css'), ASSET_CSS);
});

afterEach(() => {
  if (distDir) {
    fs.rmSync(distDir, { recursive: true, force: true });
  }
});

describe('signerVerifier crosscheck (#1870)', () => {
  it('verifyManifest accepts a manifest signed by sign-spa-manifest.mjs', async () => {
    const generatedAt = new Date().toISOString();
    await signSpaDist(distDir, privateKeyPem, {
      buildId: 'abc1234',
      spaIpcContract: IPC_CONTRACT,
      generatedAt,
    });

    const manifestBytes = fs.readFileSync(path.join(distDir, SPA_MANIFEST_FILENAME));
    const signatureBase64 = fs
      .readFileSync(path.join(distDir, SPA_MANIFEST_SIG_FILENAME), 'utf8')
      .trim();

    const manifest = JSON.parse(manifestBytes.toString('utf8'));
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem,
      shellIpcContract: IPC_CONTRACT,
      nowMs: Date.parse(manifest.generatedAt),
    });

    expect(result.ok).toBe(true);
    if (!result.ok) return; // narrow for TS

    // Manifest fields match the dist exactly.
    expect(result.manifest.schemaVersion).toBe(1);
    expect(result.manifest.buildId).toBe('abc1234');
    expect(result.manifest.spaIpcContract).toBe(IPC_CONTRACT);
    expect(result.manifest.entry.path).toBe('index.html');
    expect(result.manifest.entry.size).toBe(Buffer.byteLength(ENTRY_HTML));
    // Two assets, sorted by path (css sorts before js: "assets/x-d..." < "assets/x-a..."? no — 'a'<'d').
    expect(result.manifest.assets.map((a) => a.path)).toEqual([
      'assets/x-abc123.js',
      'assets/x-def456.css',
    ]);
    const expectedTotal =
      Buffer.byteLength(ENTRY_HTML) + Buffer.byteLength(ASSET_JS) + Buffer.byteLength(ASSET_CSS);
    expect(result.manifest.totalSize).toBe(expectedTotal);
  });

  it('buildManifest produces bytes that signManifestBytes signs to a verifiable signature', async () => {
    const generatedAt = new Date().toISOString();
    const { manifest, manifestBytes } = buildManifest(distDir, {
      buildId: 'build-99',
      spaIpcContract: IPC_CONTRACT,
      generatedAt,
    });
    const signatureBase64 = signManifestBytes(manifestBytes, privateKeyPem);

    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem,
      shellIpcContract: IPC_CONTRACT,
      nowMs: Date.parse(manifest.generatedAt),
    });

    expect(result.ok).toBe(true);
  });

  it('rejects a manifest whose bytes were tampered after signing', async () => {
    const generatedAt = new Date().toISOString();
    await signSpaDist(distDir, privateKeyPem, {
      buildId: 'abc1234',
      spaIpcContract: IPC_CONTRACT,
      generatedAt,
    });

    const manifestPath = path.join(distDir, SPA_MANIFEST_FILENAME);
    const original = fs.readFileSync(manifestPath);
    // Flip one byte in the middle of the manifest JSON.
    const tampered = Buffer.from(original);
    const flipIndex = Math.floor(tampered.length / 2);
    tampered[flipIndex] = tampered[flipIndex] ^ 0xff;
    fs.writeFileSync(manifestPath, tampered);

    const manifestBytes = fs.readFileSync(manifestPath);
    const signatureBase64 = fs
      .readFileSync(path.join(distDir, SPA_MANIFEST_SIG_FILENAME), 'utf8')
      .trim();

    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem,
      shellIpcContract: IPC_CONTRACT,
      nowMs: Date.parse(generatedAt),
    });

    expect(result.ok).toBe(false);
  });

  it('rejects when verified against the wrong public key', async () => {
    const generatedAt = new Date().toISOString();
    const { manifest, manifestBytes } = buildManifest(distDir, {
      buildId: 'abc1234',
      spaIpcContract: IPC_CONTRACT,
      generatedAt,
    });
    const signatureBase64 = signManifestBytes(manifestBytes, privateKeyPem);

    const { publicKey: otherPublicKey } = generateKeyPairSync('rsa', {
      modulusLength: 4096,
      publicKeyEncoding: { type: 'spki', format: 'pem' },
      privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
    });

    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: otherPublicKey as string,
      shellIpcContract: IPC_CONTRACT,
      nowMs: Date.parse(manifest.generatedAt),
    });

    expect(result.ok).toBe(false);
  });
});

// ── Fail-safe-skip (Finding E2) ─────────────────────────────────────────────
// The signer's no-key fail-safe lives in the CLI `main()` (not the exported
// helpers), so it is exercised by invoking the script as a subprocess. With
// SPA_MANIFEST_SIGNING_KEY unset/empty the script must exit 0 and write NEITHER
// the manifest NOR the signature (the SPA still deploys; the cache stays dormant
// — remote → bundled as before the feature). Paired with a present-key run that
// asserts BOTH files ARE written. Hermetic: temp dist, cleaned up; never commit
// a key.
describe('sign-spa-manifest.mjs CLI fail-safe (#1870 Finding E2)', () => {
  let cliDistDir: string;

  beforeEach(() => {
    cliDistDir = fs.mkdtempSync(path.join(os.tmpdir(), 'spa-signer-cli-'));
    fs.writeFileSync(path.join(cliDistDir, 'index.html'), ENTRY_HTML);
    fs.mkdirSync(path.join(cliDistDir, 'assets'), { recursive: true });
    fs.writeFileSync(path.join(cliDistDir, 'assets', 'x-abc123.js'), ASSET_JS);
  });

  afterEach(() => {
    if (cliDistDir) {
      fs.rmSync(cliDistDir, { recursive: true, force: true });
    }
  });

  function runSigner(env: Record<string, string | undefined>): number {
    try {
      execFileSync(
        process.execPath,
        [
          SIGNER_SCRIPT,
          '--dist',
          cliDistDir,
          '--build-id',
          'abc1234',
          '--ipc-contract',
          String(IPC_CONTRACT),
        ],
        { env: { ...process.env, ...env }, stdio: 'pipe' }
      );
      return 0;
    } catch (err) {
      // execFileSync throws on non-zero exit; surface the status.
      const status = (err as { status?: number }).status;
      return typeof status === 'number' ? status : 1;
    }
  }

  it('exits 0 and writes NEITHER file when SPA_MANIFEST_SIGNING_KEY is empty', () => {
    const exitCode = runSigner({ SPA_MANIFEST_SIGNING_KEY: '' });
    expect(exitCode).toBe(0);
    expect(fs.existsSync(path.join(cliDistDir, SPA_MANIFEST_FILENAME))).toBe(false);
    expect(fs.existsSync(path.join(cliDistDir, SPA_MANIFEST_SIG_FILENAME))).toBe(false);
  });

  it('exits 0 and writes NEITHER file when SPA_MANIFEST_SIGNING_KEY is unset', () => {
    // Explicitly remove the var (subprocess inherits process.env otherwise).
    const exitCode = runSigner({ SPA_MANIFEST_SIGNING_KEY: undefined });
    expect(exitCode).toBe(0);
    expect(fs.existsSync(path.join(cliDistDir, SPA_MANIFEST_FILENAME))).toBe(false);
    expect(fs.existsSync(path.join(cliDistDir, SPA_MANIFEST_SIG_FILENAME))).toBe(false);
  });

  it('writes BOTH files when a valid signing key IS present', () => {
    // Ephemeral key minted in-test, passed via env only — never written to disk.
    const { privateKey } = generateKeyPairSync('rsa', {
      modulusLength: 4096,
      publicKeyEncoding: { type: 'spki', format: 'pem' },
      privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
    });
    const exitCode = runSigner({ SPA_MANIFEST_SIGNING_KEY: privateKey as string });
    expect(exitCode).toBe(0);
    expect(fs.existsSync(path.join(cliDistDir, SPA_MANIFEST_FILENAME))).toBe(true);
    expect(fs.existsSync(path.join(cliDistDir, SPA_MANIFEST_SIG_FILENAME))).toBe(true);
  });
});
