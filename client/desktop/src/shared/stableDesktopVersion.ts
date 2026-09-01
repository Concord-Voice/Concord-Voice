const STABLE_DESKTOP_VERSION_RE = /^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/;
const MAX_STABLE_DESKTOP_VERSION_BYTES = 32;
const MAX_UINT64 = (1n << 64n) - 1n;

/** Matches the control-plane's stable X.Y.Z client-version contract. */
export function isStableDesktopVersion(version: string): boolean {
  const match = STABLE_DESKTOP_VERSION_RE.exec(version);
  if (version.length > MAX_STABLE_DESKTOP_VERSION_BYTES || match?.[0] !== version) {
    return false;
  }
  return version.split('.').every((component) => BigInt(component) <= MAX_UINT64);
}
