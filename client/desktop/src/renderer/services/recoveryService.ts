/**
 * Recovery Service — Preflight diagnostics and crash tracking
 *
 * Used by the connection recovery system to determine whether a disconnect
 * is caused by a server issue (Option A) or client issue (Option B).
 */

import { API_BASE } from '../config';
import { refreshAccessToken } from './apiClient';

/**
 * Result of a single diagnostic check.
 * - 'unknown': check did not run (e.g., short-circuit, IPC unavailable,
 *   or no positive evidence either way)
 * - 'ok':      check ran and passed
 * - 'failed':  check ran and failed
 */
export type CheckResult = 'unknown' | 'ok' | 'failed';

export interface DiagnosticResults {
  /** OS-level "machine reports online" hint. Sourced from navigator.onLine.
   *  May be true on captive portals — see spec §3 "Known tradeoff accepted". */
  internet: CheckResult;
  /** Authoritative connectivity signal — HEAD ${API_BASE}/health succeeded. */
  serverReachable: CheckResult;
  /** Token refresh succeeded via main-process IPC. */
  tokenValid: CheckResult;
  /** Session was revoked by the server (terminal — user must re-auth). */
  sessionRevoked: boolean | undefined;
  /** Renderer did not record a crash on the previous load. */
  rendererStable: CheckResult;
}

/**
 * Run preflight diagnostics to determine the cause of a connection loss.
 * Each check runs independently — no check short-circuits a later one.
 * The four status fields (`internet`, `serverReachable`, `tokenValid`,
 * `rendererStable`) use the `CheckResult` sum type so "did not run"
 * is structurally distinguishable from "ran and failed". `sessionRevoked`
 * remains a `boolean | undefined` because its semantic is positive-evidence-
 * required (always set explicitly when the token check runs).
 */
export async function runPreflight(): Promise<DiagnosticResults> {
  const results: DiagnosticResults = {
    internet: 'unknown',
    serverReachable: 'unknown',
    tokenValid: 'unknown',
    sessionRevoked: undefined,
    rendererStable: 'unknown',
  };

  // 1. Internet — OS-level hint. No fetch, no third-party probe, no privacy leak.
  //    navigator.onLine is unreliable on captive portals (may return true with
  //    no real internet) — that's OK: serverReachable below is the authoritative
  //    connectivity signal. See spec §3 "Known tradeoff accepted".
  results.internet = navigator.onLine ? 'ok' : 'failed';

  // 2. Server reachability — HEAD to /health (bare path; #882).
  //    NOTE: no short-circuit on `internet`. Captive portals can report
  //    navigator.onLine=true with no real connectivity, or vice versa.
  try {
    const res = await fetch(`${API_BASE}/health`, {
      method: 'HEAD',
      cache: 'no-store',
      signal: AbortSignal.timeout(5000),
    });
    results.serverReachable = res.ok ? 'ok' : 'failed';
  } catch {
    results.serverReachable = 'failed';
  }

  // 3. Token validity — attempt refresh via main process IPC.
  //    NOTE: no short-circuit on `serverReachable`. Refresh may use a
  //    different network path (e.g., cached creds) and is worth attempting.
  //    NOTE on sessionRevoked: `performRefresh` maps every failure mode
  //    (HTTP 401, HTTP 500, network unreachable) to `refresh_failed` →
  //    null. We can only claim "session revoked" when we have positive
  //    evidence — i.e., the server was reachable for /health AND refresh
  //    refused our credentials. If serverReachable is false, refresh
  //    failure is more likely a network artifact than a real revocation;
  //    don't trip the fatal-state UI on it.
  try {
    const newToken = await refreshAccessToken();
    results.tokenValid = newToken ? 'ok' : 'failed';
    results.sessionRevoked = results.serverReachable === 'ok' && !newToken;
  } catch {
    results.tokenValid = 'failed';
    // Throw is NOT positive evidence of revocation (could be IPC failure,
    // network blip, etc.). Don't claim revoked without explicit server signal.
    results.sessionRevoked = false;
  }

  // 4. Renderer stability (check sessionStorage crash flag)
  results.rendererStable = sessionStorage.getItem('concord:renderer-crashed') ? 'failed' : 'ok';

  return results;
}

/** Record that the renderer crashed (called from error boundary) */
export function markRendererCrashed(): void {
  sessionStorage.setItem('concord:renderer-crashed', Date.now().toString());
}

/** Clear crash flag (called after successful recovery) */
export function clearCrashFlag(): void {
  sessionStorage.removeItem('concord:renderer-crashed');
}
