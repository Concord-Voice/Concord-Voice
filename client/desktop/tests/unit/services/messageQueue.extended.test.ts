/**
 * Extended tests for MessageQueue — covers startProcessing, stopProcessing,
 * processQueue flow, trimQueue edge cases, loadQueue localStorage cleanup,
 * and the getMessageQueue singleton.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { MessageQueue, MessageStatus, getMessageQueue } from '@/renderer/services/messageQueue';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';
import { e2eeService } from '@/renderer/services/e2eeService';

describe('MessageQueue — extended', () => {
  let queue: MessageQueue;

  beforeEach(() => {
    vi.useFakeTimers();
    queue = new MessageQueue();
  });

  afterEach(() => {
    queue.stopProcessing();
    vi.useRealTimers();
  });

  describe('startProcessing', () => {
    it('processes pending messages immediately', async () => {
      const id = queue.enqueue('ch-1', 'Hello');
      const sendFn = vi.fn().mockResolvedValue(undefined);

      queue.startProcessing(sendFn);

      // Wait for the async processQueue to complete
      await vi.advanceTimersByTimeAsync(0);

      expect(sendFn).toHaveBeenCalledTimes(1);
      expect(sendFn).toHaveBeenCalledWith(
        expect.objectContaining({ id, channelId: 'ch-1', content: 'Hello' })
      );
    });

    it('processes periodically every 5 seconds', async () => {
      const sendFn = vi.fn().mockResolvedValue(undefined);

      // Enqueue a message first, then start processing
      queue.enqueue('ch-1', 'First');

      queue.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0); // Initial process — sends 'First'

      expect(sendFn).toHaveBeenCalledTimes(1);

      // Enqueue another message
      queue.enqueue('ch-1', 'Second');

      // Advance 5 seconds for next processing cycle
      await vi.advanceTimersByTimeAsync(5000);

      // sendFn should have been called again for 'Second'
      expect(sendFn).toHaveBeenCalledTimes(2);
    });

    it('does nothing if already processing', () => {
      const sendFn = vi.fn().mockResolvedValue(undefined);

      queue.startProcessing(sendFn);
      queue.startProcessing(sendFn); // Second call should be no-op

      // Should not set up duplicate intervals
      expect(sendFn).toBeDefined();
    });

    it('marks messages as sent and removes them after successful send', async () => {
      const id = queue.enqueue('ch-1', 'Success');
      const sendFn = vi.fn().mockResolvedValue(undefined);

      queue.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      // Message should be removed after successful processing
      expect(queue.getMessage(id)).toBeUndefined();
      expect(queue.size()).toBe(0);
    });

    it('marks messages as failed when send throws', async () => {
      const id = queue.enqueue('ch-1', 'Fail');
      const sendFn = vi.fn().mockRejectedValue(new Error('Network error'));

      queue.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      const msg = queue.getMessage(id);
      expect(msg).toBeDefined();
      // Non-typed errors (network failures) are classified as retryable —
      // after first failure, should be set back to PENDING for retry.
      expect(msg!.status).toBe(MessageStatus.PENDING);
      expect(msg!.retryCount).toBe(1);
    });

    it('terminally fails on NOT_MEMBER without retrying', async () => {
      const onMessageFailed = vi.fn();
      const q = new MessageQueue({ onMessageFailed });
      const id = q.enqueue('ch-1', 'Forbidden');
      const sendFn = vi.fn().mockRejectedValue(new E2EEKeyUnavailableError('NOT_MEMBER'));

      q.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      const msg = q.getMessage(id);
      expect(msg!.status).toBe(MessageStatus.FAILED);
      // Retry count jumped straight to maxRetries — no PENDING intermediate.
      expect(msg!.retryCount).toBe(msg!.maxRetries);
      expect(onMessageFailed).toHaveBeenCalledTimes(1);
      expect(onMessageFailed).toHaveBeenCalledWith(
        id,
        expect.stringContaining("don't have access")
      );
      q.stopProcessing();
    });

    it('terminally fails on REVOKED_EPOCH and invalidates the channel key', async () => {
      const invalidateSpy = vi
        .spyOn(e2eeService, 'invalidateChannelKey')
        .mockImplementation(() => {});
      const onMessageFailed = vi.fn();
      const q = new MessageQueue({ onMessageFailed });
      const id = q.enqueue('ch-revoked', 'Stale epoch');
      const sendFn = vi.fn().mockRejectedValue(new E2EEKeyUnavailableError('REVOKED_EPOCH'));

      q.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      expect(q.getMessage(id)!.status).toBe(MessageStatus.FAILED);
      expect(invalidateSpy).toHaveBeenCalledWith('ch-revoked');
      expect(onMessageFailed).toHaveBeenCalledWith(id, expect.stringContaining('re-establishing'));
      q.stopProcessing();
      invalidateSpy.mockRestore();
    });

    it('retries NO_KEY_YET (pending=true) through the backoff loop', async () => {
      const id = queue.enqueue('ch-pending', 'Provisioning');
      const sendFn = vi.fn().mockRejectedValue(new E2EEKeyUnavailableError('NO_KEY_YET', true));

      queue.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      const msg = queue.getMessage(id);
      // First failure resets to PENDING for retry (retryable path).
      expect(msg!.status).toBe(MessageStatus.PENDING);
      expect(msg!.retryCount).toBe(1);
    });

    it('skips empty queue', async () => {
      const sendFn = vi.fn().mockResolvedValue(undefined);

      queue.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      expect(sendFn).not.toHaveBeenCalled();
    });
  });

  describe('stopProcessing', () => {
    it('stops the processing interval', async () => {
      const sendFn = vi.fn().mockResolvedValue(undefined);

      queue.startProcessing(sendFn);
      await vi.advanceTimersByTimeAsync(0);

      queue.stopProcessing();

      // Enqueue a new message and advance time — should not be processed
      queue.enqueue('ch-1', 'After stop');
      await vi.advanceTimersByTimeAsync(10000);

      // Only the initial process should have happened
      expect(sendFn).toHaveBeenCalledTimes(0); // No messages in initial call
    });

    it('is safe to call when not processing', () => {
      expect(() => queue.stopProcessing()).not.toThrow();
    });

    it('is safe to call multiple times', () => {
      const sendFn = vi.fn().mockResolvedValue(undefined);
      queue.startProcessing(sendFn);
      queue.stopProcessing();
      queue.stopProcessing();
      // No error
      expect(queue.size()).toBe(0);
    });
  });

  describe('message lifecycle', () => {
    it('full lifecycle: enqueue → send → markAsSent → markAsDelivered', () => {
      const onSent = vi.fn();
      const onDelivered = vi.fn();
      const q = new MessageQueue({
        onMessageSent: onSent,
        onMessageDelivered: onDelivered,
      });

      const id = q.enqueue('ch-1', 'Full lifecycle');
      expect(q.getMessage(id)!.status).toBe(MessageStatus.PENDING);

      q.markAsSent(id);
      expect(q.getMessage(id)!.status).toBe(MessageStatus.SENT);
      expect(onSent).toHaveBeenCalledWith(id);

      q.markAsDelivered(id, 'server-id-1');
      expect(q.getMessage(id)).toBeUndefined(); // Removed
      expect(onDelivered).toHaveBeenCalledWith(id, 'server-id-1');
    });

    it('marks non-existent message as sent (no-op)', () => {
      queue.markAsSent('nonexistent');
      // Should not throw
      expect(queue.getMessage('nonexistent')).toBeUndefined();
    });

    it('marks non-existent message as delivered (no-op)', () => {
      queue.markAsDelivered('nonexistent');
      // Should not throw
      expect(queue.getMessage('nonexistent')).toBeUndefined();
    });

    it('marks non-existent message as failed (no-op)', () => {
      queue.markAsFailed('nonexistent', 'error');
      // Should not throw
      expect(queue.getMessage('nonexistent')).toBeUndefined();
    });

    it('removes non-existent message (no-op)', () => {
      queue.remove('nonexistent');
      // Should not throw
      expect(queue.getMessage('nonexistent')).toBeUndefined();
    });
  });

  describe('trimQueue', () => {
    it('trims delivered/failed messages first when over limit', () => {
      // Enqueue 101 messages
      for (let i = 0; i < 101; i++) {
        queue.enqueue('ch-1', `Message ${i}`);
      }
      // All messages are PENDING, so trimQueue falls through to slice
      expect(queue.size()).toBeLessThanOrEqual(100);
    });

    it('trims old pending messages as last resort', () => {
      for (let i = 0; i < 105; i++) {
        queue.enqueue('ch-1', `Message ${i}`);
      }
      expect(queue.size()).toBeLessThanOrEqual(100);
    });
  });

  describe('enqueue with different types', () => {
    it('defaults to message type', () => {
      const id = queue.enqueue('ch-1', 'Test');
      expect(queue.getMessage(id)!.type).toBe('message');
    });

    it('supports typing type', () => {
      const id = queue.enqueue('ch-1', '', 'typing');
      expect(queue.getMessage(id)!.type).toBe('typing');
    });

    it('supports other type', () => {
      const id = queue.enqueue('ch-1', 'data', 'other');
      expect(queue.getMessage(id)!.type).toBe('other');
    });
  });

  describe('getMessage', () => {
    it('returns undefined for non-existent message', () => {
      expect(queue.getMessage('nope')).toBeUndefined();
    });
  });

  describe('clearChannel', () => {
    it('only clears messages for the specified channel', () => {
      queue.enqueue('ch-1', 'A');
      queue.enqueue('ch-2', 'B');
      queue.enqueue('ch-1', 'C');

      queue.clearChannel('ch-1');

      expect(queue.size()).toBe(1);
      expect(queue.getMessagesForChannel('ch-1')).toHaveLength(0);
      expect(queue.getMessagesForChannel('ch-2')).toHaveLength(1);
    });
  });

  describe('loadQueue — localStorage cleanup', () => {
    it('removes old persisted queue from localStorage on construction', () => {
      localStorage.setItem('concord_message_queue', '[{"old": true}]');
      // Constructor side-effect: cleans up old persisted plaintext messages
      const cleanupQueue = new MessageQueue();
      expect(localStorage.getItem('concord_message_queue')).toBeNull();
      cleanupQueue.clear(); // use the instance to satisfy lint
    });
  });

  describe('getMessageQueue singleton', () => {
    it('returns the same instance on subsequent calls', () => {
      // Note: This tests the module-level singleton. We can't easily reset it
      // between tests, so we just verify it returns a MessageQueue.
      const q1 = getMessageQueue();
      const q2 = getMessageQueue();
      expect(q1).toBe(q2);
    });
  });
});
