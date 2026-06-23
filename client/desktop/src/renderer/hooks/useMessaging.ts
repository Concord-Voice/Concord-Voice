/**
 * useMessaging - Enhanced hook for sending messages with delivery tracking
 *
 * Features:
 * - Message queue for offline mode
 * - Delivery status tracking (pending → sent → delivered)
 * - Auto-retry failed messages
 * - Optimistic UI updates
 * - Persistent queue across app restarts
 *
 * Integrates with useWebSocket and MessageQueue
 */

import { useCallback, useEffect, useRef } from 'react';
import { useChatStore } from '../stores/chatStore';
import { useUserStore } from '../stores/userStore';
import { getWebSocketService, ConnectionState } from '../services/websocketService';
import { getMessageQueue, type QueuedMessage } from '../services/messageQueue';
import { e2eeService } from '../services/e2eeService';
import { classifyError } from '../services/e2eeErrors';
import { errorMessage } from '../utils/redactError';
import type { MessageWithStatus } from '../types/chat';
import {
  sendDMMessage as sendDMMessageImpl,
  wrapContentWithGifSlug,
  type SendMessageOptions,
} from '../services/dmMessageSender';

/** Encrypt a queued message's content (with optional gif_slug) for an E2EE channel.
 *  Returns the ciphertext + key version, throws on failure. */
async function encryptQueuedContent(
  channelId: string,
  content: string,
  gifSlug: string | undefined,
  msgId: string,
  isDM: boolean
): Promise<{ ciphertext: string; keyVersion: number }> {
  if (!e2eeService.isInitialized) {
    throw new Error(
      `E2EE not initialized for encrypted ${isDM ? 'DM' : 'channel'} message ${msgId}; retry later`
    );
  }
  const plaintext = wrapContentWithGifSlug(content, gifSlug);
  const ciphertext = await e2eeService.encryptForChannel(channelId, plaintext);
  if (!ciphertext || ciphertext.length < 40) {
    throw new Error(`Encryption produced invalid output for message ${msgId}`);
  }
  return { ciphertext, keyVersion: e2eeService.getCurrentKeyVersion(channelId) };
}

export function useMessaging() {
  const wsService = useRef(getWebSocketService()).current;
  // Tracks whether the connection_ready gate warn has fired for this connection.
  // Reset to false on each CONNECTED state transition so the next timeout cycle
  // logs exactly once rather than spamming the console for v1-hub sessions or
  // any scenario where the gate stays rejected for the rest of a connection.
  const connectionReadyWarnedRef = useRef(false);
  const messageQueue = useRef(
    getMessageQueue({
      onMessageSent: (messageId) => {
        console.debug(`[useMessaging] Message ${messageId} sent`);
      },
      onMessageDelivered: (messageId, serverMessageId) => {
        console.debug(`[useMessaging] Message ${messageId} delivered (${serverMessageId})`);
      },
      onMessageFailed: (messageId, error) => {
        console.warn(`[useMessaging] Message ${messageId} failed: ${error}`);
      },
    })
  ).current;

  const updateMessageStatus = useChatStore((s) => s.updateMessageStatus);
  const addMessage = useChatStore((s) => s.addMessage);
  const userId = useUserStore((s) => s.user?.id);

  // Process a single queued message: encrypt if needed, then send via correct transport
  const processQueuedMessage = useCallback(
    async (msg: QueuedMessage) => {
      // Gate on connection-ready: during reconnect, wait for the hub to
      // acknowledge all resubscribes. On cold connect, this resolves in ~RTT.
      // On timeout (5s with no ack), proceed best-effort — the error path
      // from an unsubscribed-channel send is already handled below.
      //
      // This promise rejects only on: (a) 5s timeout, or (b) disconnect-before-ready.
      // Both are intended proceed-best-effort paths — the isSubscribed check below
      // is the authoritative gate. If additional rejection reasons are added to
      // createConnectionReadyPromise() in the future, re-evaluate whether
      // proceed-best-effort remains correct.
      try {
        await wsService.whenConnectionReady();
      } catch (err) {
        if (!connectionReadyWarnedRef.current) {
          connectionReadyWarnedRef.current = true;
          console.warn(
            `[useMessaging] connection_ready gate: ${(err as Error).message}; proceeding best-effort (further warnings suppressed for this connection)`
          );
        }
      }

      const isDM = msg.type === 'dm_message';
      const subscribed = isDM
        ? wsService.isDMSubscribed(msg.channelId)
        : wsService.isSubscribed(msg.channelId);
      if (!subscribed) {
        throw new Error(
          `Subscription not available for ${isDM ? 'DM' : 'channel'} ${msg.channelId}; retry later`
        );
      }
      const { ciphertext: content, keyVersion } = await encryptQueuedContent(
        msg.channelId,
        msg.content,
        msg.gifSlug,
        msg.id,
        isDM
      );
      const opts: Record<string, unknown> = {
        nonce: msg.id,
        keyVersion,
        mentionMeta: msg.mentionMeta,
        replyToId: msg.replyToId,
      };
      if (isDM) {
        wsService.sendDMMessage(msg.channelId, content, opts);
      } else {
        wsService.sendMessage(msg.channelId, content, opts);
      }
    },
    [wsService]
  );

  // Start/stop queue processing based on connection state.
  // Also resets the connection_ready warn flag on each fresh CONNECTED handshake
  // so the next timeout cycle logs exactly once rather than staying permanently silent.
  useEffect(() => {
    const unsubscribe = wsService.onConnectionChange((state) => {
      if (state === ConnectionState.CONNECTED) {
        connectionReadyWarnedRef.current = false;
        messageQueue.startProcessing(processQueuedMessage);
      } else {
        messageQueue.stopProcessing();
      }
    });

    return () => {
      unsubscribe();
      messageQueue.stopProcessing();
    };
  }, [wsService, messageQueue, processQueuedMessage]);

  /**
   * Send a message with delivery tracking.
   * If the channel is E2EE, encrypts before sending (fail-closed: won't send if encryption fails).
   */
  const sendMessage = useCallback(
    (
      channelId: string,
      content: string,
      username: string = 'You',
      opts?: SendMessageOptions
    ): string => {
      const {
        avatarUrl,
        displayName,
        mentionMeta,
        replyToId,
        attachmentIds,
        attachments,
        gifSlug,
      } = opts ?? {};
      // Generate client-side message ID
      const clientMessageId = messageQueue.enqueue(
        channelId,
        content,
        'message',
        mentionMeta,
        replyToId,
        gifSlug
      );

      // Build replied_to summary from store for optimistic display
      // (prevents "Original message was deleted" flash before WS ack)
      let repliedTo: MessageWithStatus['replied_to'];
      if (replyToId) {
        const storeMessages = useChatStore.getState().messagesByChannel.get(channelId);
        const target = storeMessages?.find((m) => m.id === replyToId);
        if (target) {
          repliedTo = {
            id: target.id,
            user_id: target.user_id,
            username: target.username,
            display_name: target.display_name,
            content: target.content,
          };
        }
      }

      // Optimistically add message to UI with "pending" status
      // Always show plaintext locally for the sender
      const optimisticMessage: MessageWithStatus = {
        id: clientMessageId, // Temporary ID
        clientMessageId,
        channel_id: channelId,
        user_id: userId || 'unknown',
        username,
        display_name: displayName,
        avatar_url: avatarUrl,
        content,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
        status: 'pending',
        reply_to_id: replyToId,
        replied_to: repliedTo,
        attachments,
        gif_slug: gifSlug,
      };

      addMessage(channelId, optimisticMessage);

      // If connected, send immediately (with encryption if needed)
      if (wsService.getState() === ConnectionState.CONNECTED) {
        const doSend = async () => {
          try {
            // Wrap content in JSON when gif_slug is present, then encrypt
            const plaintext = wrapContentWithGifSlug(content, gifSlug);
            // Fail-closed: if encryption fails, do NOT send
            const sendContent = await e2eeService.encryptForChannel(channelId, plaintext);
            const keyVersion = e2eeService.getCurrentKeyVersion(channelId);
            // Validate encrypted output: must be non-empty base64 with minimum size
            // (12-byte IV + 16-byte auth tag = 28 bytes minimum decoded)
            if (!sendContent || sendContent.length < 40) {
              throw new Error('Encryption produced invalid output');
            }
            wsService.sendMessage(channelId, sendContent, {
              nonce: clientMessageId,
              keyVersion,
              mentionMeta,
              replyToId,
              attachmentIds,
            });
            messageQueue.markAsSent(clientMessageId);
            messageQueue.remove(clientMessageId);
            updateMessageStatus(channelId, clientMessageId, 'sent');
          } catch (error) {
            console.error('[useMessaging] Failed to send message:', errorMessage(error));
            // Classify the error to surface code-specific UX copy (spec §6.5).
            // Falls back to generic "Unable to send message" for non-typed errors.
            const classification = classifyError(error);
            const errMsg = classification.uxMessage;
            if (classification.triggerRekey) {
              // REVOKED_EPOCH: invalidate the cached key so the next retry
              // attempt rotates to the successor epoch.
              e2eeService.invalidateChannelKey(channelId);
            }
            if (classification.retryable) {
              messageQueue.markAsFailed(clientMessageId, errMsg);
            } else {
              messageQueue.markAsTerminallyFailed(clientMessageId, errMsg);
            }
            updateMessageStatus(channelId, clientMessageId, 'failed', undefined, errMsg);
          }
        };
        doSend();
      } else {
        // Message stays in queue with "pending" status
        console.debug(`[useMessaging] Message queued (offline): ${clientMessageId}`);
      }

      return clientMessageId;
    },
    [wsService, messageQueue, addMessage, updateMessageStatus, userId]
  );

  /**
   * Send a DM message with delivery tracking. Delegates to the extracted
   * `dmMessageSender.sendDMMessage` (spec §3.0) so the same canonical send is
   * reusable outside this hook (e.g. "Send to a Friend") without mounting the
   * queue-processing lifecycle. Behavior is unchanged.
   */
  const sendDMMessage = useCallback(
    (
      conversationId: string,
      content: string,
      username: string = 'You',
      opts?: SendMessageOptions
    ): string => sendDMMessageImpl(conversationId, content, username, opts),
    []
  );

  /**
   * Mark a message as delivered (called when server acknowledges)
   */
  const markDelivered = useCallback(
    (clientMessageId: string, serverMessageId: string, channelId: string) => {
      messageQueue.markAsDelivered(clientMessageId, serverMessageId);
      updateMessageStatus(channelId, clientMessageId, 'delivered', serverMessageId);
    },
    [messageQueue, updateMessageStatus]
  );

  /**
   * Send typing indicator
   */
  const sendTyping = useCallback(
    (channelId: string, isTyping: boolean) => {
      if (wsService.getState() === ConnectionState.CONNECTED) {
        wsService.sendTypingIndicator(channelId, isTyping);
      }
    },
    [wsService]
  );

  /**
   * Get pending message count
   */
  const getPendingCount = useCallback((): number => {
    return messageQueue.size();
  }, [messageQueue]);

  /**
   * Get pending messages for a specific channel
   */
  const getPendingMessagesForChannel = useCallback(
    (channelId: string): QueuedMessage[] => {
      return messageQueue.getMessagesForChannel(channelId);
    },
    [messageQueue]
  );

  return {
    sendMessage,
    sendDMMessage,
    markDelivered,
    sendTyping,
    getPendingCount,
    getPendingMessagesForChannel,
  };
}
