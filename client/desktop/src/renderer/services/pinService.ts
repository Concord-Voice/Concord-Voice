import { apiFetch, safeJson } from './apiClient';
import type { MessageWithUser } from '../types/chat';

export interface PinResponse {
  message_id: string;
  pinned_at: string;
  pinned_by: string;
  already_pinned?: boolean;
}

export interface UnpinResponse {
  message_id: string;
  already_unpinned?: boolean;
}

export interface GetPinsResponse {
  pinned_messages: MessageWithUser[];
  count: number;
}

export async function pinMessage(messageId: string): Promise<PinResponse> {
  const res = await apiFetch(`/api/v1/messages/${messageId}/pin`, {
    method: 'POST',
  });
  if (!res.ok) {
    const err = await safeJson<{ error: string }>(res);
    throw new Error(err.error || 'Failed to pin message');
  }
  return safeJson<PinResponse>(res);
}

export async function unpinMessage(messageId: string): Promise<UnpinResponse> {
  const res = await apiFetch(`/api/v1/messages/${messageId}/pin`, {
    method: 'DELETE',
  });
  if (!res.ok) {
    const err = await safeJson<{ error: string }>(res);
    throw new Error(err.error || 'Failed to unpin message');
  }
  return safeJson<UnpinResponse>(res);
}

/**
 * Fetch pinned messages for a server channel.
 *
 * Throws on 404 — a missing channel is always an error for server channels
 * (unlike DM conversations, which may 404 before the first pin is created).
 */
export async function getChannelPins(channelId: string): Promise<MessageWithUser[]> {
  const res = await apiFetch(`/api/v1/channels/${channelId}/pins`);
  if (!res.ok) {
    throw new Error('Failed to fetch pinned messages');
  }
  const data = await safeJson<GetPinsResponse>(res);
  return data.pinned_messages;
}

/**
 * Fetch pinned messages for a DM conversation.
 *
 * The backend exposes the same URL shape (`/api/v1/channels/{id}/pins`) for
 * DM conversations — the `{id}` parameter is the DM conversation ID.
 *
 * Returns an empty array on 404 so DM-pin UI can degrade gracefully if the
 * backend route isn't available yet (e.g. during phased rollout or before
 * the first pin is created in a DM conversation).
 */
export async function getPins(channelOrConversationId: string): Promise<MessageWithUser[]> {
  const res = await apiFetch(`/api/v1/channels/${channelOrConversationId}/pins`);
  if (res.status === 404) {
    return [];
  }
  if (!res.ok) {
    throw new Error('Failed to fetch pinned messages');
  }
  const data = await safeJson<GetPinsResponse>(res);
  return data.pinned_messages;
}
