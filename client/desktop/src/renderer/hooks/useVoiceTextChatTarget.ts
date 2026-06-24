import { useMemo } from 'react';
import { useVoiceStore } from '../stores/voiceStore';
import { useChannelStore } from '../stores/channelStore';
import { useDMStore } from '../stores/dmStore';
import { useUserStore } from '../stores/userStore';
import { useServerStore } from '../stores/serverStore';
import { useChannelSubscription } from './useChannelSubscription';
import { useDMSubscription } from './useDMSubscription';
import { getThreadName } from '../utils/dmThreadName';
import type { ChatContext } from '../types/chat';

export interface VoiceTextChatTarget {
  /** True when the active voice session is a DM call. */
  isDMCall: boolean;
  /** Conversation id (DM) or linked text-channel id (server); null when none. */
  targetId: string | null;
  /** Display name for the header / message panel. */
  targetName: string;
  /** Message-fetch transport selector. */
  fetchType: 'dm' | 'channel';
  /** Controller context routed to the correct transport + permission model. */
  ctx: ChatContext;
}

/**
 * Resolves the text-chat target for the active voice session — the DM
 * conversation in a DM call, else the server voice channel's linked text
 * channel (#1873) — and manages the real-time subscription for whichever
 * target is active.
 *
 * Extracted from VoiceTextChat to keep that component's cognitive complexity
 * within the S3776 bound: all DM-vs-server branching lives here, leaving the
 * component to render a single resolved target. The `!isDMCall` branch is the
 * original server behavior, unchanged.
 */
export function useVoiceTextChatTarget(): VoiceTextChatTarget {
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);
  const isDMCall = useVoiceStore((s) => s.isDMCall);
  const dmConversationId = useVoiceStore((s) => s.dmConversationId);
  const getLinkedTextChannel = useChannelStore((s) => s.getLinkedTextChannel);
  const conversations = useDMStore((s) => s.conversations);
  const user = useUserStore((s) => s.user);
  const activeServerId = useServerStore((s) => s.activeServerId);

  const linkedChannel =
    !isDMCall && activeChannelId ? getLinkedTextChannel(activeChannelId) : undefined;
  const dmConversation =
    isDMCall && dmConversationId ? conversations.find((c) => c.id === dmConversationId) : undefined;

  const targetId = isDMCall ? dmConversationId : (linkedChannel?.id ?? null);
  const targetName = isDMCall
    ? getThreadName(dmConversation, user?.id ?? '')
    : (linkedChannel?.name ?? '');

  // Both subscription hooks are called unconditionally (React rules-of-hooks)
  // with one nulled; each no-ops on a falsy id.
  useChannelSubscription(isDMCall ? null : targetId);
  useDMSubscription(isDMCall ? targetId : null);

  const ctx = useMemo<ChatContext>(
    () => ({
      type: isDMCall ? 'dm' : 'voice',
      id: targetId || '',
      serverId: isDMCall ? undefined : (activeServerId ?? undefined),
    }),
    [isDMCall, targetId, activeServerId]
  );

  return { isDMCall, targetId, targetName, fetchType: isDMCall ? 'dm' : 'channel', ctx };
}
