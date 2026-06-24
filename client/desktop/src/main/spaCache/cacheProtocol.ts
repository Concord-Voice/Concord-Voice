/**
 * SPA LKG cache — `spa-cache://concord/*` protocol resolver + handler (#1870).
 *
 * Serves the VERIFIED last-known-good SPA cache from a DEDICATED privileged
 * scheme, distinct from `app://concord` (bundled) and never generic `file://`
 * (per the #1870 trust-limits). The pure resolver MIRRORS appProtocol.ts: it
 * validates the host, rejects path-traversal both in the raw (un-normalized)
 * URL and after `path.resolve`, decodes percent-encoding, and confirms the
 * resolved path stays inside `liveRoot`.
 *
 * Serve-time trust (Finding A2 — the bytes-to-manifest re-bind): the on-disk
 * tree lives in userData and a local-FS-write attacker can overwrite a promoted
 * asset while leaving `spa-manifest.json` + its `.sig` intact. So EVERY request
 * re-binds the served bytes to the signature-verified manifest:
 *   1. obtain the signature-verified manifest (memoized by on-disk-bytes hash to
 *      avoid an RSA verify per subresource) and build a path → file map;
 *   2. path-safety resolve the request URL (resolveCachePath);
 *   3. require the manifest-relative path to be ENUMERATED in the signed
 *      manifest (serve only listed files);
 *   4. fd-bounded read + size/sha256 verify the file (readVerifiedLiveFile);
 *   5. serve the VERIFIED bytes directly — NOT via net.fetch(file://...), which
 *      would re-read from disk (reopening a decision→serve TOCTOU window and
 *      following symlinks to an unverified target).
 * Any failure fails closed: 404 (no trustworthy manifest / path not enumerated)
 * or 403 (file present but tampered / wrong-size / not a regular file).
 */

import path from 'node:path';
import { createHash } from 'node:crypto';
import { readLiveManifest } from './cacheStore';
import { verifyManifest } from './verifyManifest';
import { readVerifiedLiveFile } from './cacheIntegrity';
import { SPA_MANIFEST_PUBLIC_KEY_PEM } from './spaManifestPublicKey';
import { IPC_CONTRACT_VERSION } from '../ipcContract';
import { SPA_CACHE_HOST, type SpaManifest, type SpaManifestFile } from './manifestSchema';

export type CachePathResolveResult =
  | { ok: true; absolutePath: string }
  | { ok: false; status: 403 | 404 };

/**
 * Pure URL → absolute-path resolver for the cache scheme. Returns the resolved
 * path inside `liveRoot`, or a 403/404 rejection. Performs no I/O.
 */
export function resolveCachePath(requestUrl: string, liveRoot: string): CachePathResolveResult {
  let url: URL;
  try {
    url = new URL(requestUrl);
  } catch {
    return { ok: false, status: 404 };
  }

  if (url.host !== SPA_CACHE_HOST) {
    return { ok: false, status: 404 };
  }

  // Defense in depth: scan the RAW request path for `..` segments before the
  // WHATWG parser silently normalizes them away. Case-insensitive prefix slice
  // (the scheme is normalized to lowercase but the host case is preserved for
  // non-special schemes) so a mixed-case input still reaches the raw scan.
  const schemeAndHost = `${url.protocol}//${url.host}`;
  const rawPath = requestUrl.toLowerCase().startsWith(schemeAndHost.toLowerCase())
    ? requestUrl.slice(schemeAndHost.length)
    : '';
  let decodedRaw: string;
  try {
    decodedRaw = decodeURIComponent(rawPath);
  } catch {
    return { ok: false, status: 403 };
  }
  if (decodedRaw.split(/[/\\]/).includes('..')) {
    return { ok: false, status: 403 };
  }

  const requestedPath = url.pathname === '/' || url.pathname === '' ? '/index.html' : url.pathname;

  let decodedPath: string;
  try {
    decodedPath = decodeURIComponent(requestedPath);
  } catch {
    return { ok: false, status: 403 };
  }

  const absolutePath = path.resolve(liveRoot, '.' + decodedPath);
  const isInsideLive = absolutePath.startsWith(liveRoot + path.sep) || absolutePath === liveRoot;
  if (!isInsideLive) {
    return { ok: false, status: 403 };
  }

  return { ok: true, absolutePath };
}

// ── Memoized signature-verified manifest ───────────────────────────────────
// An RSA-PSS verify per subresource request would be wasteful (a page pulls many
// JS/CSS/font assets). Memoize the parsed manifest + path map keyed by the
// sha256 of the on-disk manifest bytes: each call reads the (small) manifest
// bytes and hashes them (both cheap); only a key change re-runs verifyManifest
// (RSA). The memo holds nothing trustworthy on its own — it is recomputed
// whenever the on-disk manifest bytes change, and invalidated (returns null)
// whenever the manifest is absent or fails verification.
interface MemoEntry {
  manifestHash: string;
  fileMap: Map<string, SpaManifestFile>;
}
let memo: MemoEntry | null = null;

function buildFileMap(manifest: SpaManifest): Map<string, SpaManifestFile> {
  const map = new Map<string, SpaManifestFile>();
  map.set(manifest.entry.path, manifest.entry);
  for (const asset of manifest.assets) {
    map.set(asset.path, asset);
  }
  return map;
}

/**
 * Return the file map of the signature-verified live manifest, or null when no
 * trustworthy manifest exists. Cheap fast-path: if the on-disk manifest bytes
 * hash to the memoized key, reuse the parsed map; otherwise run the full
 * verification once and re-cache.
 */
function getVerifiedManifestFileMap(): Map<string, SpaManifestFile> | null {
  const live = readLiveManifest();
  if (!live) {
    memo = null;
    return null;
  }
  const manifestHash = createHash('sha256').update(live.manifestBytes).digest('hex');
  if (memo && memo.manifestHash === manifestHash) {
    return memo.fileMap;
  }
  const verified = verifyManifest({
    manifestBytes: live.manifestBytes,
    signatureBase64: live.signatureBase64,
    publicKeyPem: SPA_MANIFEST_PUBLIC_KEY_PEM,
    shellIpcContract: IPC_CONTRACT_VERSION,
    nowMs: Date.now(),
  });
  if (!verified.ok) {
    memo = null;
    return null;
  }
  const fileMap = buildFileMap(verified.manifest);
  memo = { manifestHash, fileMap };
  return fileMap;
}

// ── Extension → MIME map (served bytes carry the declared content-type) ─────
// Text/script/style assets carry an explicit `; charset=utf-8` — Vite emits
// UTF-8, and an explicit charset avoids browser encoding heuristics (Gitar
// review, #1880). Binary types (images, fonts, wasm) take no charset.
const MIME_BY_EXT: Readonly<Record<string, string>> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml; charset=utf-8',
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.gif': 'image/gif',
  '.webp': 'image/webp',
  '.woff2': 'font/woff2',
  '.woff': 'font/woff',
  '.ttf': 'font/ttf',
  '.ico': 'image/x-icon',
  '.map': 'application/json; charset=utf-8',
  '.wasm': 'application/wasm',
  '.txt': 'text/plain; charset=utf-8',
};

function contentTypeFor(relPath: string): string {
  const ext = path.extname(relPath).toLowerCase();
  return MIME_BY_EXT[ext] ?? 'application/octet-stream';
}

/**
 * `protocol.handle('spa-cache', ...)` body. Re-binds the served bytes to the
 * signature-verified manifest on EVERY request (see the file header). The live
 * root is resolved lazily (via the injected getter) so the pinned userData path
 * is read at request time, not module-load time. Returns 404 (no trustworthy
 * manifest / path not enumerated) or 403 (file tampered / wrong-size) on
 * rejection; otherwise serves the verified bytes with a by-extension
 * content-type and `no-store` (the renderer must never cache an un-reverified
 * copy).
 */
export async function handleCacheProtocolRequest(
  request: Request,
  getLiveRoot: () => string
): Promise<Response> {
  // 1. Trustworthy manifest (memoized) or nothing to serve.
  const fileMap = getVerifiedManifestFileMap();
  if (!fileMap) {
    return new Response(null, { status: 404 });
  }

  // 2. Path-safety resolve (keeps the existing traversal/escape rejection).
  const liveRoot = getLiveRoot();
  const result = resolveCachePath(request.url, liveRoot);
  if (!result.ok) {
    return new Response(null, { status: result.status });
  }

  // 3. Manifest-relative path (forward-slash normalized) MUST be enumerated in
  //    the signed manifest — serve only listed files.
  const relPath = path.relative(liveRoot, result.absolutePath).split(path.sep).join('/');
  const entry = fileMap.get(relPath);
  if (!entry) {
    return new Response(null, { status: 404 });
  }

  // 4. Read + verify (size + sha256) against the signed manifest entry.
  const bytes = readVerifiedLiveFile(entry);
  if (bytes === null) {
    return new Response(null, { status: 403 });
  }

  // 5. Serve the VERIFIED bytes directly — never net.fetch(file://...), which
  //    would re-read from disk and follow symlinks past this verification.
  return new Response(new Uint8Array(bytes), {
    headers: {
      'content-type': contentTypeFor(relPath),
      'cache-control': 'no-store',
    },
  });
}

/** Test-only: reset the verified-manifest memo between cases. */
export function __resetCacheProtocolMemoForTests(): void {
  memo = null;
}
