import type { DMConversation } from '../stores/dmStore';

/**
 * Display name for a DM conversation header / voice surface. Moved out of
 * DMChatArea so VoiceTextChat can reuse it for the DM voice text panel (#1873).
 */
export function getThreadName(conv: DMConversation | undefined, currentUserId: string): string {
  if (!conv) return 'Conversation';
  if (conv.isPersonal) return 'Personal Thread';
  if (conv.isGroup) {
    return conv.name || conv.participants.map((p) => p.displayName || p.username).join(', ');
  }
  const other = conv.participants.find((p) => p.userId !== currentUserId);
  return other?.displayName || other?.username || 'Unknown';
}
