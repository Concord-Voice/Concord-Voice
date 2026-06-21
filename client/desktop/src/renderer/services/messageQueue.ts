/**
 * Message Queue Service - Handles offline message queueing and delivery tracking
 *
 * Features:
 * - Queue messages when offline
 * - Persist queue across app restarts (localStorage)
 * - Track message delivery status (pending, sent, delivered, failed)
 * - Auto-retry failed messages
 * - Delivery acknowledgments
 *
 * Integrates with WebSocketService for message delivery
 */

import { classifyError } from './e2eeErrors';
import { e2eeService } from './e2eeService';
import { errorMessage } from '../utils/redactError';

export enum MessageStatus {
  PENDING = 'pending', // Waiting to be sent
  SENT = 'sent', // Sent to server, awaiting ack
  DELIVERED = 'delivered', // Acknowledged by server
  FAILED = 'failed', // Failed to send
}

export interface QueuedMessage {
  id: string; // Unique message ID (client-generated)
  channelId: string;
  content: string;
  timestamp: number;
  status: MessageStatus;
  retryCount: number;
  maxRetries: number;
  type: 'message' | 'dm_message' | 'typing' | 'other';
  mentionMeta?: string;
  replyToId?: string;
  gifSlug?: string;
}

interface MessageQueueCallbacks {
  onMessageSent?: (messageId: string) => void;
  onMessageDelivered?: (messageId: string, serverMessageId?: string) => void;
  onMessageFailed?: (messageId: string, error: string) => void;
}

const QUEUE_STORAGE_KEY = 'concord_message_queue';
const MAX_QUEUE_SIZE = 100;
const MAX_RETRIES = 3;

export class MessageQueue {
  private queue: QueuedMessage[] = [];
  private readonly callbacks: MessageQueueCallbacks = {};
  private processingInterval: NodeJS.Timeout | null = null;

  constructor(callbacks?: MessageQueueCallbacks) {
    this.callbacks = callbacks || {};
    this.loadQueue();
  }

  /**
   * Add a message to the queue
   */
  enqueue(
    channelId: string,
    content: string,
    type: 'message' | 'dm_message' | 'typing' | 'other' = 'message',
    mentionMeta?: string,
    replyToId?: string
  ): string {
    const messageId = this.generateMessageId();

    const queuedMessage: QueuedMessage = {
      id: messageId,
      channelId,
      content,
      timestamp: Date.now(),
      status: MessageStatus.PENDING,
      retryCount: 0,
      maxRetries: MAX_RETRIES,
      type,
      mentionMeta,
      replyToId,
    };

    this.queue.push(queuedMessage);
    this.trimQueue();
    this.saveQueue();

    console.debug(`[MessageQueue] Enqueued ${messageId} for channel ${channelId}`);

    return messageId;
  }

  /**
   * Mark a message as sent (awaiting server acknowledgment)
   */
  markAsSent(messageId: string): void {
    const message = this.queue.find((m) => m.id === messageId);
    if (message) {
      message.status = MessageStatus.SENT;
      this.saveQueue();
      this.callbacks.onMessageSent?.(messageId);
      console.debug(`[MessageQueue] ${messageId} marked as sent`);
    }
  }

  /**
   * Mark a message as delivered (server acknowledged)
   */
  markAsDelivered(messageId: string, serverMessageId?: string): void {
    const index = this.queue.findIndex((m) => m.id === messageId);
    if (index >= 0) {
      const message = this.queue[index];
      message.status = MessageStatus.DELIVERED;

      // Remove from queue after delivery
      this.queue.splice(index, 1);
      this.saveQueue();

      this.callbacks.onMessageDelivered?.(messageId, serverMessageId);
      console.debug(`[MessageQueue] ${messageId} delivered, removed from queue`);
    }
  }

  /**
   * Mark a message as failed
   */
  markAsFailed(messageId: string, error: string): void {
    const message = this.queue.find((m) => m.id === messageId);
    if (message) {
      message.status = MessageStatus.FAILED;
      message.retryCount++;

      if (message.retryCount >= message.maxRetries) {
        console.error(
          `[MessageQueue] Message ${messageId} failed after ${message.retryCount} retries`
        );
        this.callbacks.onMessageFailed?.(messageId, error);
      } else {
        // Reset to pending for retry
        message.status = MessageStatus.PENDING;
        console.warn(
          `[MessageQueue] Message ${messageId} failed, will retry (${message.retryCount}/${message.maxRetries})`
        );
      }

      this.saveQueue();
    }
  }

  /**
   * Mark a message as terminally failed — skips the retry loop entirely.
   *
   * Used for errors classified as non-retryable (NOT_MEMBER, REVOKED_EPOCH,
   * MALFORMED_PAYLOAD, INVALID_REQUEST). Fires the
   * onMessageFailed callback immediately instead of cycling through
   * retryCount attempts. See spec §6.5.
   */
  markAsTerminallyFailed(messageId: string, error: string): void {
    const message = this.queue.find((m) => m.id === messageId);
    if (message) {
      message.status = MessageStatus.FAILED;
      message.retryCount = message.maxRetries;
      // eslint-disable-next-line no-restricted-syntax -- error is typed string (a caller-stringified error message), not a raw Error; no err.cause chain to propagate
      console.error('[MessageQueue] Message %s terminally failed: %s', messageId, error);
      this.callbacks.onMessageFailed?.(messageId, error);
      this.saveQueue();
    }
  }

  /**
   * Get all pending messages
   */
  getPendingMessages(): QueuedMessage[] {
    return this.queue.filter((m) => m.status === MessageStatus.PENDING);
  }

  /**
   * Get all messages for a channel
   */
  getMessagesForChannel(channelId: string): QueuedMessage[] {
    return this.queue.filter((m) => m.channelId === channelId);
  }

  /**
   * Get a specific message by ID
   */
  getMessage(messageId: string): QueuedMessage | undefined {
    return this.queue.find((m) => m.id === messageId);
  }

  /**
   * Remove a message from the queue
   */
  remove(messageId: string): void {
    const index = this.queue.findIndex((m) => m.id === messageId);
    if (index >= 0) {
      this.queue.splice(index, 1);
      this.saveQueue();
      console.debug(`[MessageQueue] ${messageId} removed from queue`);
    }
  }

  /**
   * Clear all messages from the queue
   */
  clear(): void {
    this.queue = [];
    this.saveQueue();
    console.debug('[MessageQueue] Queue cleared');
  }

  /**
   * Clear all messages for a specific channel
   */
  clearChannel(channelId: string): void {
    this.queue = this.queue.filter((m) => m.channelId !== channelId);
    this.saveQueue();
    console.debug(`[MessageQueue] Cleared channel ${channelId}`);
  }

  /**
   * Get queue size
   */
  size(): number {
    return this.queue.length;
  }

  /**
   * Start processing the queue (call when connection is established)
   */
  startProcessing(sendFn: (message: QueuedMessage) => Promise<void>): void {
    if (this.processingInterval) {
      return; // Already processing
    }

    console.debug('[MessageQueue] Started queue processing');

    // Process immediately
    this.processQueue(sendFn);

    // Then process every 5 seconds
    this.processingInterval = setInterval(() => {
      this.processQueue(sendFn);
    }, 5000);
  }

  /**
   * Stop processing the queue (call when connection is lost)
   */
  stopProcessing(): void {
    if (this.processingInterval) {
      clearInterval(this.processingInterval);
      this.processingInterval = null;
      console.debug('[MessageQueue] Stopped queue processing');
    }
  }

  // Private methods

  private async processQueue(sendFn: (message: QueuedMessage) => Promise<void>): Promise<void> {
    const pending = this.getPendingMessages();

    if (pending.length === 0) {
      return;
    }

    console.debug(`[MessageQueue] Processing ${pending.length} pending messages`);

    for (const message of pending) {
      try {
        await sendFn(message);
        this.markAsSent(message.id);
        this.remove(message.id); // Server has persisted it — remove from queue
      } catch (error) {
        console.error('[MessageQueue] Failed to send message %s:', message.id, errorMessage(error));

        // Classify the error to decide retry vs terminal disposition (spec §6.5).
        // Retryable errors fall through to markAsFailed's existing retry-counter
        // loop (3 attempts before terminal); terminal errors short-circuit.
        const classification = classifyError(error);

        if (classification.triggerRekey) {
          // REVOKED_EPOCH: the cached key is stale. Invalidate so the next
          // send attempt fetches the fresh (successor) epoch key. The send
          // itself is terminal — the user's next attempt rotates.
          e2eeService.invalidateChannelKey(message.channelId);
          this.markAsTerminallyFailed(message.id, classification.uxMessage);
        } else if (classification.retryable) {
          // NO_KEY_YET with pending=true, or other retryable error:
          // defer to the existing 3-retry backoff loop.
          this.markAsFailed(message.id, classification.uxMessage);
        } else {
          // Terminal E2EE codes (NOT_MEMBER, MALFORMED_PAYLOAD,
          // INVALID_REQUEST) and the classifyError `default:` fallback for
          // unknown future codes. Non-typed errors (network failures,
          // WebSocket disconnects, encryption-output validation) take the
          // retryable path above.
          this.markAsTerminallyFailed(message.id, classification.uxMessage);
        }
      }
    }
  }

  private generateMessageId(): string {
    return crypto.randomUUID();
  }

  private trimQueue(): void {
    if (this.queue.length > MAX_QUEUE_SIZE) {
      // Remove oldest delivered/failed messages first
      const toRemove = this.queue.length - MAX_QUEUE_SIZE;
      const delivered = this.queue
        .filter((m) => m.status === MessageStatus.DELIVERED || m.status === MessageStatus.FAILED)
        .slice(0, toRemove);

      for (const m of delivered) this.remove(m.id);

      // If still over limit, drop oldest pending. Surface the eviction loudly
      // so the chat-store's optimistic bubble flips to "failed" instead of
      // sitting in perpetual "pending" — silent drop here is a confirmed
      // failure mode: the chat-store optimistic bubble sits in perpetual 'pending' with no UI signal.
      if (this.queue.length > MAX_QUEUE_SIZE) {
        const overflow = this.queue.length - MAX_QUEUE_SIZE;
        const evicted = this.queue.slice(0, overflow);
        for (const m of evicted) {
          console.warn('[MessageQueue] Queue overflow — evicting %s (status=%s)', m.id, m.status);
          this.callbacks.onMessageFailed?.(m.id, 'Queue overflow — message dropped');
        }
        this.queue = this.queue.slice(-MAX_QUEUE_SIZE);
      }
    }
  }

  private saveQueue(): void {
    // No-op: plaintext message content must not be persisted to localStorage
    // in an E2EE application. Queued messages are transient and lost on restart.
  }

  private loadQueue(): void {
    // Clean up any previously persisted plaintext messages
    try {
      localStorage.removeItem(QUEUE_STORAGE_KEY);
    } catch {
      // Ignore
    }
  }
}

// Singleton instance
let messageQueue: MessageQueue | null = null;

export const getMessageQueue = (callbacks?: MessageQueueCallbacks): MessageQueue => {
  messageQueue ??= new MessageQueue(callbacks);
  return messageQueue;
};

export default getMessageQueue;
