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

const CONFIG_TIMEOUT_MS = 5_000;

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
    const response = await fetchWithTimeout(configUrl, CONFIG_TIMEOUT_MS);

    if (!response.ok) {
      return { mode: 'bundled', reason: `config fetch returned ${response.status}` };
    }

    const data = (await response.json()) as ClientConfigResponse;

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

/**
 * Fetch with a timeout using Electron's net module (respects proxy settings,
 * works before renderer loads, not subject to CORS).
 */
function fetchWithTimeout(url: string, timeoutMs: number): Promise<Response> {
  return new Promise((resolve, reject) => {
    const controller = new AbortController();
    const timer = setTimeout(() => {
      controller.abort();
      reject(new Error(`timeout after ${timeoutMs}ms`));
    }, timeoutMs);

    net
      .fetch(url, { signal: controller.signal })
      .then((response) => {
        clearTimeout(timer);
        resolve(response);
      })
      .catch((err) => {
        clearTimeout(timer);
        reject(err);
      });
  });
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
  try {
    // Determine the URL to fetch the entry HTML from.
    const entryUrl = mode === 'remote' && remoteUrl ? remoteUrl : 'app://concord/index.html';

    const response = await net.fetch(entryUrl);
    const arrayBuffer = await response.arrayBuffer();
    const bytes = Buffer.from(arrayBuffer);

    const hash = `sha256:${createHash('sha256').update(bytes).digest('hex')}`;

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
  } catch (err) {
    // Best-effort capture: swallow the error so the caller's SPA load path
    // is not interrupted. The singletons retain their previous values.
    // Per [internal]rules/observability.md, never pass raw err to console.warn.
    console.warn('[SpaLoader] hash capture failed:', (err as Error).message);
  }
}
