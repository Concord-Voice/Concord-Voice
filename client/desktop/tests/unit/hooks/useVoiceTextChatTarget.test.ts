import { describe, it, expect, beforeEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';

const mockChannelSub = vi.fn();
const mockDMSub = vi.fn();
vi.mock('@/renderer/hooks/messaging/useChannelSubscription', () => ({
  useChannelSubscription: (id: string | null) => mockChannelSub(id),
}));
vi.mock('@/renderer/hooks/messaging/useDMSubscription', () => ({
  useDMSubscription: (id: string | null) => mockDMSub(id),
}));

import {
  useVoiceTextChatTarget,
  useHasVoiceTextTarget,
} from '@/renderer/hooks/voice/useVoiceTextChatTarget';

const linkedTextChannel = {
  id: 'text-1',
  server_id: 's1',
  name: 'voice-chat',
  type: 'text' as const,
  position: 0,
  linked_voice_channel_id: 'voice-1',
  created_at: '',
  updated_at: '',
};

const dmConversation = {
  id: 'dm-1',
  isGroup: false,
  isPersonal: false,
  name: '',
  participants: [
    { userId: 'me', username: 'me' },
    { userId: 'u2', username: 'bob', displayName: 'Bob' },
  ],
};

describe('useVoiceTextChatTarget (#1873)', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useUserStore.setState({ user: { id: 'me', username: 'me' } } as never);
  });

  it('resolves the DM conversation in a DM call and subscribes via the DM path', () => {
    useVoiceStore.setState({ isDMCall: true, dmConversationId: 'dm-1', activeChannelId: 'dm-1' });
    useDMStore.setState({ conversations: [dmConversation] } as never);

    const { result } = renderHook(() => useVoiceTextChatTarget());

    expect(result.current.isDMCall).toBe(true);
    expect(result.current.targetId).toBe('dm-1');
    expect(result.current.targetName).toBe('Bob');
    expect(result.current.fetchType).toBe('dm');
    expect(result.current.ctx).toEqual({ type: 'dm', id: 'dm-1', serverId: undefined });
    expect(mockDMSub).toHaveBeenCalledWith('dm-1');
    expect(mockChannelSub).toHaveBeenCalledWith(null);
  });

  it('resolves the server linked text channel when not in a DM call', () => {
    useVoiceStore.setState({ isDMCall: false, dmConversationId: null, activeChannelId: 'voice-1' });
    useChannelStore.setState({ channels: [linkedTextChannel] } as never);
    useServerStore.setState({ activeServerId: 'server-1' } as never);

    const { result } = renderHook(() => useVoiceTextChatTarget());

    expect(result.current.isDMCall).toBe(false);
    expect(result.current.targetId).toBe('text-1');
    expect(result.current.targetName).toBe('voice-chat');
    expect(result.current.fetchType).toBe('channel');
    expect(result.current.ctx).toEqual({ type: 'voice', id: 'text-1', serverId: 'server-1' });
    expect(mockChannelSub).toHaveBeenCalledWith('text-1');
    expect(mockDMSub).toHaveBeenCalledWith(null);
  });

  it('returns a null target when a DM call has no conversation id', () => {
    useVoiceStore.setState({ isDMCall: true, dmConversationId: null });
    const { result } = renderHook(() => useVoiceTextChatTarget());
    expect(result.current.targetId).toBeNull();
    expect(result.current.targetName).toBe('Conversation'); // getThreadName(undefined)
  });
});

describe('useHasVoiceTextTarget — reactivity', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('turns true when the link arrives AFTER the first render', () => {
    // The defect this pins: the hook subscribed to `getLinkedTextChannel`, a
    // stable closure over `get().channels`, so a change to `channels` alone
    // never notified. A `channel_updated` linking a text channel mid-call, or a
    // reconnect refetch landing after the control mounted, left this false and
    // the Chat button silently never appeared.
    useVoiceStore.setState({ activeChannelId: 'voice-1', isDMCall: false, dmConversationId: null });
    useChannelStore.setState({ channels: [] });

    const { result } = renderHook(() => useHasVoiceTextTarget());
    expect(result.current).toBe(false); // precondition, not the assertion

    // NO explicit rerender(). That is the whole point: a manual rerender forces
    // a fresh read regardless of whether the store notified, so it passes just
    // as happily against a subscription that never fires. The store update has
    // to drive the re-render by itself, or nothing here is being tested.
    act(() => {
      useChannelStore.setState({ channels: [linkedTextChannel] });
    });

    expect(result.current).toBe(true);
  });

  it('stays true for a DM call regardless of the channel list', () => {
    useVoiceStore.setState({ activeChannelId: 'dm-1', isDMCall: true, dmConversationId: 'dm-1' });
    useChannelStore.setState({ channels: [] });
    const { result } = renderHook(() => useHasVoiceTextTarget());
    expect(result.current).toBe(true);
  });
});
