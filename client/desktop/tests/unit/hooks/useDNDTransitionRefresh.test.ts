import { renderHook, act, waitFor } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';
import { useDNDTransitionRefresh } from '@/renderer/hooks/useDNDTransitionRefresh';

const mockApiFetch = vi.mocked(apiFetch);
const SERVER_ID = 'server-1';
const CHANNEL_A = 'channel-a';
const CHANNEL_B = 'channel-b';

function mockUnreadResponse(
  unreads: Array<{ channel_id: string; unread_count: number }>,
  ok = true
): Response {
  return {
    ok,
    status: ok ? 200 : 500,
    json: async () => ({ unreads }),
  } as unknown as Response;
}

describe('useDNDTransitionRefresh', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    // Default: there IS an active server so the dnd→online transition has
    // something to refetch for. Tests that want the no-active-server case
    // override this explicitly.
    useServerStore.setState({ activeServerId: SERVER_ID });
  });

  it('does not fetch on initial mount (no transition yet)', () => {
    // Start in 'online' status, mount the hook, assert nothing fired.
    // The hook only fires on a status change subscription event — mounting
    // alone should not trigger a refetch.
    useMemberStore.setState({ selfStatus: 'online' });
    renderHook(() => useDNDTransitionRefresh());
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('refetches active server unread counts on dnd → online transition', async () => {
    useMemberStore.setState({ selfStatus: 'dnd' });
    renderHook(() => useDNDTransitionRefresh());

    mockApiFetch.mockResolvedValueOnce(
      mockUnreadResponse([
        { channel_id: CHANNEL_A, unread_count: 3 },
        { channel_id: CHANNEL_B, unread_count: 1 },
      ])
    );

    act(() => {
      useMemberStore.getState().setSelfStatus('online');
    });

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(`/api/v1/servers/${SERVER_ID}/unread`);
    });

    // Both channels appear with their server-provided counts.
    await waitFor(() => {
      expect(useUnreadStore.getState().unreadCounts.get(CHANNEL_A)).toBe(3);
    });
    expect(useUnreadStore.getState().unreadCounts.get(CHANNEL_B)).toBe(1);
    // Server-level unread set marked since the response had non-zero counts.
    expect(useUnreadStore.getState().serverUnreadSet.has(SERVER_ID)).toBe(true);
  });

  it('filters out the currently-active channel from refetched counts', async () => {
    // The active channel is being viewed — its unread count should NOT
    // come back from the refetch, because the user is reading it RIGHT NOW
    // and the server count is the pre-read snapshot.
    useChannelStore.setState({ activeChannelId: CHANNEL_A });
    useMemberStore.setState({ selfStatus: 'dnd' });
    renderHook(() => useDNDTransitionRefresh());

    mockApiFetch.mockResolvedValueOnce(
      mockUnreadResponse([
        { channel_id: CHANNEL_A, unread_count: 5 },
        { channel_id: CHANNEL_B, unread_count: 2 },
      ])
    );

    act(() => {
      useMemberStore.getState().setSelfStatus('online');
    });

    await waitFor(() => {
      expect(useUnreadStore.getState().unreadCounts.get(CHANNEL_B)).toBe(2);
    });
    // Active channel filtered out even though the server returned a count.
    expect(useUnreadStore.getState().unreadCounts.has(CHANNEL_A)).toBe(false);
  });

  it('clears the server-unread flag when refetched counts are all zero', async () => {
    // Pre-seed serverUnreadSet so we can verify it gets cleared.
    useUnreadStore.getState().markServerUnread(SERVER_ID);
    useMemberStore.setState({ selfStatus: 'dnd' });
    renderHook(() => useDNDTransitionRefresh());

    mockApiFetch.mockResolvedValueOnce(
      mockUnreadResponse([{ channel_id: CHANNEL_A, unread_count: 0 }])
    );

    act(() => {
      useMemberStore.getState().setSelfStatus('online');
    });

    await waitFor(() => {
      expect(useUnreadStore.getState().serverUnreadSet.has(SERVER_ID)).toBe(false);
    });
  });

  it('does NOT refetch on non-DND-exit transitions', () => {
    // dnd → dnd is not a transition (no change).
    useMemberStore.setState({ selfStatus: 'online' });
    renderHook(() => useDNDTransitionRefresh());

    act(() => {
      useMemberStore.getState().setSelfStatus('online'); // same value
    });
    expect(mockApiFetch).not.toHaveBeenCalled();

    // online → invisible is not a DND exit either.
    act(() => {
      useMemberStore.getState().setSelfStatus('invisible');
    });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('does NOT refetch when transitioning INTO dnd', () => {
    useMemberStore.setState({ selfStatus: 'online' });
    renderHook(() => useDNDTransitionRefresh());

    act(() => {
      useMemberStore.getState().setSelfStatus('dnd');
    });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('is a no-op when there is no active server', () => {
    // A user could toggle DND off while looking at the DM page (no server
    // active). The hook should not fire a fetch against `undefined`.
    useServerStore.setState({ activeServerId: null });
    useMemberStore.setState({ selfStatus: 'dnd' });
    renderHook(() => useDNDTransitionRefresh());

    act(() => {
      useMemberStore.getState().setSelfStatus('online');
    });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('swallows a failing fetch without throwing or polluting the store', async () => {
    useMemberStore.setState({ selfStatus: 'dnd' });
    renderHook(() => useDNDTransitionRefresh());

    // Simulate a network failure (rejected promise, not a 500 — covers the
    // catch arm of the chain).
    mockApiFetch.mockRejectedValueOnce(new Error('network down'));
    const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});

    act(() => {
      useMemberStore.getState().setSelfStatus('online');
    });

    await waitFor(() => {
      expect(warnSpy).toHaveBeenCalled();
    });
    // Store untouched — the failure was non-fatal.
    expect(useUnreadStore.getState().unreadCounts.size).toBe(0);

    warnSpy.mockRestore();
  });

  it('unsubscribes on unmount (no fetch after unmount)', () => {
    useMemberStore.setState({ selfStatus: 'dnd' });
    const { unmount } = renderHook(() => useDNDTransitionRefresh());

    unmount();

    // Now flip dnd → online AFTER unmount. The hook's subscription should
    // be torn down so no fetch fires. This prevents a leaked subscription
    // outliving the React tree (a real concern with manual store.subscribe
    // outside of useSyncExternalStore).
    act(() => {
      useMemberStore.getState().setSelfStatus('online');
    });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });
});
