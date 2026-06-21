import { vi, describe, it, expect, beforeEach } from 'vitest';
import { pinMessage, unpinMessage, getChannelPins, getPins } from '@/renderer/services/pinService';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

import { apiFetch, safeJson } from '@/renderer/services/apiClient';

const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;
const mockSafeJson = safeJson as ReturnType<typeof vi.fn>;

describe('pinService', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('pinMessage', () => {
    it('sends POST to correct URL', async () => {
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({
        message_id: 'msg-1',
        pinned_at: '2025-01-01',
        pinned_by: 'user-1',
      });

      await pinMessage('msg-1');

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/messages/msg-1/pin', { method: 'POST' });
    });

    it('returns parsed response', async () => {
      const expected = { message_id: 'msg-1', pinned_at: '2025-01-01', pinned_by: 'user-1' };
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue(expected);

      const result = await pinMessage('msg-1');
      expect(result).toEqual(expected);
    });

    it('throws on error response', async () => {
      mockApiFetch.mockResolvedValue({ ok: false });
      mockSafeJson.mockResolvedValue({ error: 'Maximum of 50 pinned messages per channel' });

      await expect(pinMessage('msg-1')).rejects.toThrow(
        'Maximum of 50 pinned messages per channel'
      );
    });
  });

  describe('unpinMessage', () => {
    it('sends DELETE to correct URL', async () => {
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({ message_id: 'msg-1' });

      await unpinMessage('msg-1');

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/messages/msg-1/pin', { method: 'DELETE' });
    });

    it('throws on error response', async () => {
      mockApiFetch.mockResolvedValue({ ok: false });
      mockSafeJson.mockResolvedValue({ error: 'Failed to unpin' });

      await expect(unpinMessage('msg-1')).rejects.toThrow('Failed to unpin');
    });
  });

  describe('getChannelPins', () => {
    it('sends GET to correct URL', async () => {
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({ pinned_messages: [], count: 0 });

      await getChannelPins('channel-1');

      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/channels/channel-1/pins');
    });

    it('returns pinned messages array', async () => {
      const pins = [{ id: 'msg-1', content: 'Pinned!' }];
      mockApiFetch.mockResolvedValue({ ok: true });
      mockSafeJson.mockResolvedValue({ pinned_messages: pins, count: 1 });

      const result = await getChannelPins('channel-1');
      expect(result).toEqual(pins);
    });

    it('throws on error response', async () => {
      mockApiFetch.mockResolvedValue({ ok: false, status: 500 });

      await expect(getChannelPins('channel-1')).rejects.toThrow('Failed to fetch pinned messages');
    });

    it('throws on 404 (strict — channel must exist)', async () => {
      mockApiFetch.mockResolvedValue({ ok: false, status: 404 });

      await expect(getChannelPins('channel-1')).rejects.toThrow('Failed to fetch pinned messages');
    });
  });

  describe('getPins (generic channel/DM conversation)', () => {
    it('returns empty array on 404 (graceful degrade for DM pins)', async () => {
      mockApiFetch.mockResolvedValue({ ok: false, status: 404 });
      const result = await getPins('conv-123');
      expect(result).toEqual([]);
      expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/channels/conv-123/pins');
    });

    it('returns pinned_messages on success', async () => {
      const pins = [{ id: 'msg-1', content: 'pinned' }];
      mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
      mockSafeJson.mockResolvedValue({ pinned_messages: pins, count: 1 });
      const result = await getPins('conv-123');
      expect(result).toEqual(pins);
    });

    it('throws on non-404 error responses', async () => {
      mockApiFetch.mockResolvedValue({ ok: false, status: 500 });
      await expect(getPins('conv-123')).rejects.toThrow('Failed to fetch pinned messages');
    });
  });
});
