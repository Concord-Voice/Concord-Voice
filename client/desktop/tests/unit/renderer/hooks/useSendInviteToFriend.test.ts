import { describe, it, expect, beforeEach, vi } from 'vitest';
import { renderHook } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useUserStore } from '@/renderer/stores/userStore';
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

  it('mints, opens the DM, and sends the canonical URL attributed to the sender (#1740)', async () => {
    const createInvite = vi.fn().mockResolvedValue({ code: 'GHJKMNPQ' });
    const openDM = vi.fn().mockResolvedValue({ id: 'dm-conv-9' });
    useInviteStore.setState({ createInvite });
    useDMStore.setState({ openDM } as Partial<ReturnType<typeof useDMStore.getState>>);
    // Sender identity must reach the optimistic bubble, else it falls back to "You".
    useUserStore.setState({
      user: {
        id: 'user-1',
        username: 'alice',
        display_name: 'Alice',
        avatar_url: '/api/v1/media/avatars/alice.png',
      },
    });

    const { result } = renderHook(() => useSendInviteToFriend('server-1'));
    const res = await result.current.send(friend);

    expect(createInvite).toHaveBeenCalledWith('server-1');
    expect(openDM).toHaveBeenCalledWith('user-2');
    // regression for #1740: the sender's own username/display_name/avatar must be passed
    // so the optimistic invite bubble is attributed to them, never the "You" fallback.
    expect(mockSend).toHaveBeenCalledWith(
      'dm-conv-9',
      'https://invite.concordvoice.chat/GHJKMNPQ',
      'alice',
      { displayName: 'Alice', avatarUrl: '/api/v1/media/avatars/alice.png' }
    );
    expect(res).toEqual({ ok: true, conversationId: 'dm-conv-9' });
  });

  it('falls back to the "You" default when no user is resolved (#1740)', async () => {
    // resetAllStores() leaves userStore.user null; the hook passes undefined for
    // username, and sendDMMessage applies its own 'You' default — a safe degrade,
    // not a throw. Locks the else-branch of `me?.username`.
    const createInvite = vi.fn().mockResolvedValue({ code: 'GHJKMNPQ' });
    const openDM = vi.fn().mockResolvedValue({ id: 'dm-conv-9' });
    useInviteStore.setState({ createInvite });
    useDMStore.setState({ openDM } as Partial<ReturnType<typeof useDMStore.getState>>);

    const { result } = renderHook(() => useSendInviteToFriend('server-1'));
    const res = await result.current.send(friend);

    expect(mockSend).toHaveBeenCalledWith(
      'dm-conv-9',
      'https://invite.concordvoice.chat/GHJKMNPQ',
      undefined,
      { displayName: undefined, avatarUrl: undefined }
    );
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
