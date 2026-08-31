const HIDDEN_SPA_VERSIONS = new Set(['', 'bundled', 'remote']);
const FULL_SHA_RE = /^[0-9a-f]{40}$/i;
const HTML_SHA_RE = /^sha256:([0-9a-f]{64})$/i;

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
