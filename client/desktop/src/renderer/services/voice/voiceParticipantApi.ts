/**
 * voiceParticipantApi — thin HTTP client for server-voice participant actions
 * (#487 Scope B). Mirrors the apiFetch path-building used by
 * EnforcementMenuItems (`/api/v1/servers/{serverId}/voice/{userId}/...`).
 *
 * These wrap the control-plane voice-moderation routes:
 *   - move:       POST /api/v1/servers/{serverId}/voice/{userId}/move
 *                 body { target_channel_id } — implemented server-side (ServerMove).
 *   - disconnect: POST /api/v1/servers/{serverId}/voice/{userId}/disconnect
 *                 returns 202 after exact-session disconnect commands are
 *                 durably queued. Media application and voice.left arrive later.
 */

import { apiFetch } from '../system/apiClient';

/** Relocate a voice participant to another voice channel in the same server. */
export async function moveVoiceParticipant(
  serverId: string,
  userId: string,
  targetChannelId: string
): Promise<Response> {
  return apiFetch(`/api/v1/servers/${serverId}/voice/${userId}/move`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ target_channel_id: targetChannelId }),
  });
}

/** Queue an exact-session disconnect; HTTP acceptance does not confirm closure. */
export async function disconnectVoiceParticipant(
  serverId: string,
  userId: string
): Promise<Response> {
  return apiFetch(`/api/v1/servers/${serverId}/voice/${userId}/disconnect`, {
    method: 'POST',
  });
}
