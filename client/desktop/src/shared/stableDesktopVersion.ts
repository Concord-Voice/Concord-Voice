const STABLE_DESKTOP_VERSION_RE = /^(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*)$/;
const PILOT_DESKTOP_VERSION_RE = /^((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.0)-pilot\.(0|[1-9]\d*)$/;
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

/** Accepts stable releases and the bounded X.Y.0-pilot.N admission declaration. */
export function isSupportedDesktopClientVersion(version: string): boolean {
  if (isStableDesktopVersion(version)) return true;
  if (version.length > MAX_STABLE_DESKTOP_VERSION_BYTES) return false;
  const match = PILOT_DESKTOP_VERSION_RE.exec(version);
  return (
    match?.[0] === version && isStableDesktopVersion(match[1]) && BigInt(match[2]) <= MAX_UINT64
  );
}
