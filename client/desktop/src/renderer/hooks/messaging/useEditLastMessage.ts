import { useCallback } from 'react';
import type { MessageWithStatus } from '../../types/chat';
import { useChatStore } from '../../stores/chat/chatStore';
import {
  canEditMessage,
  latestEditableOwnMessage,
} from '../../utils/chat/latestEditableOwnMessage';

/**
 * The composer's Up Arrow handler (#1959): open the current user's newest message for editing
 * in this chat surface, when that message can be edited. It resolves against the loaded
 * `messages` at call time and does nothing when the selector finds no editable newest message,
 * so Up in an empty composer never errors and never opens an older message.
 *
 * It also does nothing while this surface already has an edit open on a message that is still
 * editable: one edit is open at a time, and replacing it would discard that edit's unsaved
 * draft without a word. One hook shared by the three composer owners (channel chat, DM chat,
 * voice text chat), each passing its own surface id.
 */
export function useEditLastMessage(
  messages: readonly MessageWithStatus[],
  currentUserId: string,
  surfaceId: string
): () => void {
  return useCallback(() => {
    const { editingMessage, setEditingMessage } = useChatStore.getState();
    if (editingMessage?.surfaceId === surfaceId) {
      const open = messages.find((message) => message.id === editingMessage.messageId);
      if (open && canEditMessage(open, currentUserId)) return;
    }
    const id = latestEditableOwnMessage(messages, currentUserId);
    if (id) setEditingMessage(surfaceId, id);
  }, [messages, currentUserId, surfaceId]);
}
