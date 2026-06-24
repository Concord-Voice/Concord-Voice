/**
 * SPA LKG cache — opportunistic population from the live remote SPA (#1870).
 *
 * After a SUCCESSFUL remote SPA load, main.ts fires this best-effort to refresh
 * the signed last-known-good cache. It is:
 *
 *   - Best-effort: NEVER throws into the caller; every failure path returns a
 *     `{ populated: false, reason }` result instead.
 *   - Single-flight: a module-scope in-flight promise dedupes concurrent calls
 *     (e.g., a launch populate racing a manual reload populate).
 *   - Fail-closed on trust: the manifest signature is verified BEFORE any asset
 *     is fetched; every asset is size-bounded and sha256-checked against the
 *     signed manifest; staging is promoted to live ONLY after ALL files verify.
 *     Any mismatch / oversize / fetch failure aborts WITHOUT promoting, so the
 *     previous live cache (if any) is left untouched.
 *
 * Logging is PII/secret-safe per [internal]rules/observability.md — never the raw
 * `err`, never file bytes, never the manifest payload.
 */

import { net } from 'electron';
import { createHash } from 'node:crypto';
import { verifyManifest } from './verifyManifest';
import { promoteStagingToLive, resetStaging, writeStagedFile } from './cacheStore';
import { SPA_MANIFEST_PUBLIC_KEY_PEM, isSpaManifestKeyConfigured } from './spaManifestPublicKey';
import { IPC_CONTRACT_VERSION } from '../ipcContract';
import {
  SPA_CACHE_MAX_FILE_BYTES,
  SPA_CACHE_MAX_TOTAL_BYTES,
  SPA_MANIFEST_FILENAME,
  SPA_MANIFEST_MAX_BYTES,
  SPA_MANIFEST_SIG_FILENAME,
  type SpaManifest,
  type SpaManifestFile,
} from './manifestSchema';

export interface PopulateResult {
  populated: boolean;
  reason: string;
}

const NO_CACHE_HEADERS = {
  'Cache-Control': 'no-cache',
  Pragma: 'no-cache',
} as const;

/** Single-flight guard: concurrent callers share the same in-flight promise. */
let inFlight: Promise<PopulateResult> | null = null;

/**
 * Refresh the LKG cache from the live remote SPA. `remoteBaseUrl` is the origin
 * root the SPA was loaded from (e.g. `https://spa.concordvoice.chat/`). Returns
 * a result; never throws.
 */
export function populateCacheFromRemote(remoteBaseUrl: string): Promise<PopulateResult> {
  if (inFlight) {
    return inFlight;
  }
  inFlight = doPopulate(remoteBaseUrl).finally(() => {
    inFlight = null;
  });
  return inFlight;
}

/**
 * Read a ReadableStream body into a Buffer, bounded to `maxBytes`: throws as soon
 * as the running total exceeds the cap, so an oversized body is never fully
 * materialized into memory. Extracted from fetchBytes to keep that function's
 * cognitive complexity within bounds (S3776).
 */
async function readBoundedStream(
  body: ReadableStream<Uint8Array>,
  maxBytes: number
): Promise<Buffer> {
  const reader = body.getReader();
  const chunks: Buffer[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      if (value) {
        total += value.byteLength;
        if (total > maxBytes) {
          throw new Error(`response exceeds ${maxBytes} bytes`);
        }
        chunks.push(Buffer.from(value));
      }
    }
  } finally {
    reader.releaseLock();
  }
  return Buffer.concat(chunks, total);
}

/**
 * Fetch a URL and return its bytes, bounded to `maxBytes`. The cap is enforced
 * WHILE reading (streamed via readBoundedStream), not after buffering the whole
 * response — so an oversized body (e.g. a compromised CDN) is aborted mid-stream
 * instead of being fully materialized into main-process memory first (Gitar
 * review, #1880). An advertised over-cap Content-Length is rejected before any
 * read. Falls back to a bounded arrayBuffer() read when the response exposes no
 * ReadableStream (e.g. non-streaming test stubs).
 */
async function fetchBytes(url: string, maxBytes: number): Promise<Buffer> {
  const response = await net.fetch(url, { headers: NO_CACHE_HEADERS, cache: 'no-store' });
  if (!response.ok) {
    throw new Error(`fetch returned ${response.status}`);
  }
  // Honest-server fast reject: refuse an over-cap response by its advertised
  // length before reading a single byte.
  const declaredLen = Number(response.headers?.get?.('content-length'));
  if (Number.isFinite(declaredLen) && declaredLen > maxBytes) {
    throw new Error(`response exceeds ${maxBytes} bytes (content-length ${declaredLen})`);
  }
  const body = response.body;
  if (body && typeof body.getReader === 'function') {
    return readBoundedStream(body, maxBytes);
  }
  // Fallback: response has no readable stream — buffer, then bound.
  const buf = Buffer.from(await response.arrayBuffer());
  if (buf.length > maxBytes) {
    throw new Error(`response exceeds ${maxBytes} bytes`);
  }
  return buf;
}

async function doPopulate(remoteBaseUrl: string): Promise<PopulateResult> {
  // 0. Fast-skip if no key is configured — verifyManifest would reject anyway,
  //    but skipping avoids a pointless fetch round-trip when the cache is
  //    dormant (placeholder public key).
  if (!isSpaManifestKeyConfigured()) {
    return { populated: false, reason: 'no verification key configured (cache disabled)' };
  }

  // 1. Fetch the manifest + detached signature.
  let manifestBytes: Buffer;
  let signatureBase64: string;
  try {
    const manifestUrl = new URL(SPA_MANIFEST_FILENAME, remoteBaseUrl).href;
    const sigUrl = new URL(SPA_MANIFEST_SIG_FILENAME, remoteBaseUrl).href;
    manifestBytes = await fetchBytes(manifestUrl, SPA_MANIFEST_MAX_BYTES);
    if (manifestBytes.length > SPA_MANIFEST_MAX_BYTES) {
      return { populated: false, reason: 'manifest exceeds size cap' };
    }
    // The signature is tiny (base64 RSA-4096 ~ 700 bytes); bound it generously
    // with the manifest cap rather than add a separate constant.
    const sigBuf = await fetchBytes(sigUrl, SPA_MANIFEST_MAX_BYTES);
    signatureBase64 = sigBuf.toString('utf8').trim();
  } catch (err) {
    return { populated: false, reason: `manifest fetch failed: ${(err as Error).message}` };
  }

  // 2. Verify the manifest BEFORE fetching any asset bytes.
  const verified = verifyManifest({
    manifestBytes,
    signatureBase64,
    publicKeyPem: SPA_MANIFEST_PUBLIC_KEY_PEM,
    shellIpcContract: IPC_CONTRACT_VERSION,
    nowMs: Date.now(),
  });
  if (!verified.ok) {
    return { populated: false, reason: `manifest rejected: ${verified.reason}` };
  }
  const manifest = verified.manifest;

  // 3. Download + verify every file into a fresh staging tree.
  try {
    resetStaging();
    const downloaded = await downloadAndStageAll(manifest, remoteBaseUrl);
    if (!downloaded.ok) {
      // Staging stays (never promoted); previous live cache is untouched.
      return { populated: false, reason: downloaded.reason };
    }
    // Persist the manifest + signature so readLiveManifest works post-promote
    // and the serve-time gate can re-verify the cache.
    writeStagedFile(SPA_MANIFEST_FILENAME, manifestBytes);
    writeStagedFile(SPA_MANIFEST_SIG_FILENAME, Buffer.from(signatureBase64, 'utf8'));
  } catch (err) {
    return { populated: false, reason: `staging failed: ${(err as Error).message}` };
  }

  // 4. All files verified — atomically promote.
  try {
    promoteStagingToLive();
  } catch (err) {
    return { populated: false, reason: `promote failed: ${(err as Error).message}` };
  }
  return { populated: true, reason: 'ok' };
}

interface DownloadOutcome {
  ok: boolean;
  reason: string;
}

async function downloadAndStageAll(
  manifest: SpaManifest,
  remoteBaseUrl: string
): Promise<DownloadOutcome> {
  const files: SpaManifestFile[] = [manifest.entry, ...manifest.assets];
  let runningTotal = 0;

  for (const file of files) {
    let bytes: Buffer;
    try {
      const fileUrl = new URL(file.path, remoteBaseUrl).href;
      bytes = await fetchBytes(fileUrl, SPA_CACHE_MAX_FILE_BYTES);
    } catch (err) {
      return { ok: false, reason: `asset fetch failed: ${(err as Error).message}` };
    }

    // Per-file cap (defense in depth — the manifest schema already bounds size,
    // but the SERVED bytes are what we cap; a server returning more than the
    // manifest declared is a mismatch we reject).
    if (bytes.length > SPA_CACHE_MAX_FILE_BYTES) {
      return { ok: false, reason: 'asset exceeds per-file size cap' };
    }
    runningTotal += bytes.length;
    if (runningTotal > SPA_CACHE_MAX_TOTAL_BYTES) {
      return { ok: false, reason: 'cache exceeds total size cap' };
    }

    // Integrity: the served bytes MUST hash to the signed manifest's value.
    // Straight compare is fine — this is an integrity hash, not a secret.
    const actualSha = createHash('sha256').update(bytes).digest('hex');
    if (actualSha !== file.sha256) {
      return { ok: false, reason: 'asset sha256 mismatch' };
    }
    // The declared size must match the served size too (the signed manifest's
    // totalSize sum was already validated against the declared sizes).
    if (bytes.length !== file.size) {
      return { ok: false, reason: 'asset size does not match manifest' };
    }

    try {
      writeStagedFile(file.path, bytes);
    } catch (err) {
      return { ok: false, reason: `staged write failed: ${(err as Error).message}` };
    }
  }

  return { ok: true, reason: 'ok' };
}

/** Test-only: reset the single-flight guard between cases. */
export function __resetInFlightForTests(): void {
  inFlight = null;
}
