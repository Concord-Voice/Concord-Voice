/**
 * Server → client wire shapes for the #2153 media-rate policer. Additive
 * only: no IPC contract, OpenAPI or control-plane change. The renderer's zod
 * mirror lives in `client/desktop/src/renderer/services/voice/mediaPolicyEvents.ts`.
 */
import type { MediaPolicyCooldownError, MediaSource } from './roomManager.js';

/** `producer-paused` / `producer-resumed`, at every emit site. */
export interface ProducerStateChangePayload {
  producerId: string;
  userId: string;
  kind: 'audio' | 'video';
  source: MediaSource;
}

/** `media-policy-notice`, to the owner socket only. `action` is a closed enum. */
export interface MediaPolicyNoticePayload {
  producerId: string;
  kind: 'audio' | 'video';
  source: MediaSource;
  action: 'paused';
}

/** The `join-room` / `produce` ack while the user is in a media-policy cooldown. */
export interface MediaPolicyCooldownAck {
  error: 'Media policy cooldown';
  code: 'media_policy_cooldown';
  retryAfterSec: number;
}

/** The `resume-producer` ack while the producer is latched. */
export interface MediaPolicyPausedResumeAck {
  error: 'media_policy_paused';
  message: 'Paused by media policy';
}

export const MEDIA_POLICY_PAUSED_RESUME_ACK: Readonly<MediaPolicyPausedResumeAck> = Object.freeze({
  error: 'media_policy_paused',
  message: 'Paused by media policy',
});

export function mediaPolicyCooldownAck(error: MediaPolicyCooldownError): MediaPolicyCooldownAck {
  return {
    error: 'Media policy cooldown',
    code: 'media_policy_cooldown',
    retryAfterSec: error.retryAfterSec,
  };
}
