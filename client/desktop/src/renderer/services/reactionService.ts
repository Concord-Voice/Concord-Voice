import { apiFetch, safeJson } from './apiClient';
import type { ReactionSummary } from '../types/chat';

interface ToggleReactionResponse {
  action: 'added' | 'removed';
  reaction?: ReactionSummary;
}

export async function toggleReaction(
  messageId: string,
  emoji: string
): Promise<ToggleReactionResponse> {
  const res = await apiFetch(`/api/v1/messages/${messageId}/reactions`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ emoji }),
  });
  if (!res.ok) {
    const err = await safeJson<{ error: string }>(res);
    throw new Error(err.error || 'Failed to toggle reaction');
  }
  return safeJson<ToggleReactionResponse>(res);
}

export async function getReactions(messageId: string): Promise<ReactionSummary[]> {
  const res = await apiFetch(`/api/v1/messages/${messageId}/reactions`);
  if (!res.ok) {
    throw new Error('Failed to fetch reactions');
  }
  const data = await safeJson<{ reactions: ReactionSummary[] }>(res);
  return data.reactions;
}
