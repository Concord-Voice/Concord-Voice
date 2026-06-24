/**
 * SPA LKG cache — on-disk store + atomic promotion (#1870).
 *
 * Filesystem layout (all under `app.getPath('userData')/spa-cache/`):
 *   live/     — the verified cache currently served by the `spa-cache://` handler
 *   staging/  — the download target for an in-progress populate; promoted to
 *               live/ ONLY after every file has been hash-verified against a
 *               signature-verified manifest.
 *
 * Promotion is atomic: the old `live/` is removed and `staging/` is renamed onto
 * it. `rename(2)` is atomic on the same filesystem, and staging is a sibling of
 * live under the same userData dir, so the rename never crosses a mount point.
 *
 * This module performs filesystem I/O only — no network, no crypto. The trust
 * decisions (signature, hashes, size caps) live in verifyManifest.ts and
 * populateCache.ts. Path-safety on writes mirrors appProtocol.ts so a malicious
 * manifest path can never escape the staging tree (defense in depth — the
 * manifest schema also rejects traversal paths before populate ever runs).
 */

import { app } from 'electron';
import fs from 'node:fs';
import path from 'node:path';
import {
  SPA_MANIFEST_FILENAME,
  SPA_MANIFEST_MAX_BYTES,
  SPA_MANIFEST_SIG_FILENAME,
} from './manifestSchema';

/** Root of the SPA cache tree under the pinned userData dir. */
function getCacheRoot(): string {
  return path.join(app.getPath('userData'), 'spa-cache');
}

/** The directory served by the `spa-cache://` protocol handler. */
export function getLiveDir(): string {
  return path.join(getCacheRoot(), 'live');
}

/** The download target for an in-progress populate. */
export function getStagingDir(): string {
  return path.join(getCacheRoot(), 'staging');
}

export interface LiveManifestBytes {
  manifestBytes: Buffer;
  signatureBase64: string;
}

/**
 * Read the live manifest + detached signature, or null if either is absent or
 * unreadable. The manifest read is bounded by the schema's max-bytes cap (DoS
 * guard) — a manifest larger than the cap is treated as no cache. The signature
 * is read as UTF-8 text (base64) and trimmed.
 *
 * Never throws: any fs error (missing dir, permission, oversize) yields null so
 * the caller cleanly falls back to remote/bundled.
 */
export function readLiveManifest(): LiveManifestBytes | null {
  const liveDir = getLiveDir();
  const manifestPath = path.join(liveDir, SPA_MANIFEST_FILENAME);
  const sigPath = path.join(liveDir, SPA_MANIFEST_SIG_FILENAME);
  try {
    // Open ONCE and stat + read through the SAME file descriptor so the size
    // check and the read operate on one inode — no path-based time-of-check /
    // time-of-use race (CodeQL js/file-system-race). fstat on the fd genuinely
    // bounds the allocation (read only after confirming size <= cap), unlike a
    // path stat followed by readFileSync (which reads the whole file regardless).
    const fd = fs.openSync(manifestPath, 'r');
    try {
      const st = fs.fstatSync(fd);
      if (!st.isFile() || st.size > SPA_MANIFEST_MAX_BYTES) {
        return null;
      }
      const manifestBytes = Buffer.alloc(st.size);
      let offset = 0;
      while (offset < st.size) {
        const read = fs.readSync(fd, manifestBytes, offset, st.size - offset, offset);
        if (read === 0) break;
        offset += read;
      }
      if (offset !== st.size) {
        return null; // short read — treat as no usable cache
      }
      // Signature is tiny (RSA-4096 base64 ~ 700 bytes); a bare read has no
      // preceding check, so no race. Missing/empty → no usable cache.
      const signatureBase64 = fs.readFileSync(sigPath, 'utf8').trim();
      if (signatureBase64.length === 0) {
        return null;
      }
      return { manifestBytes, signatureBase64 };
    } finally {
      fs.closeSync(fd);
    }
  } catch {
    // Missing files / permission errors / not-a-file → no usable cache.
    return null;
  }
}

/**
 * Atomically promote the staged cache to live: remove the old live tree, then
 * rename staging → live. Creates the cache root if needed. The caller is
 * responsible for having fully populated + verified staging first; this function
 * performs no validation.
 */
export function promoteStagingToLive(): void {
  const liveDir = getLiveDir();
  const stagingDir = getStagingDir();
  fs.mkdirSync(getCacheRoot(), { recursive: true });
  fs.rmSync(liveDir, { recursive: true, force: true });
  fs.renameSync(stagingDir, liveDir);
}

/**
 * Reset the staging directory to a fresh empty tree (rm then mkdir). Called at
 * the start of every populate so a previous aborted populate's partial bytes
 * are never reused.
 */
export function resetStaging(): void {
  const stagingDir = getStagingDir();
  fs.rmSync(stagingDir, { recursive: true, force: true });
  fs.mkdirSync(stagingDir, { recursive: true });
}

/**
 * Resolve a manifest-relative path safely inside the staging dir. Mirrors
 * appProtocol.ts: reject absolute paths, drive letters, backslashes, and any
 * `..` traversal segment, then confirm the resolved absolute path is inside the
 * staging root. Throws on any rejection — the caller treats that as a populate
 * abort (never promote).
 */
function resolveStagedPath(relPath: string): string {
  if (relPath.length === 0) {
    throw new Error('staged path is empty');
  }
  if (relPath.startsWith('/') || /^[a-zA-Z]:/.test(relPath) || relPath.includes('\\')) {
    throw new Error('staged path must be relative (no leading slash, drive letter, or backslash)');
  }
  if (relPath.split('/').some((seg) => seg === '..' || seg === '.' || seg === '')) {
    throw new Error('staged path must not contain traversal, empty, or dot segments');
  }
  const stagingDir = getStagingDir();
  const absolutePath = path.resolve(stagingDir, relPath);
  const insideStaging =
    absolutePath.startsWith(stagingDir + path.sep) || absolutePath === stagingDir;
  if (!insideStaging) {
    throw new Error('staged path escapes the staging directory');
  }
  return absolutePath;
}

/**
 * Write `bytes` to `relPath` under the staging directory, creating intermediate
 * directories. Path-safety is enforced via resolveStagedPath (throws on any
 * traversal / absolute / drive-letter input). Because the staging tree is
 * created fresh by resetStaging, there are no pre-existing symlinks to follow.
 */
export function writeStagedFile(relPath: string, bytes: Buffer): void {
  const absolutePath = resolveStagedPath(relPath);
  fs.mkdirSync(path.dirname(absolutePath), { recursive: true });
  fs.writeFileSync(absolutePath, bytes);
}

/**
 * Resolve a manifest-relative path safely inside the LIVE dir. Mirrors
 * resolveStagedPath's path-safety (reject empty / leading-slash / drive-letter /
 * backslash / `..`/`.`/empty segments / escape-liveDir) but rooted at
 * getLiveDir(). Returns the absolute path, or null on any rejection — callers in
 * the serve path treat null as "reject" (403/404), so this does NOT throw.
 */
export function resolveLivePath(relPath: string): string | null {
  if (relPath.length === 0) {
    return null;
  }
  if (relPath.startsWith('/') || /^[a-zA-Z]:/.test(relPath) || relPath.includes('\\')) {
    return null;
  }
  if (relPath.split('/').some((seg) => seg === '..' || seg === '.' || seg === '')) {
    return null;
  }
  const liveDir = getLiveDir();
  const absolutePath = path.resolve(liveDir, relPath);
  const insideLive = absolutePath.startsWith(liveDir + path.sep) || absolutePath === liveDir;
  if (!insideLive) {
    return null;
  }
  return absolutePath;
}

/**
 * Read a live-tree file by manifest-relative path, fd-bounded and TOCTOU-safe.
 *
 * The size check and the read operate on the SAME file descriptor (fstat on the
 * fd, then readSync from that fd) so there is no path-based time-of-check /
 * time-of-use race (CodeQL js/file-system-race) — mirrors readLiveManifest. The
 * fd-based `isFile()` check also rejects directories and device files, and the
 * `st.size !== expectedSize` check rejects any wrong-size replacement (e.g. a
 * symlink to a differently-sized out-of-tree target) BEFORE allocating/reading.
 *
 * Returns the file bytes, or null on any rejection (resolve failure, missing
 * file, not a regular file, size mismatch, short read). Never throws — callers
 * treat null as "reject". No crypto here (the hash check lives in
 * cacheIntegrity.ts).
 */
export function readLiveFile(relPath: string, expectedSize: number): Buffer | null {
  const absolutePath = resolveLivePath(relPath);
  if (absolutePath === null) {
    return null;
  }
  try {
    const fd = fs.openSync(absolutePath, 'r');
    try {
      const st = fs.fstatSync(fd);
      if (!st.isFile() || st.size !== expectedSize) {
        return null;
      }
      const bytes = Buffer.alloc(st.size);
      let offset = 0;
      while (offset < st.size) {
        const read = fs.readSync(fd, bytes, offset, st.size - offset, offset);
        if (read === 0) break;
        offset += read;
      }
      if (offset !== st.size) {
        return null; // short read — treat as unreadable
      }
      return bytes;
    } finally {
      fs.closeSync(fd);
    }
  } catch {
    // Missing file / permission error / not-a-file → reject.
    return null;
  }
}
