import { describe, it, expect, beforeEach, vi } from 'vitest';
import { renderHook } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useServerStore } from '@/renderer/stores/serverStore';

const mockChannelSub = vi.fn();
const mockDMSub = vi.fn();
vi.mock('@/renderer/hooks/useChannelSubscription', () => ({
  useChannelSubscription: (id: string | null) => mockChannelSub(id),
}));
vi.mock('@/renderer/hooks/useDMSubscription', () => ({
  useDMSubscription: (id: string | null) => mockDMSub(id),
}));

import { useVoiceTextChatTarget } from '@/renderer/hooks/useVoiceTextChatTarget';

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
