#!/usr/bin/env node
/**
 * sign-spa-manifest.mjs — SPA last-known-good (LKG) cache PRODUCER (#1870).
 *
 * Walks the built renderer dist, builds a `spa-manifest.json` describing every
 * file (sha256 + size), and writes a DETACHED RSA-PSS signature over the EXACT
 * bytes of that manifest to a sibling `spa-manifest.json.sig`. The Electron
 * verifier (`src/main/spaCache/verifyManifest.ts`) checks the signature over the
 * bytes it fetched VERBATIM before parsing the JSON, so there is no
 * canonicalization to drift between this signer and the verifier.
 *
 * The field shape + algorithm parameters are the contract declared in
 * `src/main/spaCache/manifestSchema.ts` (the single source of truth). This file
 * intentionally re-declares the small constants it needs rather than importing
 * the TypeScript module — it runs as plain Node ESM in the deploy path, with no
 * build step and no third-party deps (Node built-ins only).
 *
 * FAIL-SAFE: the CLI skips signing (exit 0) when `SPA_MANIFEST_SIGNING_KEY` is
 * unset/empty — the SPA still deploys and the client cache stays dormant
 * (remote → bundled exactly as before this feature shipped). The private key is
 * NEVER logged.
 *
 * Spec: [internal]specs — "Signed LKG Cache"; runbook:
 * [internal]spa-manifest-signing.md.
 */

import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { constants as cryptoConstants, createHash, sign as cryptoSign } from 'node:crypto';
import { pathToFileURL } from 'node:url';

// ── Contract constants (mirror src/main/spaCache/manifestSchema.ts) ────────
const SPA_MANIFEST_SCHEMA_VERSION = 1;
const SPA_MANIFEST_FILENAME = 'spa-manifest.json';
const SPA_MANIFEST_SIG_FILENAME = 'spa-manifest.json.sig';
const SPA_MANIFEST_SIGN_ALGORITHM = 'sha256';
const SPA_MANIFEST_SIGN_SALT_LENGTH = 32;
/** The dist-root entry HTML treated as the manifest `entry` (all else = assets). */
const ENTRY_FILENAME = 'index.html';

const LOG_PREFIX = '[sign-spa-manifest]';

/** sha256 of a file's bytes, lowercase hex (matches the verifier's SHA256_HEX). */
function sha256HexOfFile(filePath) {
  return createHash('sha256').update(readFileSync(filePath)).digest('hex');
}

/**
 * Recursively walk `distDir` and return every file as a relative POSIX path
 * from distDir. Symlinks are followed by statSync; the published Vite dist is a
 * plain file tree, so this is the complete asset graph.
 */
function walkFiles(distDir) {
  const out = [];
  function walk(absDir) {
    for (const entry of readdirSync(absDir)) {
      const abs = path.join(absDir, entry);
      const st = statSync(abs);
      if (st.isDirectory()) {
        walk(abs);
      } else if (st.isFile()) {
        // Relative POSIX path from distDir (forward slashes on every platform).
        const rel = path.relative(distDir, abs).split(path.sep).join('/');
        out.push(rel);
      }
    }
  }
  walk(distDir);
  return out;
}

/**
 * Build the manifest object + the exact bytes that will be signed and written.
 * The dist-root `index.html` is the `entry`; every other file is an asset.
 * Assets are sorted by path for deterministic output. The signature is over
 * `manifestBytes`, so any stable serialization is acceptable — we pick
 * 2-space-pretty JSON + trailing newline and stick to it.
 *
 * @returns {{ manifest: object, manifestBytes: Buffer }}
 */
export function buildManifest(distDir, { buildId, spaIpcContract, generatedAt }) {
  const resolvedDist = path.resolve(distDir);
  const entryAbs = path.join(resolvedDist, ENTRY_FILENAME);
  let entryStat;
  try {
    entryStat = statSync(entryAbs);
  } catch {
    throw new Error(`${ENTRY_FILENAME} not found at dist root: ${resolvedDist}`);
  }
  if (!entryStat.isFile()) {
    throw new Error(`${ENTRY_FILENAME} at dist root is not a regular file: ${resolvedDist}`);
  }

  const entry = {
    path: ENTRY_FILENAME,
    sha256: sha256HexOfFile(entryAbs),
    size: entryStat.size,
  };

  const assets = walkFiles(resolvedDist)
    .filter((rel) => rel !== ENTRY_FILENAME)
    .sort((a, b) => (a < b ? -1 : a > b ? 1 : 0))
    .map((rel) => {
      const abs = path.join(resolvedDist, rel);
      return { path: rel, sha256: sha256HexOfFile(abs), size: statSync(abs).size };
    });

  const totalSize = entry.size + assets.reduce((acc, a) => acc + a.size, 0);

  const manifest = {
    schemaVersion: SPA_MANIFEST_SCHEMA_VERSION,
    buildId,
    spaIpcContract,
    generatedAt,
    entry,
    assets,
    totalSize,
  };

  const manifestBytes = Buffer.from(`${JSON.stringify(manifest, null, 2)}\n`, 'utf8');
  return { manifest, manifestBytes };
}

/**
 * Sign the exact manifest bytes with RSA-PSS / SHA-256 / saltLength 32 and
 * return the signature as base64. Mirrors `crypto.verify(...)` in
 * verifyManifest.ts exactly.
 */
export function signManifestBytes(manifestBytes, privateKeyPem) {
  const signature = cryptoSign(SPA_MANIFEST_SIGN_ALGORITHM, manifestBytes, {
    key: privateKeyPem,
    padding: cryptoConstants.RSA_PKCS1_PSS_PADDING,
    saltLength: SPA_MANIFEST_SIGN_SALT_LENGTH,
  });
  return signature.toString('base64');
}

/**
 * Build + sign + write `spa-manifest.json` (the signed bytes) and
 * `spa-manifest.json.sig` (base64 signature) into distDir.
 *
 * @returns {{ manifestPath: string, signaturePath: string, manifest: object }}
 */
export async function signSpaDist(distDir, privateKeyPem, opts) {
  const { buildId, spaIpcContract, generatedAt } = opts;
  const { manifest, manifestBytes } = buildManifest(distDir, {
    buildId,
    spaIpcContract,
    generatedAt,
  });
  const signatureBase64 = signManifestBytes(manifestBytes, privateKeyPem);

  const resolvedDist = path.resolve(distDir);
  const manifestPath = path.join(resolvedDist, SPA_MANIFEST_FILENAME);
  const signaturePath = path.join(resolvedDist, SPA_MANIFEST_SIG_FILENAME);

  // Write the EXACT bytes that were signed (no re-serialization).
  writeFileSync(manifestPath, manifestBytes);
  writeFileSync(signaturePath, `${signatureBase64}\n`, 'utf8');

  return { manifestPath, signaturePath, manifest };
}

// ── CLI ────────────────────────────────────────────────────────────────────
function parseArgs(argv) {
  const opts = { dist: '', buildId: '', ipcContract: '' };
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    if (arg === '--dist') opts.dist = argv[(i += 1)] ?? '';
    else if (arg.startsWith('--dist=')) opts.dist = arg.slice('--dist='.length);
    else if (arg === '--build-id') opts.buildId = argv[(i += 1)] ?? '';
    else if (arg.startsWith('--build-id=')) opts.buildId = arg.slice('--build-id='.length);
    else if (arg === '--ipc-contract') opts.ipcContract = argv[(i += 1)] ?? '';
    else if (arg.startsWith('--ipc-contract='))
      opts.ipcContract = arg.slice('--ipc-contract='.length);
    else {
      console.error(`${LOG_PREFIX} unknown arg '${arg}'`);
      process.exit(2);
    }
  }
  return opts;
}

async function main() {
  const { dist, buildId, ipcContract } = parseArgs(process.argv.slice(2));

  if (!dist || !buildId || !ipcContract) {
    console.error(
      `${LOG_PREFIX} usage: sign-spa-manifest.mjs --dist <dir> --build-id <id> --ipc-contract <n>`
    );
    process.exit(2);
  }

  const spaIpcContract = Number.parseInt(ipcContract, 10);
  if (!Number.isInteger(spaIpcContract) || spaIpcContract <= 0) {
    console.error(`${LOG_PREFIX} --ipc-contract must be a positive integer, got '${ipcContract}'`);
    process.exit(2);
  }

  // FAIL-SAFE: no key ⇒ skip signing, exit 0. The SPA still deploys; the client
  // cache stays dormant (remote → bundled as before).
  const privateKeyPem = process.env.SPA_MANIFEST_SIGNING_KEY ?? '';
  if (privateKeyPem.trim().length === 0) {
    console.error(
      `${LOG_PREFIX} no SPA_MANIFEST_SIGNING_KEY; skipping manifest signing (cache stays dormant)`
    );
    process.exit(0);
  }

  const generatedAt = new Date().toISOString();
  const { manifestPath, signaturePath, manifest } = await signSpaDist(dist, privateKeyPem, {
    buildId,
    spaIpcContract,
    generatedAt,
  });

  // NEVER log the key or its bytes. Log only the non-secret manifest summary.
  console.log(
    `${LOG_PREFIX} signed SPA manifest: buildId=${manifest.buildId} ipc=${manifest.spaIpcContract} ` +
      `assets=${manifest.assets.length} totalSize=${manifest.totalSize}`
  );
  console.log(`${LOG_PREFIX} wrote ${manifestPath}`);
  console.log(`${LOG_PREFIX} wrote ${signaturePath}`);
}

/* istanbul ignore next -- thin CLI entry shim; the exported core is unit-tested. */
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((err) => {
    console.error(`${LOG_PREFIX} ERROR: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(1);
  });
}

// Re-export the filename constants so callers/tests can assert against them.
export { SPA_MANIFEST_FILENAME, SPA_MANIFEST_SIG_FILENAME };
