import { describe, it, expect, beforeEach, vi } from 'vitest';
import { MessageQueue, MessageStatus } from '@/renderer/services/messageQueue';

describe('MessageQueue', () => {
  let queue: MessageQueue;

  beforeEach(() => {
    queue = new MessageQueue();
  });

  describe('enqueue', () => {
    it('adds a message to the queue', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      expect(id).toBeTruthy();
      expect(queue.size()).toBe(1);
    });

    it('returns a unique ID for each message', () => {
      const id1 = queue.enqueue('channel-1', 'Hello');
      const id2 = queue.enqueue('channel-1', 'World');
      expect(id1).not.toBe(id2);
    });

    it('sets initial status to PENDING', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      const msg = queue.getMessage(id);
      expect(msg?.status).toBe(MessageStatus.PENDING);
    });

    it('sets retryCount to 0', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      const msg = queue.getMessage(id);
      expect(msg?.retryCount).toBe(0);
    });

    it('stores gif slugs for queued retry sends', () => {
      const id = queue.enqueue('channel-1', ' ', 'dm_message', undefined, undefined, 'cat-wave');
      expect(queue.getMessage(id)?.gifSlug).toBe('cat-wave');
    });
  });

  describe('markAsSent', () => {
    it('updates status to SENT', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.markAsSent(id);
      expect(queue.getMessage(id)?.status).toBe(MessageStatus.SENT);
    });

    it('fires onMessageSent callback', () => {
      const onMessageSent = vi.fn();
      const q = new MessageQueue({ onMessageSent });
      const id = q.enqueue('channel-1', 'Hello');
      q.markAsSent(id);
      expect(onMessageSent).toHaveBeenCalledWith(id);
    });
  });

  describe('markAsDelivered', () => {
    it('removes message from queue', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.markAsDelivered(id, 'server-msg-1');
      expect(queue.size()).toBe(0);
      expect(queue.getMessage(id)).toBeUndefined();
    });

    it('fires onMessageDelivered callback', () => {
      const onMessageDelivered = vi.fn();
      const q = new MessageQueue({ onMessageDelivered });
      const id = q.enqueue('channel-1', 'Hello');
      q.markAsDelivered(id, 'server-msg-1');
      expect(onMessageDelivered).toHaveBeenCalledWith(id, 'server-msg-1');
    });
  });

  describe('markAsFailed', () => {
    it('increments retryCount', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.markAsFailed(id, 'Network error');
      const msg = queue.getMessage(id);
      expect(msg?.retryCount).toBe(1);
    });

    it('resets to PENDING for retry when under max retries', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.markAsFailed(id, 'Network error');
      expect(queue.getMessage(id)?.status).toBe(MessageStatus.PENDING);
    });

    it('fires onMessageFailed after max retries (3)', () => {
      const onMessageFailed = vi.fn();
      const q = new MessageQueue({ onMessageFailed });
      const id = q.enqueue('channel-1', 'Hello');
      q.markAsFailed(id, 'err');
      q.markAsFailed(id, 'err');
      q.markAsFailed(id, 'err');
      expect(onMessageFailed).toHaveBeenCalledWith(id, 'err');
      expect(q.getMessage(id)?.status).toBe(MessageStatus.FAILED);
    });

    it('does not fire callback before max retries', () => {
      const onMessageFailed = vi.fn();
      const q = new MessageQueue({ onMessageFailed });
      const id = q.enqueue('channel-1', 'Hello');
      q.markAsFailed(id, 'err');
      q.markAsFailed(id, 'err');
      expect(onMessageFailed).not.toHaveBeenCalled();
    });
  });

  describe('markAsTerminallyFailed', () => {
    it('sets status to FAILED and flushes retryCount to maxRetries', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.markAsTerminallyFailed(id, 'NOT_MEMBER');
      const msg = queue.getMessage(id);
      expect(msg?.status).toBe(MessageStatus.FAILED);
      expect(msg?.retryCount).toBe(msg?.maxRetries);
    });

    it('fires onMessageFailed immediately on first call', () => {
      const onMessageFailed = vi.fn();
      const q = new MessageQueue({ onMessageFailed });
      const id = q.enqueue('channel-1', 'Hello');
      q.markAsTerminallyFailed(id, 'Key rotated — re-establishing secure session…');
      expect(onMessageFailed).toHaveBeenCalledTimes(1);
      expect(onMessageFailed).toHaveBeenCalledWith(
        id,
        'Key rotated — re-establishing secure session…'
      );
    });

    it('is a no-op for non-existent message', () => {
      const onMessageFailed = vi.fn();
      const q = new MessageQueue({ onMessageFailed });
      q.markAsTerminallyFailed('nonexistent', 'err');
      expect(onMessageFailed).not.toHaveBeenCalled();
    });

    it('skips the retry loop entirely (no reset to PENDING)', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.markAsTerminallyFailed(id, 'INVALID_REQUEST');
      // Unlike markAsFailed which resets to PENDING when retries remain,
      // markAsTerminallyFailed stays FAILED.
      expect(queue.getMessage(id)?.status).toBe(MessageStatus.FAILED);
    });
  });

  describe('getPendingMessages', () => {
    it('returns only pending messages', () => {
      const id1 = queue.enqueue('channel-1', 'Hello');
      const id2 = queue.enqueue('channel-1', 'World');
      queue.markAsSent(id1);
      const pending = queue.getPendingMessages();
      expect(pending).toHaveLength(1);
      expect(pending[0].id).toBe(id2);
    });
  });

  describe('getMessagesForChannel', () => {
    it('returns messages for a specific channel', () => {
      queue.enqueue('channel-1', 'Hello');
      queue.enqueue('channel-2', 'World');
      queue.enqueue('channel-1', 'Foo');
      expect(queue.getMessagesForChannel('channel-1')).toHaveLength(2);
      expect(queue.getMessagesForChannel('channel-2')).toHaveLength(1);
    });
  });

  describe('remove', () => {
    it('removes a message by ID', () => {
      const id = queue.enqueue('channel-1', 'Hello');
      queue.remove(id);
      expect(queue.size()).toBe(0);
    });
  });

  describe('clear', () => {
    it('clears all messages', () => {
      queue.enqueue('channel-1', 'Hello');
      queue.enqueue('channel-2', 'World');
      queue.clear();
      expect(queue.size()).toBe(0);
    });
  });

  describe('clearChannel', () => {
    it('clears messages for a specific channel', () => {
      queue.enqueue('channel-1', 'Hello');
      queue.enqueue('channel-2', 'World');
      queue.enqueue('channel-1', 'Foo');
      queue.clearChannel('channel-1');
      expect(queue.size()).toBe(1);
      expect(queue.getMessagesForChannel('channel-1')).toHaveLength(0);
    });
  });

  describe('queue size limit', () => {
    it('trims queue when exceeding max size (100)', () => {
      for (let i = 0; i < 105; i++) {
        queue.enqueue('channel-1', `Message ${i}`);
      }
      expect(queue.size()).toBeLessThanOrEqual(100);
    });
  });
});
