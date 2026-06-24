/**
 * SPA LKG cache — serve-time resolution (#1870).
 *
 * Decides whether a verified last-known-good cache may serve the renderer. This
 * is the SERVE-time gate (distinct from populateCache's WRITE-time gate): the
 * on-disk cache lives in userData and could be tampered with between launches,
 * so we re-run, every time, BOTH layers of verification before returning a URL:
 *   1. the manifest's RSA-PSS signature + schema + staleness + IPC-contract
 *      checks (verifyManifest), proving the manifest contents are authentic; and
 *   2. an on-disk byte re-verification of EVERY file the signed manifest
 *      enumerates (entry + all assets) against its size + sha256
 *      (verifyAllLiveAssets), proving the promoted files still match the
 *      manifest — a manifest+sig left intact while an asset is overwritten on
 *      disk is the fail-open this gate closes.
 *
 * Returns the privileged cache URL only when the manifest verifies AND every
 * enumerated file matches its signed hash; a tampered (manifest OR any file),
 * stale, IPC-incompatible, or absent cache yields null and the caller falls
 * through to the bundled `app://concord` path.
 */

import { readLiveManifest } from './cacheStore';
import { verifyManifest } from './verifyManifest';
import { verifyAllLiveAssets } from './cacheIntegrity';
import { SPA_CACHE_HOST, SPA_CACHE_SCHEME } from './manifestSchema';
import { SPA_MANIFEST_PUBLIC_KEY_PEM } from './spaManifestPublicKey';
import { IPC_CONTRACT_VERSION } from '../ipcContract';

export interface CachedSpaDecision {
  url: string;
}

/**
 * Resolve a servable cache URL, or null. `nowMs` is injected for deterministic
 * tests (staleness check); it defaults to the wall clock.
 */
export function resolveCachedSpa(nowMs: number = Date.now()): CachedSpaDecision | null {
  const live = readLiveManifest();
  if (!live) {
    return null;
  }

  const verified = verifyManifest({
    manifestBytes: live.manifestBytes,
    signatureBase64: live.signatureBase64,
    publicKeyPem: SPA_MANIFEST_PUBLIC_KEY_PEM,
    shellIpcContract: IPC_CONTRACT_VERSION,
    nowMs,
  });
  if (!verified.ok) {
    // A tampered / stale / IPC-incompatible / unsigned cache is ignored.
    return null;
  }

  // Bind the on-disk bytes to the now-verified manifest: every enumerated file
  // must still match its signed size + sha256. A missing / overwritten / wrong-
  // size file (manifest+sig intact) fails here and we fall back to bundled.
  if (!verifyAllLiveAssets(verified.manifest)) {
    return null;
  }

  return { url: `${SPA_CACHE_SCHEME}://${SPA_CACHE_HOST}/index.html` };
}
