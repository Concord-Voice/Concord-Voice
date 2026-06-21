/**
 * Pure URL-validation + path-resolution helper for the `app://` protocol
 * handler. Extracted from main.ts so the path-traversal rejection logic
 * is unit-testable without the Electron protocol runtime.
 *
 * Trust boundary: the input `requestUrl` is attacker-controllable (any
 * code in the renderer or any subresource fetch can synthesize a URL).
 * The resolver MUST reject any path that escapes `bundleRoot` after
 * resolution. The two-condition check `startsWith(bundleRoot + path.sep)
 * || equals bundleRoot` covers both subdirectory access and the bundle
 * root itself.
 *
 * The resolver is pure: it performs no I/O. The caller is responsible
 * for serving the resolved path via `net.fetch('file://' + absolutePath)`
 * or `fs.readFile(absolutePath)`.
 */

import path from 'node:path';

export interface AppProtocolResolveResult {
  ok: boolean;
  status: 404 | 403;
  absolutePath: string | null;
}

export function resolveAppProtocolPath(
  requestUrl: string,
  bundleRoot: string
): AppProtocolResolveResult {
  let url: URL;
  try {
    url = new URL(requestUrl);
  } catch {
    return { ok: false, status: 404, absolutePath: null };
  }

  if (url.host !== 'concord') {
    return { ok: false, status: 404, absolutePath: null };
  }

  // Defense in depth: reject any request URL that contains `..` segments
  // or their percent-encoded forms, even though the WHATWG URL parser
  // normalizes them away in `url.pathname`. We inspect the raw input so
  // an attacker cannot rely on the parser's silent normalization to
  // confuse downstream consumers; treat any traversal attempt as a
  // 403, not a quiet redirect to a different file.
  //
  // We extract the raw path portion of the request URL by stripping the
  // `app://<host>` prefix, then decode percent-escapes once and look for
  // `..` segments separated by `/` (or at the boundaries).
  // Case-insensitive prefix match: WHATWG URL parser normalizes scheme to
  // lowercase but preserves host case for non-special schemes like app://.
  // A mixed-case input like 'APP://concord/../foo' would otherwise fail the
  // exact-case startsWith check, leaving rawPath empty and silently skipping
  // the `..` scan. The fallback path.resolve boundary check still prevents
  // actual escape (because WHATWG strips `..` from pathname), but the
  // "fail loud on suspicious input" intent of the layered defense requires
  // the raw scan to be reachable regardless of input case. Slice from the
  // ORIGINAL requestUrl by schemeAndHost.length — case differences don't
  // change byte length for ASCII inputs (which is all `app://concord/...`
  // ever is), so the slice index is correct.
  const schemeAndHost = `${url.protocol}//${url.host}`;
  const rawPath = requestUrl.toLowerCase().startsWith(schemeAndHost.toLowerCase())
    ? requestUrl.slice(schemeAndHost.length)
    : '';
  let decodedRaw: string;
  try {
    decodedRaw = decodeURIComponent(rawPath);
  } catch {
    return { ok: false, status: 403, absolutePath: null };
  }
  const segments = decodedRaw.split(/[/\\]/);
  if (segments.includes('..')) {
    return { ok: false, status: 403, absolutePath: null };
  }

  const requestedPath = url.pathname === '/' || url.pathname === '' ? '/index.html' : url.pathname;

  // Decode percent-encoded path components (e.g., %2E%2E) before resolving.
  // Without this, an encoded traversal would slip past the resolve check.
  let decodedPath: string;
  try {
    decodedPath = decodeURIComponent(requestedPath);
  } catch {
    return { ok: false, status: 403, absolutePath: null };
  }

  const absolutePath = path.resolve(bundleRoot, '.' + decodedPath);
  const isInsideBundle =
    absolutePath.startsWith(bundleRoot + path.sep) || absolutePath === bundleRoot;

  if (!isInsideBundle) {
    return { ok: false, status: 403, absolutePath: null };
  }

  return { ok: true, status: 404, absolutePath };
}
