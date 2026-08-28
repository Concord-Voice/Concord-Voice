/**
 * SPA Loader — Adaptive renderer loading for Tier 3 hot updates.
 *
 * On startup (packaged builds only), fetches the server's client config to
 * determine whether to load the renderer from a remote SPA URL or the bundled
 * index.html.
 *
 * Decision flow:
 * 1. Read persisted API base from token metadata (requires prior login)
 * 2. Fetch GET /api/v1/client/config?ipc={contract} with a short timeout
 * 3. If server returns a spaUrl and our contract version is compatible → loadURL(spaUrl)
 * 4. Otherwise → loadFile(bundled index.html)
 *
 * Safety guarantees:
 * - First-time users (no stored token) → always bundled
 * - Server unreachable → always bundled
 * - IPC contract mismatch → bundled (shell needs a native update)
 * - Self-hosted servers (no spaUrl) → always bundled
 */

import { net } from 'electron';
import { createHash } from 'node:crypto';
import { IPC_CONTRACT_VERSION } from './ipcContract';
import { getPersistedApiBase } from './tokenManager';
import { setSpaHash, setSpaVersion } from './spaState';
import { getBuildSha7 } from './buildInfo';
import type { SpaFallbackKind } from '../shared/spaIpcTypes';
import { CONFIG_TIMEOUT_MS } from './spaTiming';

const SPA_NO_CACHE_HEADERS = {
  'Cache-Control': 'no-cache',
  Pragma: 'no-cache',
} as const;

export const SPA_NO_CACHE_LOAD_OPTIONS = {
  extraHeaders: 'Cache-Control: no-cache\nPragma: no-cache\n',
} as const;

interface ClientConfigResponse {
  spaUrl?: string;
  spaIpcContract?: number;
}

export interface SpaLoadDecision {
  mode: 'remote' | 'bundled';
  url?: string;
  reason: string;
}

/**
 * Determine whether to load the renderer from a remote SPA or the bundled file.
 * This runs in the main process before the BrowserWindow loads any content.
 */
export async function resolveSpaSource(): Promise<SpaLoadDecision> {
  const apiBase = getPersistedApiBase();
  if (!apiBase) {
    return { mode: 'bundled', reason: 'no persisted API base (first launch or logged out)' };
  }

  try {
    const configUrl = `${apiBase}/api/v1/client/config?ipc=${IPC_CONTRACT_VERSION}`;
    const result = await fetchJsonWithTimeout<ClientConfigResponse>(configUrl, CONFIG_TIMEOUT_MS);

    if (!result.ok) {
      return { mode: 'bundled', reason: `config fetch returned ${result.status}` };
    }

    const data = result.data as ClientConfigResponse;

    if (!data.spaUrl) {
      return { mode: 'bundled', reason: 'server has no spaUrl configured' };
    }

    // Security: only allow HTTPS remote SPAs (the SPA gets full IPC access)
    try {
      const spaOrigin = new URL(data.spaUrl);
      if (spaOrigin.protocol !== 'https:') {
        return {
          mode: 'bundled',
          reason: `spaUrl rejected: non-HTTPS protocol ${spaOrigin.protocol}`,
        };
      }

      // Defensive sentinel: reject the legacy /api/v1/spa/ prefix. PR #726 moved
      // the SPA handler to /spa/* to escape Cloudflare JSD-beacon injection on
      // API-shaped paths. If the server ever returns a spaUrl under the old
      // prefix, treat it as a misconfiguration and fall back to bundled. Narrow
      // by design — we do NOT reject all /api/* prefixes, just /api/v1/spa/.
      // See #750 (parent epic #749).
      if (spaOrigin.pathname.startsWith('/api/v1/spa/')) {
        return {
          mode: 'bundled',
          reason: 'spaUrl rejected: legacy /api/v1/spa/ path (poisoned sentinel)',
        };
      }
    } catch {
      return { mode: 'bundled', reason: 'spaUrl rejected: invalid URL' };
    }

    if (!data.spaIpcContract || data.spaIpcContract <= 0) {
      return { mode: 'bundled', reason: 'server spaIpcContract is zero or absent' };
    }

    if (IPC_CONTRACT_VERSION < data.spaIpcContract) {
      return {
        mode: 'bundled',
        reason: `IPC contract ${IPC_CONTRACT_VERSION} < required ${data.spaIpcContract} — shell update needed`,
      };
    }

    // All checks pass — use remote SPA
    return {
      mode: 'remote',
      url: data.spaUrl,
      reason: `remote SPA compatible (contract ${IPC_CONTRACT_VERSION} >= ${data.spaIpcContract})`,
    };
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    return { mode: 'bundled', reason: `config fetch failed: ${message}` };
  }
}

interface TimedJsonResult<T> {
  ok: boolean;
  status: number;
  data: T | null;
}

/**
 * Fetch JSON under a deadline that spans the WHOLE operation — headers AND body
 * — using Electron's net module (respects proxy settings, works before the
 * renderer loads, not subject to CORS).
 *
 * The body read is inside the deadline deliberately (#2363, Codex). The earlier
 * shape returned a `Response` and cleared its timer the moment `net.fetch`
 * produced one, i.e. when HEADERS arrived; the caller then awaited
 * `response.json()` with no deadline and an AbortController that could no longer
 * fire. A server that answers promptly and then trickles or stalls the body hung
 * SPA resolution indefinitely, and silently — nothing above this has a bound
 * either. It also broke the arithmetic behind DEEP_LINK_CARRY_MAX_AGE_MS, which
 * budgets one `resolveSpaSource` attempt at CONFIG_TIMEOUT_MS. This constant is a
 * budget for producing a DECISION, not for producing headers.
 *
 * The `timedOut` flag preserves the `timeout after Nms` message across the abort:
 * `isUnexpectedBundled` / `isTransientRemoteFailure` classify on that string, so
 * letting a bare AbortError surface would silently reclassify every timeout.
 */
async function fetchJsonWithTimeout<T>(
  url: string,
  timeoutMs: number
): Promise<TimedJsonResult<T>> {
  const controller = new AbortController();
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);

  try {
    const response = await net.fetch(url, {
      signal: controller.signal,
      headers: SPA_NO_CACHE_HEADERS,
      cache: 'no-store',
    });
    if (!response.ok) return { ok: false, status: response.status, data: null };
    return { ok: true, status: response.status, data: (await response.json()) as T };
  } catch (err) {
    if (timedOut) throw new Error(`timeout after ${timeoutMs}ms`);
    throw err;
  } finally {
    clearTimeout(timer);
  }
}

/**
 * Classify a SpaLoadDecision.reason as "expected" (first launch, server
 * has no SPA configured, contract zero) or "unexpected" (config fetch
 * failed, spaUrl rejected, IPC contract mismatch, anything else).
 *
 * Used by main.ts to decide whether to surface the Option C overlay
 * (#830) — only unexpected fallbacks should trigger it. Expected
 * fallbacks are normal operation (e.g., a self-hosted server with no
 * spaUrl configured, or a logged-out user on a fresh launch).
 *
 * Unknown reasons are treated as unexpected (fail-loud principle):
 * if a future code path adds a new reason string we didn't anticipate,
 * we'd rather surface it than silently swallow it.
 */
const EXPECTED_BUNDLED_REASON_PREFIXES = [
  'no persisted API base',
  'server has no spaUrl configured',
  'server spaIpcContract is zero or absent',
];

export function isUnexpectedBundled(reason: string): boolean {
  return !EXPECTED_BUNDLED_REASON_PREFIXES.some((prefix) => reason.startsWith(prefix));
}

/**
 * Classify a bundled-fallback reason as a TRANSIENT remote failure — i.e. the
 * server was reachable-in-principle but the config fetch failed or returned a
 * 5xx. The signed LKG cache (#1870) is consulted ONLY for these reasons: a
 * transient network/server hiccup is exactly when a last-known-good cache should
 * bridge the gap.
 *
 * Deliberately NARROW. It must NOT match:
 *   - expected bundled reasons (no apiBase, no spaUrl, contract zero) — those
 *     are normal operation, not an outage; serving a cache there would mask a
 *     legitimately-bundled posture.
 *   - IPC-contract mismatch or a rejected spaUrl — a stale cache must not bypass
 *     a required binary update or a server-side misconfiguration signal.
 *
 * Matches `resolveSpaSource`'s two transient reason shapes:
 *   - `config fetch failed: <message>`        (network error / timeout, line 118)
 *   - `config fetch returned 5xx`             (server-side 5xx, line 64)
 * A 4xx (`config fetch returned 4xx`) is NOT transient — it is a client/auth
 * problem, so the cache is not consulted.
 */
export function isTransientRemoteFailure(reason: string): boolean {
  return reason.startsWith('config fetch failed') || /^config fetch returned 5\d\d/.test(reason);
}

/**
 * Classify an UNEXPECTED bundled-fallback reason for the renderer's diagnostic
 * banner (#2401). See `SpaFallbackKind` in shared/spaIpcTypes.ts for what each
 * class means and why only `unreachable` may be retracted.
 *
 * Call only for reasons `isUnexpectedBundled()` accepts — an expected reason
 * emits no diagnostic at all, so it has no class.
 *
 * `unreachable` deliberately reuses `isTransientRemoteFailure` rather than
 * re-deriving the predicate: "the config fetch got no answer or a 5xx" is
 * exactly the same question the LKG cache already asks, and one classifier
 * cannot drift from the other.
 */
export function classifyFallbackReason(reason: string): SpaFallbackKind {
  if (isTransientRemoteFailure(reason)) return 'unreachable';
  if (reason.startsWith('IPC contract ')) return 'contract';
  // Everything else — spaUrl rejected (non-HTTPS / invalid / #750 poisoned
  // sentinel), a 4xx, and any future reason string we have not anticipated.
  // Conservative by construction: `rejected` is never retracted, so an
  // unclassified new reason stays fail-loud.
  return 'rejected';
}

/**
 * Regex to extract the deploy SHA from a remote SPA URL path of the form
 * `/spa/<sha>/...`. Mirrors the pattern in versionInfo.ts.
 */
const SPA_SHA_RE = /^\/spa\/([a-f0-9]+)\/?/i;

/**
 * Capture the SHA-256 hash of the entry HTML bytes that the renderer loaded,
 * and persist it — alongside the effective loader mode and SPA version — in
 * the spaState singletons so the attestation request can include them.
 *
 * This is BEST-EFFORT: a capture failure MUST NOT throw (the caller awaits it
 * inside the renderer-load path and a throw would break SPA loading). On
 * error, the singletons retain whatever value they held before this call.
 *
 * Hash format: `sha256:<lowercase-hex>`, matching CI's `shasum -a 256` output.
 *
 * @param mode    The effective loader mode ('remote' or 'bundled').
 * @param remoteUrl The resolved remote SPA URL (required when mode='remote').
 */
export async function captureSpaHash(
  mode: 'remote' | 'bundled',
  remoteUrl?: string
): Promise<void> {
  // Determine the URL to fetch the entry HTML from.
  const entryUrl = mode === 'remote' && remoteUrl ? remoteUrl : 'app://concord/index.html';

  const hash = await hashEntryHtml(entryUrl);
  // Best-effort capture: on a fetch/hash failure the singletons retain their
  // previous values so the caller's SPA load path is not interrupted.
  if (!hash) return;

  // Derive the SPA version:
  //   remote  → the <sha> segment from the URL path (/spa/<sha>/index.html)
  //   bundled → sha7 derived from the build tag baked into the app at CI time
  //
  // The server's SPA registry is keyed by sha7 (main-cd.yml publishes with
  // `GITHUB_SHA:0:7`), so the bundled-mode `spa_version` MUST be sha7 too.
  // Using `getBuildTag()` directly here ships `release-<sha8>` (8 chars +
  // prefix) which the server cannot resolve → 403 ATTESTATION_UNKNOWN_RELEASE
  // even for legitimately-built bundles. `getBuildSha7()` returns the
  // canonical sha7 form for release builds and '' for PR-smoke / dev builds
  // (the latter is the correct fail-loud posture — non-release bundles are
  // not in the server's registry).
  let version: string;
  if (mode === 'remote' && remoteUrl) {
    try {
      const pathname = new URL(remoteUrl).pathname;
      const shaMatch = SPA_SHA_RE.exec(pathname);
      version = shaMatch ? shaMatch[1] : '';
    } catch {
      version = '';
    }
  } else {
    version = getBuildSha7();
  }

  setSpaHash(hash);
  setSpaVersion(version);
}

/**
 * Fetch the entry HTML at `url` and return its `sha256:<hex>` content hash, or
 * null on any fetch/hash error (best-effort, never throws). Shared by
 * captureSpaHash (load-time capture) and the spa:checkForUpdate handler
 * (available-bytes diff) so both hash the served bytes identically.
 *
 * A production Vite build content-hashes asset filenames, so the entry HTML
 * bytes (and this hash) change on every atomic Cloudflare Pages deploy — which
 * is what makes a "newer UI bytes available" signal possible without a
 * server-exposed build id.
 */
export async function hashEntryHtml(url: string): Promise<string | null> {
  // Bounded, headers AND body, for the same reason fetchJsonWithTimeout is
  // (#2363, Codex). This ran with no timeout and no signal at all, so a trickled
  // entry document stalled it forever — and `captureSpaHash` is awaited inside
  // `applySpaDecision`, INSIDE the deep-link carry wrapper, so an unbounded tail
  // here keeps `deepLinkCarryDepth` raised indefinitely after the successor
  // document has already loaded and been replayed to.
  //
  // Reuses CONFIG_TIMEOUT_MS: same budget, same startup path, and a second
  // constant would only invite the two to drift. Failure stays best-effort — the
  // hash singletons keep their previous values and the SPA load is not
  // interrupted — so a timeout costs an attestation hash, never a launch.
  const controller = new AbortController();
  let timedOut = false;
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, CONFIG_TIMEOUT_MS);

  try {
    const response = await net.fetch(url, {
      signal: controller.signal,
      headers: SPA_NO_CACHE_HEADERS,
      cache: 'no-store',
    });
    if (!response.ok) {
      return null;
    }
    const arrayBuffer = await response.arrayBuffer();
    const bytes = Buffer.from(arrayBuffer);
    return `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
  } catch (err) {
    // Per [internal]rules/observability.md, never pass raw err to console.warn.
    console.warn(
      '[SpaLoader] entry HTML hash failed:',
      timedOut ? `timeout after ${CONFIG_TIMEOUT_MS}ms` : (err as Error).message
    );
    return null;
  } finally {
    clearTimeout(timer);
  }
}
