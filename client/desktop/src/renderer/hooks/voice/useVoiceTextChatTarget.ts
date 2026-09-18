import { useMemo } from 'react';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { useChannelSubscription } from '../messaging/useChannelSubscription';
import { useDMSubscription } from '../messaging/useDMSubscription';
import { getThreadName } from '../../utils/messaging/dmThreadName';
import type { ChatContext } from '../../types/chat';

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
  const conversations = useDMStore((s) => s.conversations);
  const user = useUserStore((s) => s.user);
  const activeServerId = useServerStore((s) => s.activeServerId);

  // Select the CHANNEL, not `getLinkedTextChannel`. That action is a stable
  // closure over `get().channels`, so subscribing to it never notifies — a
  // change to `channels` alone did not re-render the consumer. `find` returns a
  // reference into the existing array, so this stays referentially stable while
  // `channels` does and cannot loop.
  const linkedChannel = useChannelStore((s) =>
    !isDMCall && activeChannelId
      ? s.channels.find((c) => c.linked_voice_channel_id === activeChannelId)
      : undefined
  );
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

/**
 * Whether the active voice session has ANY text target: the DM conversation in
 * a DM call, else the linked text channel of `channelId` (defaulting to the
 * connected channel).
 *
 * Separate from `useVoiceTextChatTarget` on purpose — that hook also opens the
 * real-time subscription for whatever it resolves, so calling it from every
 * control surface just to read a boolean would open one subscription per
 * surface. Shared so the render gates cannot drift, which they already had:
 * VoiceView was DM-aware while VoiceControls and PersistentVoiceBar were not,
 * so the DM chat button never rendered and VoiceView's own correct branch was
 * unreachable (#1873 updated the panel and not its entry points).
 *
 * `channelId` is a parameter rather than a store read because VoiceView renders
 * for a SPECIFIC channel, which is not necessarily the connected one.
 */
export function useHasVoiceTextTarget(channelId?: string | null): boolean {
  const activeChannelId = useVoiceStore((s) => s.activeChannelId);
  const isDMCall = useVoiceStore((s) => s.isDMCall);
  const dmConversationId = useVoiceStore((s) => s.dmConversationId);
  const id = channelId ?? activeChannelId;

  // Subscribe to the DERIVED boolean rather than to `getLinkedTextChannel`,
  // which is a stable closure over `get().channels` and therefore never
  // notifies. The link arriving after this first rendered — a reconnect
  // refetch, or a `channel_updated` linking a text channel to the live voice
  // channel mid-call — left this `false` until some unrelated `voiceStore`
  // write forced a render, and the Chat button silently never appeared. The
  // three old call sites carried the same bug; being the one definition of
  // this gate is exactly why it is fixed here.
  const hasLinkedText = useChannelStore(
    (s) => !!id && s.channels.some((c) => c.linked_voice_channel_id === id)
  );

  // A DM's thread is the conversation itself and is always present, but the
  // panel resolves its target from dmConversationId — so gate on that rather
  // than on isDMCall alone, or the button opens an empty thread.
  if (isDMCall) return !!dmConversationId;
  return hasLinkedText;
}
