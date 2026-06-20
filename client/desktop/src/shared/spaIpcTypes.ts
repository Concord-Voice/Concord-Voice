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
