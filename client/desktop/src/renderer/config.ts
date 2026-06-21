// Server configuration — resolved from Vite env vars at build time.
// Local dev: defaults to localhost (no .env needed)
// LAN/staging: set VITE_SERVER_HOST in .env or .env.staging (e.g., "192.0.2.10")
const SERVER_HOST = import.meta.env.VITE_SERVER_HOST || 'localhost';
const API_PORT = import.meta.env.VITE_API_PORT || '8080';
const USE_TLS = import.meta.env.VITE_USE_TLS === 'true';

const protocol = USE_TLS ? 'https' : 'http';
const wsProtocol = USE_TLS ? 'wss' : 'ws';

// Omit port when it's the default for the protocol (443 for TLS, 80 for plain)
const isDefaultPort = (USE_TLS && API_PORT === '443') || (!USE_TLS && API_PORT === '80');
const portSuffix = isDefaultPort ? '' : `:${API_PORT}`;

export const API_BASE = `${protocol}://${SERVER_HOST}${portSuffix}`;
export const WS_BASE = `${wsProtocol}://${SERVER_HOST}${portSuffix}`;

// SPA build identifier shown in Settings → About (display-only).
//
// Source of truth (post-#976 / ADR-0015): the build stamps VITE_SPA_VERSION =
// github.sha (main-cd.yml). Cloudflare Pages serves the SPA FLAT at the origin
// root with NO `/spa/<sha>/` path, so the URL no longer encodes the version —
// the build-time env var is authoritative for the flat host.
//
// Runtime fallbacks (reached only when VITE_SPA_VERSION is unset — the #547
// drift case):
//   1. Legacy per-SHA host (pre-#976): the version is in the `/spa/<sha>/` path.
//   2. Otherwise classify by origin so an unversioned REMOTE SPA isn't
//      mislabelled 'bundled' (the post-#976 collateral the URL-extraction
//      assumption left behind): bundled/dev origins → 'bundled', any other
//      (remote) origin → 'remote'.

// Legacy per-SHA URL shape (pre-#976), kept for backward-compat extraction.
// Mirrors SPA_HASH_RE in `src/main/ipc/versionInfo.ts`. Bounded {7,40} on the
// hex group + the literal `/spa/` after the `[^/]+` host keep backtracking
// linear (no super-linear ReDoS).
const SPA_SHA_FROM_URL_PATTERN = /^https?:\/\/[^/]+\/spa\/([0-9a-f]{7,40})\//i;

// Sanity cap on the input length before regex matching — defense-in-depth
// against unbounded inputs even though the regex itself is bounded.
const MAX_HREF_LEN_FOR_MATCH = 2048;

// Origins that mean "not a remote SPA": the bundled `app://` scheme, legacy
// `file://`, and the localhost dev server. Keeps the unversioned-remote
// fallback ('remote') distinct from a genuinely bundled/dev build ('bundled').
function isBundledOrigin(href: string): boolean {
  try {
    const { protocol, hostname } = new URL(href);
    return (
      protocol === 'app:' ||
      protocol === 'file:' ||
      hostname === 'localhost' ||
      hostname === '127.0.0.1'
    );
  } catch {
    return true; // malformed → treat as bundled (conservative)
  }
}

function detectSpaVersion(): string {
  const buildTime = import.meta.env.VITE_SPA_VERSION;
  if (buildTime) return buildTime;

  const href = (globalThis.location?.href ?? '').slice(0, MAX_HREF_LEN_FOR_MATCH);
  // SonarQube typescript:S6594 prefers `RegExp.exec()` over `String#match()`
  // for non-global patterns.
  const match = SPA_SHA_FROM_URL_PATTERN.exec(href);
  if (match) return match[1];

  // No version in the URL: a bundled/dev origin is 'bundled'; any other origin
  // is an unversioned remote SPA (post-#976 flat host with VITE_SPA_VERSION
  // unset) — report 'remote' so it is not mislabelled as a bundled build.
  return isBundledOrigin(href) ? 'bundled' : 'remote';
}

export const SPA_VERSION = detectSpaVersion();
