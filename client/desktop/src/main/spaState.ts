/**
 * Shared module-scope state for the active remote SPA load.
 *
 * `remoteSpaBaseUrl` is the origin (scheme + host + port). `remoteSpaUrl` is
 * the full URL including the `/spa/<sha>/index.html` path. Both are written
 * atomically via `setRemoteSpaState()` so origin-only readers and full-URL
 * readers never see divergent state.
 *
 * Origin-only readers: openExternalHandler, SSOIPC, will-navigate origin
 * guard, did-fail-load self-heal filter (#753), spa:requestSelfHeal IPC
 * sender-frame validator (#753).
 * Full-URL reader: PiP window URL construction. Origin alone strips the
 * `/spa/<sha>/` path required by nginx, causing fallthrough to the
 * marketing-site catch-all (#802).
 *
 * Single-threaded JS event-loop semantics make reads at trigger time atomic
 * w.r.t. writes.
 */

let remoteSpaBaseUrl: string | null = null;
let remoteSpaUrl: string | null = null;
let remoteSpaBaseDir: string | null = null;

type SpaStateListener = (url: string | null) => void;
const listeners = new Set<SpaStateListener>();

export function getRemoteSpaBaseUrl(): string | null {
  return remoteSpaBaseUrl;
}

export function getRemoteSpaUrl(): string | null {
  return remoteSpaUrl;
}

/**
 * The directory the SPA is served from — the pathname of `remoteSpaUrl` up to
 * and including the final '/'. `'/'` for the flat Cloudflare Pages host
 * (post-#976, SPA at the origin root), `'/spa/<sha>/'` for the legacy per-SHA
 * host. The self-heal sender-frame / did-fail-load validators use it as the
 * SPA-frame path-prefix, derived at runtime so a host/path migration cannot
 * silently re-break self-heal (the #976 bug). `null` when no SPA is active.
 *
 * LOAD-BEARING INVARIANT: when this returns `'/'` (the flat Pages host), the
 * validators' `pathname.startsWith('/')` prefix is vacuously true, so the
 * self-heal sender-frame trust check collapses to pure origin-equality. That is
 * safe ONLY because `spa.concordvoice.chat` is a DEDICATED, SPA-only origin —
 * "same origin" ⟺ "the SPA", so there is no non-SPA same-origin frame to spoof
 * and the worst case is a bounded reload. If that origin ever co-hosts non-SPA
 * or embeddable content (a second app, marketing pages, a third-party widget),
 * the path-shape defense-in-depth silently disappears and any same-origin frame
 * could trigger main-process self-heal. Keep the remote SPA on a dedicated
 * origin, or restore a non-trivial base-dir / explicit allow-flag before then.
 */
export function getRemoteSpaBaseDir(): string | null {
  return remoteSpaBaseDir;
}

/**
 * Subscribe to SPA state changes. Listener is invoked synchronously with the
 * full URL (or null) whenever `setRemoteSpaState` runs. Returns an
 * unsubscribe function.
 *
 * Listeners are NOT wrapped in try/catch; throwing in a listener propagates
 * to the caller of setRemoteSpaState. This is intentional per
 * [internal]rules/observability.md — silent error swallowing in main-process
 * code is forbidden.
 */
export function onSpaStateChange(listener: SpaStateListener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

// ─── SPA attestation capture state (Task 17 / #677) ─────────────────────────
// These singletons record the hash of the entry HTML bytes the client loaded
// and the SPA version string. A later task reads them when assembling the
// attestation request payload.
//
// Note: an earlier iteration also tracked a loader-mode singleton (remote
// vs bundled), but the dual-hash → single-hash collapse (see the
// 2026-05-31 reconciliation spec for #677) made the loader mode dead weight
// on the attestation wire. The mode DECISION (remote vs bundled) still
// lives in spaLoader.ts and drives delivery selection — only its
// attestation export was removed.

let spaHash = '';
let spaVersion = '';

export function setSpaHash(h: string): void {
  spaHash = h;
}
export function getSpaHash(): string {
  return spaHash;
}
export function setSpaVersion(v: string): void {
  spaVersion = v;
}
export function getSpaVersion(): string {
  return spaVersion;
}

// ─────────────────────────────────────────────────────────────────────────────

/**
 * Atomically set or clear both SPA-state variables. Pass a valid URL to set
 * both (origin extracted via `new URL(decisionUrl).origin`); pass `null` to
 * clear both. If URL parsing throws, both are cleared (treat malformed input
 * as bundled-mode fallback — same posture as the original loadPackagedRenderer
 * catch path).
 */
export function setRemoteSpaState(decisionUrl: string | null): void {
  if (decisionUrl === null) {
    remoteSpaBaseUrl = null;
    remoteSpaUrl = null;
    remoteSpaBaseDir = null;
  } else {
    try {
      const parsed = new URL(decisionUrl);
      remoteSpaBaseUrl = parsed.origin;
      remoteSpaUrl = decisionUrl;
      // pathname up to and including the final '/': '/' for the flat Pages host,
      // '/spa/<sha>/' for the legacy host. See getRemoteSpaBaseDir().
      remoteSpaBaseDir = parsed.pathname.slice(0, parsed.pathname.lastIndexOf('/') + 1) || '/';
    } catch {
      remoteSpaBaseUrl = null;
      remoteSpaUrl = null;
      remoteSpaBaseDir = null;
    }
  }
  listeners.forEach((l) => l(remoteSpaUrl));
}
