/**
 * Typed error for E2EE key-fetch failures.
 *
 * Mirrored from services/control-plane/pkg/e2eekeys/response.go.
 * When adding, renaming, or removing codes, update both files together.
 *
 * MALFORMED_PAYLOAD is a client-only synthetic code — the server never emits
 * it. It's raised when a wrapped-key payload received from the server fails
 * shape validation (e.g., non-512 bytes after base64 decode). See
 * [internal]specs/2026-04-23-751-*.md §6.4 for the derivation.
 */
export type E2EEKeyErrorCode =
  | 'NOT_MEMBER'
  | 'NO_KEY_YET'
  | 'REVOKED_EPOCH'
  | 'INVALID_REQUEST'
  | 'INTERNAL_ERROR'
  | 'MALFORMED_PAYLOAD';

export class E2EEKeyUnavailableError extends Error {
  constructor(
    public readonly code: E2EEKeyErrorCode,
    public readonly pending: boolean = false
  ) {
    super(`E2EE key unavailable: ${code}`);
    this.name = 'E2EEKeyUnavailableError';
  }
}

/**
 * Type guard: returns true if the error is a typed E2EE key-unavailable error
 * with the retryable `NO_KEY_YET + pending: true` shape. This is the
 * replacement for the legacy `err.message === 'PENDING_KEY'` string match
 * that fired when the server responded with `{pending: true}`.
 *
 * Consumers use this to decide whether to apply a longer/backoff-multiplied
 * retry delay (the server is still provisioning the key) versus generic
 * retry or terminal failure.
 */
export function isPendingKeyError(err: unknown): boolean {
  return (
    err instanceof E2EEKeyUnavailableError && err.code === 'NO_KEY_YET' && err.pending === true
  );
}

/**
 * Classification of how the outbound message queue should treat an error.
 * Consumed by messageQueue.ts and useMessaging.ts to decide retry vs terminal
 * disposition and whether to trigger a rekey flow.
 */
export interface ErrorClassification {
  /** True if the error is retryable with existing backoff policy */
  retryable: boolean;
  /** True if the error signals a key rotation — consumer should invalidate cache + fetch fresh */
  triggerRekey: boolean;
  /** Human-readable UX message appropriate for each error class */
  uxMessage: string;
}

/**
 * Classify an error for message-queue retry/terminal decisions.
 *
 * - NO_KEY_YET (pending) → retryable (backoff)
 * - REVOKED_EPOCH → terminal + trigger rekey
 * - NOT_MEMBER / MALFORMED_PAYLOAD / INVALID_REQUEST → terminal
 * - INTERNAL_ERROR → retryable (transient server-side 500; markAsFailed's
 *   3-strike cap bounds the retry loop)
 * - Any non-E2EEKeyUnavailableError → retryable (preserves existing
 *   network-error retry behavior; WebSocket disconnects and similar transient
 *   failures should cycle through the 3-retry loop as before)
 *
 * Per [internal]specs/2026-04-23-751-*.md §6.5. The retry cadence
 * itself lives in MessageQueue.markAsFailed (3 retries before terminal);
 * this classifier decides whether to SKIP the retry loop and go straight to
 * terminal + surface a code-specific UX message for known terminal
 * E2EE codes.
 */
export function classifyError(err: unknown): ErrorClassification {
  if (!(err instanceof E2EEKeyUnavailableError)) {
    // Non-typed errors (network failures, WebSocket disconnects, encryption
    // output validation, etc.) default to retryable so the existing 3-retry
    // cadence handles transient issues.
    return { retryable: true, triggerRekey: false, uxMessage: 'Unable to send message' };
  }
  switch (err.code) {
    case 'NO_KEY_YET':
      return {
        retryable: err.pending,
        triggerRekey: false,
        uxMessage: err.pending ? 'Key not ready — retrying…' : 'Key unavailable',
      };
    case 'REVOKED_EPOCH':
      return {
        retryable: false,
        triggerRekey: true,
        uxMessage: 'Key rotated — re-establishing secure session…',
      };
    case 'NOT_MEMBER':
      return {
        retryable: false,
        triggerRekey: false,
        uxMessage: "You don't have access to this channel",
      };
    case 'MALFORMED_PAYLOAD':
      return {
        retryable: false,
        triggerRekey: false,
        uxMessage: 'Key unavailable — please try again',
      };
    case 'INVALID_REQUEST':
      return {
        retryable: false,
        triggerRekey: false,
        uxMessage: 'Invalid request',
      };
    case 'INTERNAL_ERROR':
      return {
        retryable: true,
        triggerRekey: false,
        uxMessage: 'Temporarily unavailable — retrying…',
      };
    default:
      // Defense-in-depth: if a future server version emits a code this client
      // hasn't shipped yet, `err.code` may be outside the declared union. Fall
      // back to terminal + generic UX rather than returning undefined (which
      // would cause a TypeError at consumer sites dereferencing .retryable etc.).
      return { retryable: false, triggerRekey: false, uxMessage: 'Unable to send message' };
  }
}
