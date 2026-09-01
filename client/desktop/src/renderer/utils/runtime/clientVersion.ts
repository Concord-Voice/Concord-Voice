import { isStableDesktopVersion } from '../../../shared/stableDesktopVersion';

export { isStableDesktopVersion } from '../../../shared/stableDesktopVersion';

const HIDDEN_SPA_VERSIONS = new Set(['', 'bundled', 'remote']);
const FULL_SHA_RE = /^[0-9a-f]{40}$/i;
const HTML_SHA_RE = /^sha256:([0-9a-f]{64})$/i;

let desktopClientDisplayVersionPromise: Promise<string | null> | null = null;

export function compareStableDesktopVersions(a: string, b: string): number | null {
  if (!isStableDesktopVersion(a) || !isStableDesktopVersion(b)) return null;
  const left = a.split('.').map(BigInt);
  const right = b.split('.').map(BigInt);
  for (let index = 0; index < left.length; index += 1) {
    if (left[index] < right[index]) return -1;
    if (left[index] > right[index]) return 1;
  }
  return 0;
}

/**
 * Returns the raw desktop version for display and legacy config advisory only.
 * Never use this for declarations or trust decisions; use getDesktopClientVersion instead.
 */
export function getDesktopClientDisplayVersion(): Promise<string | null> {
  if (desktopClientDisplayVersionPromise !== null) return desktopClientDisplayVersionPromise;
  const getVersion = globalThis.electron?.getVersion;
  if (!getVersion) return Promise.resolve(null);

  const lookup = getVersion()
    .then((version) => (typeof version === 'string' ? version : null))
    .catch(() => null);
  desktopClientDisplayVersionPromise = lookup;
  return lookup;
}

export function getDesktopClientVersion(): Promise<string | null> {
  return getDesktopClientDisplayVersion().then((version) =>
    version !== null && isStableDesktopVersion(version) ? version : null
  );
}

/** Reset the cached bridge lookup for tests. */
export function _resetClientVersionCache(): void {
  desktopClientDisplayVersionPromise = null;
}

export function compactSpaHash(hash: string | null | undefined): string | null {
  if (hash == null) return null;
  const normalized = hash.trim();
  if (HIDDEN_SPA_VERSIONS.has(normalized)) return null;
  const htmlHash = HTML_SHA_RE.exec(normalized);
  if (htmlHash) return htmlHash[1].slice(0, 7);
  return FULL_SHA_RE.test(normalized) ? normalized.slice(0, 7) : normalized;
}

export function formatClientVersion(
  appVersion: string | null | undefined,
  spaHash?: string | null
): string {
  if (!appVersion) return '';
  const compactHash = compactSpaHash(spaHash);
  return compactHash ? `v${appVersion}-${compactHash}` : `v${appVersion}`;
}
