/**
 * useChatController — Unified chat operations hook (#492)
 *
 * Routes send/edit/delete/reply/pin/typing to the correct transport
 * (WebSocket method, REST endpoint) and permission model based on context type.
 *
 * Security boundaries enforced:
 * - Channel/voice: sendMessage(), /api/v1/messages/, RBAC permissions
 * - DM: sendDMMessage(), /api/v1/dm/conversations/{id}/messages/, ownership-based
 * - Typing/subscription methods never cross context boundaries
 */

import { useCallback, useEffect, useMemo } from 'react';
import { useChatStore } from '../stores/chatStore';
import { useUserStore } from '../stores/userStore';
import { usePermissionStore } from '../stores/permissionStore';
import { PIN_MESSAGES } from '../utils/permissions';
import { useMessaging } from './useMessaging';
import { pinMessage, unpinMessage } from '../services/pinService';
import { getWebSocketService, ConnectionState } from '../services/websocketService';
import { apiFetch, safeJson } from '../services/apiClient';
import { e2eeService } from '../services/e2eeService';
import type {
  ChatContext,
  ChatContextType,
  MessageWithStatus,
  AttachmentSummary,
} from '../types/chat';

export interface SendOpts {
  mentionMeta?: string;
  replyToId?: string;
  attachmentIds?: string[];
  attachments?: AttachmentSummary[];
  gifSlug?: string;
}

export function useChatController(ctx: ChatContext) {
  const messaging = useMessaging();
  const user = useUserStore((s) => s.user);
  const updateMessage = useChatStore((s) => s.updateMessage);
  const storeDeleteMessage = useChatStore((s) => s.deleteMessage);
  const replyingTo = useChatStore((s) => s.replyingTo.get(ctx.id) ?? null);
  const setReplyingTo = useChatStore((s) => s.setReplyingTo);
  const hasServerPermission = usePermissionStore((s) => s.hasServerPermission);

  // Runtime sanity: DM context should never have serverId
  useEffect(() => {
    if (ctx.type === 'dm' && ctx.serverId) {
      console.warn(
        '[useChatController] DM context should not have serverId — possible misconfiguration'
      );
    }
  }, [ctx.type, ctx.serverId]);

  // --- Context derivation ---
  const chatContext: ChatContextType = ctx.type;
  const isDM = ctx.type === 'dm';

  // --- Send ---
  const sendMessage = useCallback(
    (content: string, opts?: SendOpts) => {
      if (!ctx.id || !user) return;
      const username = user.username || 'You';
      const sendOpts = {
        avatarUrl: user.avatar_url,
        displayName: user.display_name,
        mentionMeta: opts?.mentionMeta,
        replyToId: opts?.replyToId,
        attachmentIds: opts?.attachmentIds,
        attachments: opts?.attachments,
        gifSlug: opts?.gifSlug,
      };

      if (isDM) {
        messaging.sendDMMessage(ctx.id, content, username, sendOpts);
      } else {
        messaging.sendMessage(ctx.id, content, username, sendOpts);
      }

      // Clear reply state after send
      setReplyingTo(ctx.id, null);
    },
    [ctx.id, isDM, user, messaging, setReplyingTo]
  );

  // --- Edit ---
  const editMessage = useCallback(
    async (messageId: string, newContent: string) => {
      if (!ctx.id) return;

      try {
        // Encrypt if context is E2EE
        let sendContent = newContent;
        if (e2eeService.isInitialized) {
          sendContent = await e2eeService.encryptForChannel(ctx.id, newContent);
        }

        const url = isDM
          ? `/api/v1/dm/conversations/${ctx.id}/messages/${messageId}`
          : `/api/v1/messages/${messageId}`;

        const res = await apiFetch(url, {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ content: sendContent }),
        });

        if (!res.ok) {
          const data = await safeJson<{ error?: string }>(res);
          throw new Error(data.error || 'Failed to edit message');
        }

        const data = await safeJson<{
          message: {
            content: string;
            key_version?: number;
            edited_at: string;
            updated_at?: string;
          };
        }>(res);

        updateMessage(ctx.id, messageId, {
          content: newContent,
          key_version: data.message.key_version,
          edited_at: data.message.edited_at,
          ...(data.message.updated_at && { updated_at: data.message.updated_at }),
        });
      } catch (err) {
        if (isDM) {
          console.error('Failed to edit DM message:', (err as Error).message);
        } else {
          console.error('Failed to edit message:', (err as Error).message);
        }
      }
    },
    [ctx.id, isDM, updateMessage]
  );

  // --- Delete ---
  const deleteMessage = useCallback(
    async (messageId: string) => {
      if (!ctx.id) return;

      try {
        const url = isDM
          ? `/api/v1/dm/conversations/${ctx.id}/messages/${messageId}`
          : `/api/v1/messages/${messageId}`;

        const res = await apiFetch(url, { method: 'DELETE' });

        if (!res.ok) {
          const data = await safeJson<{ error?: string }>(res);
          throw new Error(data.error || 'Failed to delete message');
        }

        storeDeleteMessage(ctx.id, messageId);
      } catch (err) {
        if (isDM) {
          console.error('Failed to delete DM message:', (err as Error).message);
        } else {
          console.error('Failed to delete message:', (err as Error).message);
        }
      }
    },
    [ctx.id, isDM, storeDeleteMessage]
  );

  // --- Reply ---
  const handleReply = useCallback(
    (msg: MessageWithStatus) => {
      setReplyingTo(ctx.id, msg);
    },
    [ctx.id, setReplyingTo]
  );

  const cancelReply = useCallback(() => {
    setReplyingTo(ctx.id, null);
  }, [ctx.id, setReplyingTo]);

  // --- Pin ---
  const canPin = useMemo(() => {
    if (isDM) return true; // DMs: ownership-based, no RBAC
    if (!ctx.serverId) return false;
    return hasServerPermission(ctx.serverId, PIN_MESSAGES);
  }, [isDM, ctx.serverId, hasServerPermission]);

  const handlePinToggle = useCallback(async (msg: MessageWithStatus) => {
    try {
      if (msg.pinned_at) {
        await unpinMessage(msg.id);
      } else {
        await pinMessage(msg.id);
      }
    } catch (err) {
      console.error('Failed to toggle pin:', (err as Error).message);
    }
  }, []);

  // --- Typing ---
  const sendTyping = useCallback(
    (isTyping: boolean) => {
      const ws = getWebSocketService();
      if (ws?.getState() !== ConnectionState.CONNECTED) return;

      if (isDM) {
        ws.sendDMTypingIndicator(ctx.id, isTyping);
      } else {
        ws.sendTypingIndicator(ctx.id, isTyping);
      }
    },
    [isDM, ctx.id]
  );

  return {
    // Message operations
    sendMessage,
    editMessage,
    deleteMessage,
    // Reply
    replyingTo,
    handleReply,
    cancelReply,
    // Pin
    canPin,
    handlePinToggle,
    // Typing
    sendTyping,
    // Context
    chatContext,
  };
}
