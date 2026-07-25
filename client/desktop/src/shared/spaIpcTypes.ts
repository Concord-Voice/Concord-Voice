/**
 * SPA self-heal IPC types — single source of truth across the renderer/main
 * trust boundary (#753, ADR-0001).
 *
 * The renderer can only originate two reasons (`'chunk-load'` from a window
 * `error` event, `'chunk-import-rejected'` from `unhandledrejection`). The
 * main process can additionally originate `'main-frame-load'` and
 * `'sub-resource'` from its `did-fail-load` listener.
 *
 * The IPC handler MUST validate `payload.reason` against
 * `RENDERER_SELF_HEAL_REASONS` before forwarding to `attemptSelfHeal` —
 * a malicious or buggy renderer could otherwise impersonate a main-process
 * trigger and corrupt the recovery flow's reason context (telemetry,
 * exhaustion accounting). `isRendererSelfHealRequest()` is the canonical
 * runtime guard.
 */

export const RENDERER_SELF_HEAL_REASONS = ['chunk-load', 'chunk-import-rejected'] as const;
export type RendererSelfHealReason = (typeof RENDERER_SELF_HEAL_REASONS)[number];

export const MAIN_PROCESS_SELF_HEAL_REASONS = ['main-frame-load', 'sub-resource'] as const;
export type MainProcessSelfHealReason = (typeof MAIN_PROCESS_SELF_HEAL_REASONS)[number];

/** Full union — used internally by the recovery primitive. */
export type SelfHealReason = RendererSelfHealReason | MainProcessSelfHealReason;

/**
 * Renderer-side IPC payload. Only renderer-originated reasons are accepted
 * across the IPC boundary. The `url` field is diagnostic-only (logged, never
 * fed back into navigation) — the main-process recovery primitive refetches
 * `/api/v1/client/config` from scratch.
 */
export interface RendererSelfHealRequest {
  reason: RendererSelfHealReason;
  url?: string;
}

/**
 * Runtime guard for the renderer-side IPC payload. Use at the IPC handler
 * entry point to reject malformed or boundary-violating payloads BEFORE
 * forwarding into `attemptSelfHeal`. TypeScript's structural typing erases
 * at runtime; this guard is the boundary check.
 */
export function isRendererSelfHealRequest(value: unknown): value is RendererSelfHealRequest {
  if (typeof value !== 'object' || value === null) return false;
  const v = value as Record<string, unknown>;
  if (typeof v.reason !== 'string') return false;
  if (!(RENDERER_SELF_HEAL_REASONS as readonly string[]).includes(v.reason)) return false;
  if (v.url !== undefined && typeof v.url !== 'string') return false;
  return true;
}

/**
 * Bundled-SPA fallback diagnostic classes (#2401).
 *
 * `isUnexpectedBundled` (spaLoader.ts) fires the `app:configFetchFailed`
 * diagnostic for six distinct conditions, and only ONE of them means the
 * servers were actually unreachable. Emitting a single hardcoded "Could not
 * reach Concord servers" string for all six was both factually wrong (five
 * occur against a perfectly reachable server) and un-retractable (the renderer
 * had no way to tell which had happened, so it could not know whether proof of
 * reachability falsified the claim).
 *
 *   unreachable — the config fetch got no answer, or a 5xx. An established
 *                 connection genuinely falsifies this, so it is the ONLY
 *                 retractable class.
 *   rejected    — the server answered and this client refused what it sent: a
 *                 non-HTTPS spaUrl, an invalid URL, a 4xx, or the
 *                 `/api/v1/spa/` poisoned sentinel (#750). Reaching the server
 *                 does not falsify this — with the sentinel, reaching the
 *                 server is the whole point. NEVER retract.
 *   contract    — the shell is older than the deployed SPA requires. Reaching
 *                 the server does not falsify it; the app needs an update.
 *
 * Unknown or future reason strings classify as `rejected`, the conservative
 * arm, so a newly-added reason stays fail-loud — matching the same posture
 * `isUnexpectedBundled` already takes ("unknown reasons are treated as
 * unexpected... we'd rather surface it than silently swallow it").
 */
export const SPA_FALLBACK_KINDS = ['unreachable', 'rejected', 'contract'] as const;
export type SpaFallbackKind = (typeof SPA_FALLBACK_KINDS)[number];

/**
 * User-facing copy per class. Generic per `[internal]rules/observability.md` —
 * no apiBase, host, proxy detail, status code, or PII crosses the boundary.
 */
export const SPA_FALLBACK_MESSAGE: Record<SpaFallbackKind, string> = {
  unreachable: 'Could not reach Concord servers',
  rejected: 'Server sent an app configuration this client refused',
  contract: 'Update required to load the latest interface',
};

/** Payload of the `app:configFetchFailed` event (main → renderer). */
export interface SpaFallbackDiagnostic {
  reason: string;
  /**
   * Absent when emitted by a pre-#2401 shell (the remote SPA can be newer than
   * the shell hosting it). Consumers MUST fail closed on absence — treat an
   * unclassified diagnostic as non-retractable rather than assuming
   * `unreachable`, which would silently re-open the sentinel gap this class
   * exists to close.
   *
   * There is deliberately no runtime guard for this field, unlike
   * `isRendererSelfHealRequest` above. That guard protects the renderer → main
   * direction, where the sender is untrusted; this payload travels main →
   * renderer, and a compromised main process is already outside the threat
   * model. Fail-closed is structural instead: every consumer branches on
   * `kind === 'unreachable'`, so absence, a malformed value, or a class added
   * later all fall through to the non-retractable path without validation.
   */
  kind?: SpaFallbackKind;
}
