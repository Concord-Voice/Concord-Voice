/**
 * SPA LKG cache — on-disk asset integrity (#1870, Finding A).
 *
 * Binds the BYTES on disk to the signature-verified manifest. The manifest
 * signature (verifyManifest.ts) proves the manifest contents are authentic; it
 * does NOT prove that the files promoted into `live/` still match the hashes the
 * manifest enumerates. A local-FS-write attacker can overwrite a promoted asset
 * while leaving `spa-manifest.json` + its `.sig` intact — so the serve path must
 * re-read each file and verify (size + sha256) against the signed manifest.
 *
 * Pure integrity hashing only — no signature work (that is verifyManifest.ts)
 * and no path/scheme handling (that is cacheProtocol.ts). The file read itself
 * is fd-bounded + TOCTOU-safe via cacheStore.readLiveFile.
 */

import { createHash } from 'node:crypto';
import { readLiveFile } from './cacheStore';
import type { SpaManifest, SpaManifestFile } from './manifestSchema';

/**
 * Read a single live file named by a signed-manifest entry and verify it matches
 * (size via the fd-bounded read in readLiveFile, then sha256). Returns the
 * verified bytes, or null on any mismatch (missing / wrong-size / hash-mismatch /
 * not a regular file). The sha256 compare is a plain string equality — this is
 * an integrity hash, not a secret, so constant-time comparison is not required.
 */
export function readVerifiedLiveFile(entry: SpaManifestFile): Buffer | null {
  const bytes = readLiveFile(entry.path, entry.size);
  if (bytes === null) {
    return null;
  }
  const actualSha = createHash('sha256').update(bytes).digest('hex');
  if (actualSha !== entry.sha256) {
    return null;
  }
  return bytes;
}

/**
 * Verify that EVERY file the signed manifest enumerates (entry + all assets) is
 * present on disk and matches its manifest size + sha256. Returns true only when
 * all files verify; a single missing / tampered / wrong-size file yields false.
 *
 * This is the decision-time gate consumed by resolveCachedSpa: if it returns
 * false, the cache is not trustworthy and the caller falls back to bundled.
 */
export function verifyAllLiveAssets(manifest: SpaManifest): boolean {
  if (readVerifiedLiveFile(manifest.entry) === null) {
    return false;
  }
  for (const asset of manifest.assets) {
    if (readVerifiedLiveFile(asset) === null) {
      return false;
    }
  }
  return true;
}
