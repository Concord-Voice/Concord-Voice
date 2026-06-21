import { describe, it, expect, beforeEach, vi } from 'vitest';
import { renderHook } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useSendInviteToFriend } from '@/renderer/hooks/useSendInviteToFriend';
import type { Friend } from '@/renderer/stores/friendStore';

const mockSend = vi.fn(() => 'client-msg-1');
vi.mock('@/renderer/services/dmMessageSender', () => ({
  sendDMMessage: (...args: unknown[]) => mockSend(...args),
}));

let mockInitialized = true;
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return mockInitialized;
    },
  },
}));

const friend: Friend = { id: 'fr-1', userId: 'user-2', username: 'bob', status: 'online' };

describe('useSendInviteToFriend', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    mockInitialized = true;
  });

  it('mints, opens the DM, and sends the canonical URL (ok)', async () => {
    const createInvite = vi.fn().mockResolvedValue({ code: 'GHJKMNPQ' });
    const openDM = vi.fn().mockResolvedValue({ id: 'dm-conv-9' });
    useInviteStore.setState({ createInvite });
    useDMStore.setState({ openDM } as Partial<ReturnType<typeof useDMStore.getState>>);

    const { result } = renderHook(() => useSendInviteToFriend('server-1'));
    const res = await result.current.send(friend);

    expect(createInvite).toHaveBeenCalledWith('server-1');
    expect(openDM).toHaveBeenCalledWith('user-2');
    expect(mockSend).toHaveBeenCalledWith('dm-conv-9', 'https://invite.example.com/GHJKMNPQ');
    expect(res).toEqual({ ok: true, conversationId: 'dm-conv-9' });
  });

  it('returns not_ready and never mints when e2ee is uninitialized', async () => {
    mockInitialized = false;
    const createInvite = vi.fn();
    useInviteStore.setState({ createInvite });

    const { result } = renderHook(() => useSendInviteToFriend('server-1'));
    const res = await result.current.send(friend);

    expect(res).toEqual({ ok: false, reason: 'not_ready' });
    expect(createInvite).not.toHaveBeenCalled();
  });

  it('returns mint_failed and never opens a DM when createInvite is null', async () => {
    const createInvite = vi.fn().mockResolvedValue(null);
    const openDM = vi.fn();
    useInviteStore.setState({ createInvite });
    useDMStore.setState({ openDM } as Partial<ReturnType<typeof useDMStore.getState>>);

    const { result } = renderHook(() => useSendInviteToFriend('server-1'));
    const res = await result.current.send(friend);

    expect(res).toEqual({ ok: false, reason: 'mint_failed' });
    expect(openDM).not.toHaveBeenCalled();
    expect(mockSend).not.toHaveBeenCalled();
  });

  it('returns dm_blocked and never sends when openDM throws', async () => {
    const createInvite = vi.fn().mockResolvedValue({ code: 'GHJKMNPQ' });
    const openDM = vi.fn().mockRejectedValue(new Error("isn't accepting DMs"));
    useInviteStore.setState({ createInvite });
    useDMStore.setState({ openDM } as Partial<ReturnType<typeof useDMStore.getState>>);

    const { result } = renderHook(() => useSendInviteToFriend('server-1'));
    const res = await result.current.send(friend);

    expect(res).toEqual({ ok: false, reason: 'dm_blocked' });
    expect(mockSend).not.toHaveBeenCalled();
  });
});
