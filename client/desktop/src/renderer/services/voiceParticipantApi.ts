/**
 * voiceParticipantApi — thin HTTP client for server-voice participant actions
 * (#487 Scope B). Mirrors the apiFetch path-building used by
 * EnforcementMenuItems (`/api/v1/servers/{serverId}/voice/{userId}/...`).
 *
 * These wrap the control-plane voice-moderation routes:
 *   - move:       POST /api/v1/servers/{serverId}/voice/{userId}/move
 *                 body { target_channel_id } — implemented server-side (ServerMove).
 *   - disconnect: POST /api/v1/servers/{serverId}/voice/{userId}/disconnect
 *                 (kick-from-voice). NOTE: the server-side route for disconnect is
 *                 not yet implemented as a dedicated HTTP endpoint — the
 *                 force-disconnect primitive currently fires only via the
 *                 voice.enforce.disconnect NATS path inside
 *                 revokeTemporaryChannelAccess. The client method is provided so
 *                 the Disconnect UI affordance is wired; it will start succeeding
 *                 once the backend exposes the route.
 */

import { apiFetch } from './apiClient';

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

/** Disconnect (kick-from-voice) a participant from the server voice channel. */
export async function disconnectVoiceParticipant(
  serverId: string,
  userId: string
): Promise<Response> {
  return apiFetch(`/api/v1/servers/${serverId}/voice/${userId}/disconnect`, {
    method: 'POST',
  });
}
