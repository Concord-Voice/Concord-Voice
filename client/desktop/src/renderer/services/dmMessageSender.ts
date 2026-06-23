import { useChatStore } from '../stores/chatStore';
import { useUserStore } from '../stores/userStore';
import { useDMStore } from '../stores/dmStore';
import { getWebSocketService, ConnectionState } from './websocketService';
import { getMessageQueue } from './messageQueue';
import { e2eeService } from './e2eeService';
import { classifyError } from './e2eeErrors';
import { errorMessage } from '../utils/redactError';
import type { AttachmentSummary, MessageWithStatus } from '../types/chat';

export interface SendMessageOptions {
  avatarUrl?: string;
  displayName?: string;
  mentionMeta?: string;
  replyToId?: string;
  attachmentIds?: string[];
  attachments?: AttachmentSummary[];
  gifSlug?: string;
}

/** Wrap content + gif_slug into a JSON envelope for E2EE encryption when a GIF is attached. */
export function wrapContentWithGifSlug(content: string, gifSlug?: string): string {
  return gifSlug ? JSON.stringify({ text: content, gif_slug: gifSlug }) : content;
}

/**
 * Canonical E2EE DM send with optimistic UI + retry-queue, extracted from `useMessaging`
 * so it can be reused by non-component callers (e.g. the "Send to a Friend" flow) WITHOUT
 * mounting the hook's queue-processing lifecycle. Behavior is identical to the prior
 * `useMessaging.sendDMMessage`. All deps are read at call time via store getState() /
 * singleton getters. Returns the client-side message id.
 */
export function sendDMMessage(
  conversationId: string,
  content: string,
  username: string = 'You',
  opts?: SendMessageOptions
): string {
  const wsService = getWebSocketService();
  const messageQueue = getMessageQueue();
  const { addMessage, updateMessageStatus } = useChatStore.getState();
  const userId = useUserStore.getState().user?.id;

  const { avatarUrl, displayName, mentionMeta, replyToId, attachmentIds, attachments, gifSlug } =
    opts ?? {};

  // Generate client-side message ID
  const clientMessageId = messageQueue.enqueue(
    conversationId,
    content,
    'dm_message',
    mentionMeta,
    replyToId,
    gifSlug
  );

  // Build replied_to summary from store for optimistic display
  let repliedTo: MessageWithStatus['replied_to'];
  if (replyToId) {
    const storeMessages = useChatStore.getState().messagesByChannel.get(conversationId);
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
  const optimisticMessage: MessageWithStatus = {
    id: clientMessageId,
    clientMessageId,
    channel_id: conversationId,
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

  addMessage(conversationId, optimisticMessage);

  // Capture previous lastMessage for rollback if send fails
  const prevLastMessage =
    useDMStore.getState().conversations.find((c) => c.id === conversationId)?.lastMessage ?? null;

  // Optimistically bump conversation to top of DM list (#656)
  useDMStore.getState().bumpConversation(conversationId, {
    content,
    plaintextPreview: content,
    userId: userId || 'unknown',
    username,
    createdAt: optimisticMessage.created_at,
    ...(opts?.attachments?.length
      ? { attachmentType: opts.attachments[0].file_type as string }
      : {}),
  });

  // If connected, send immediately (encrypt before sending)
  if (wsService.getState() === ConnectionState.CONNECTED) {
    const doSend = async () => {
      try {
        const plaintext = wrapContentWithGifSlug(content, gifSlug);
        const sendContent = await e2eeService.encryptForChannel(conversationId, plaintext);
        const keyVersion = e2eeService.getCurrentKeyVersion(conversationId);
        if (!sendContent || sendContent.length < 40) {
          throw new Error('Encryption produced invalid output');
        }
        wsService.sendDMMessage(conversationId, sendContent, {
          nonce: clientMessageId,
          keyVersion,
          mentionMeta,
          attachmentIds,
          replyToId,
        });
        messageQueue.markAsSent(clientMessageId);
        messageQueue.remove(clientMessageId);
        updateMessageStatus(conversationId, clientMessageId, 'sent');
      } catch (error) {
        console.error('[dmMessageSender] Failed to send DM message:', errorMessage(error));
        const classification = classifyError(error);
        const errMsg = classification.uxMessage;
        if (classification.triggerRekey) {
          e2eeService.invalidateChannelKey(conversationId);
        }
        if (classification.retryable) {
          messageQueue.markAsFailed(clientMessageId, errMsg);
        } else {
          messageQueue.markAsTerminallyFailed(clientMessageId, errMsg);
        }
        updateMessageStatus(conversationId, clientMessageId, 'failed', undefined, errMsg);
        useDMStore.getState().bumpConversation(conversationId, prevLastMessage);
      }
    };
    doSend();
  } else {
    console.debug(`[dmMessageSender] DM message queued (offline): ${clientMessageId}`);
  }

  return clientMessageId;
}
