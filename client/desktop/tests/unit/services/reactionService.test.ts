import { vi, describe, it, expect, beforeEach } from 'vitest';
import { toggleReaction, getReactions } from '@/renderer/services/reactionService';

// Mock apiClient
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

import { apiFetch, safeJson } from '@/renderer/services/apiClient';

const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;
const mockSafeJson = safeJson as ReturnType<typeof vi.fn>;

describe('reactionService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('toggleReaction', () => {
    it('sends PUT with emoji in body', async () => {
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({
        action: 'added',
        reaction: { emoji: '👍', count: 1, users: [], me: true },
      });

      await toggleReaction('msg-1', '👍');

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/messages/msg-1/reactions', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ emoji: '👍' }),
      });
    });

    it('returns parsed response on success', async () => {
      const expected = {
        action: 'added',
        reaction: { emoji: '❤️', count: 1, users: [], me: true },
      };
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue(expected);

      const result = await toggleReaction('msg-1', '❤️');
      expect(result).toEqual(expected);
    });

    it('throws on error response', async () => {
      mockApiFetch.mockResolvedValue({ ok: false });
      mockSafeJson.mockResolvedValue({ error: 'Invalid emoji' });

      await expect(toggleReaction('msg-1', 'bad')).rejects.toThrow('Invalid emoji');
    });
  });

  describe('getReactions', () => {
    it('sends GET to correct URL', async () => {
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({ reactions: [] });

      await getReactions('msg-1');

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/messages/msg-1/reactions');
    });

    it('returns parsed reactions array', async () => {
      const reactions = [{ emoji: '👍', count: 1, users: [], me: true }];
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({ reactions });

      const result = await getReactions('msg-1');
      expect(result).toEqual(reactions);
    });

    it('throws on error response', async () => {
      mockApiFetch.mockResolvedValue({ ok: false });

      await expect(getReactions('msg-1')).rejects.toThrow('Failed to fetch reactions');
    });
  });
});
