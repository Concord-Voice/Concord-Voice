import { act, renderHook, waitFor } from '@testing-library/react';
import { StrictMode } from 'react';
import { afterAll, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { deferred } from '../../helpers/deferred';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockUser } from '../../mocks/fixtures';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useDMStore, type DMConversation } from '@/renderer/stores/chat/dmStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { Permissions } from '@/renderer/utils/policy/permissions';
import { useExpirationPolicy } from '@/renderer/hooks/messaging/useExpirationPolicy';

const API = 'http://localhost:8080';
const policy = {
  window_seconds: 86400,
  updated_at: '2026-09-08T05:00:00Z',
  revision: 4,
  backfill_pending: false,
};
const channelRow = {
  ...mockChannel,
  ...{
    expiration_window_seconds: policy.window_seconds,
    expiration_updated_at: policy.updated_at,
    expiration_revision: policy.revision,
    expiration_backfill_pending: policy.backfill_pending,
  },
};
type DMRow = {
  id: string;
  is_group: boolean;
  is_personal: boolean;
  name: string;
  participants: Array<{ user_id: string; username: string; role: 'admin' | 'member' }>;
  last_message: null;
  unread_count: number;
  created_at: string;
  expiration_window_seconds: number;
  expiration_updated_at: string;
  expiration_revision: number;
  expiration_backfill_pending: boolean;
};

const dmRow = (overrides: Partial<DMRow> = {}): DMRow => ({
  id: 'dm-1',
  is_group: false,
  is_personal: false,
  name: 'Alex',
  participants: [
    { user_id: mockUser.id, username: mockUser.username, role: 'member' },
    { user_id: 'user-2', username: 'alex', role: 'member' },
  ],
  last_message: null,
  unread_count: 0,
  created_at: policy.updated_at,
  expiration_window_seconds: policy.window_seconds,
  expiration_updated_at: policy.updated_at,
  expiration_revision: policy.revision,
  expiration_backfill_pending: policy.backfill_pending,
  ...overrides,
});

const personalConversation = {
  id: 'personal-1',
  isGroup: false,
  isPersonal: true,
  name: 'Saved Messages',
  participants: [{ userId: mockUser.id, username: mockUser.username, role: 'member' as const }],
  lastMessage: null,
  unreadCount: 0,
  createdAt: policy.updated_at,
} satisfies DMConversation;

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useAuthStore.getState().setSessionId('session-1');
  useUserStore.getState().setUser({ id: mockUser.id, username: mockUser.username });
  server.resetHandlers();
});

describe('useExpirationPolicy', () => {
  it.each([
    ['null scope', null, false],
    ['missing account', { kind: 'dm', id: 'dm-1' }, false],
  ])('%s performs no read and cannot edit', async (_label, scope, expected) => {
    if (_label === 'missing account') {
      useUserStore.getState().clearUser();
    }
    const get = vi.fn(() => HttpResponse.json({ conversations: [dmRow()] }));
    server.use(http.get(`${API}/api/v1/dm/conversations`, get));
    const { result } = renderHook(() => useExpirationPolicy(scope as never));
    expect(result.current.policyState).toBe('unavailable');
    expect(get).not.toHaveBeenCalled();
    expect(result.current.canEdit).toBe(expected);
    expect(result.current.policy).toBeNull();
  });

  it('excludes a seeded personal conversation without reading it', async () => {
    useDMStore.setState({ conversations: [personalConversation] });
    const get = vi.fn(() => HttpResponse.json({ conversations: [dmRow()] }));
    server.use(http.get(`${API}/api/v1/dm/conversations`, get));
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'dm', id: personalConversation.id })
    );
    expect(result.current.policyState).toBe('unavailable');
    expect(get).not.toHaveBeenCalled();
    expect(result.current.canEdit).toBe(false);
  });

  it('reads a channel policy from the exact channel list and writes a fresh policy after PATCH', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    let body: unknown;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [channelRow] })
      ),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ ...policy, revision: 5, window_seconds: 2592000 });
      })
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policy?.revision).toBe(4));
    expect(result.current.canEdit).toBe(true);
    await act(async () => {
      await expect(
        result.current.onApplyPolicy({
          mode: 'set',
          window_seconds: 2592000,
          retroactive: 'new_only',
        })
      ).resolves.toMatchObject({ kind: 'ok' });
    });
    expect(body).toEqual({ mode: 'set', window_seconds: 2592000, retroactive: 'new_only' });
    await waitFor(() => expect(result.current.policy?.windowSeconds).toBe(2592000));
  });

  it('refreshes the active policy after connection recovery', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    let reads = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        return HttpResponse.json({
          channels: [
            {
              ...channelRow,
              expiration_window_seconds: reads === 1 ? 86400 : 2592000,
              expiration_revision: reads === 1 ? 4 : 5,
            },
          ],
        });
      })
    );
    const { result, unmount } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    try {
      await waitFor(() => expect(result.current.policy?.revision).toBe(4));
      act(() => globalThis.dispatchEvent(new CustomEvent('connection-recovered')));
      await waitFor(() => expect(result.current.policy?.revision).toBe(5));
      expect(result.current.policy?.windowSeconds).toBe(2592000);
    } finally {
      unmount();
    }
  });

  it('reports malformed target policy as unavailable even when a valid cached policy exists', async () => {
    useChannelStore.setState({
      currentServerId: 'server-1',
      channels: [
        {
          ...mockChannel,
          expirationPolicy: {
            windowSeconds: 86400,
            updatedAt: policy.updated_at,
            revision: 4,
            backfillPending: false,
          },
        },
      ],
    });
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({
          channels: [
            {
              ...mockChannel,
              expiration_window_seconds: 300,
              expiration_updated_at: policy.updated_at,
              expiration_revision: 5,
              expiration_backfill_pending: false,
            },
          ],
        })
      )
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policyState).toBe('unavailable'));
    expect(result.current.policy).toBeNull();
  });

  it('invalidates a ready policy after a malformed background channel refresh', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    let malformed = false;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({
          channels: [
            malformed
              ? { ...channelRow, expiration_window_seconds: 300, expiration_revision: 5 }
              : channelRow,
          ],
        })
      )
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policyState).toBe('ready'));

    malformed = true;
    await act(async () => {
      await useChannelStore.getState().fetchChannels('server-1');
    });

    expect(result.current.policyState).toBe('unavailable');
    expect(result.current.policy).toBeNull();
  });

  it('keeps a policy mutation current while a recovery read overlaps it', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    const patchStarted = deferred<void>();
    const patchResponse = deferred<Response>();
    let reads = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        return HttpResponse.json({ channels: [channelRow] });
      }),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, () => {
        patchStarted.resolve();
        return patchResponse.promise;
      })
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policy?.revision).toBe(4));

    const apply = result.current.onApplyPolicy({
      mode: 'set',
      window_seconds: 2592000,
      retroactive: 'new_only',
    });
    await patchStarted.promise;
    act(() => globalThis.dispatchEvent(new CustomEvent('connection-recovered')));
    await waitFor(() => expect(reads).toBe(2));
    patchResponse.resolve(HttpResponse.json({ ...policy, revision: 5, window_seconds: 2592000 }));

    await expect(apply).resolves.toMatchObject({ kind: 'ok' });
    await waitFor(() => expect(result.current.policy?.revision).toBe(5));
  });

  it('does not acknowledge a conflict as a successful setter', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [channelRow] })
      ),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json(
          {
            ...policy,
            revision: 5,
            window_seconds: 2592000,
          },
          { status: 409 }
        )
      )
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policy?.revision).toBe(4));
    await act(async () => {
      await result.current.onApplyPolicy({
        mode: 'set',
        window_seconds: 2592000,
        retroactive: 'apply',
      });
    });
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]).toEqual({
      'channel-1': 4,
    });
  });

  it('rejects a session-expired mutation without changing policy or seen marker', async () => {
    useChannelStore.setState({
      currentServerId: 'server-1',
      seenExpirationRevisionsByAccount: { [mockUser.id]: { 'channel-1': 3 } },
    });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    let patchCount = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [channelRow] })
      ),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, () => {
        patchCount += 1;
        return HttpResponse.json({ error: 'session expired' }, { status: 401 });
      })
    );
    const originalElectron = globalThis.electron;
    const refreshToken = vi.fn().mockResolvedValue({
      status: 'ok',
      accessToken: 'refreshed-token',
      sessionId: 'session-1',
      previousSessionId: 'session-1',
    });
    globalThis.electron = {
      ...(originalElectron ?? {}),
      refreshToken,
    };
    const { result, unmount } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    try {
      await waitFor(() => expect(result.current.policy?.revision).toBe(4));
      await expect(
        result.current.onApplyPolicy({
          mode: 'set',
          window_seconds: 2592000,
          retroactive: 'apply',
        })
      ).resolves.toEqual({ kind: 'rejected', reason: 'sessionExpired' });
      expect(patchCount).toBe(2);
      expect(refreshToken).toHaveBeenCalledOnce();
      expect(result.current.policy?.windowSeconds).toBe(86400);
      expect(
        useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]?.['channel-1']
      ).toBe(3);
    } finally {
      unmount();
      globalThis.electron = originalElectron;
    }
  });

  it('uses a channel override before server fallback and reports a missing DM as locked', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({ serverPermissions: { 'server-1': Permissions.MANAGE_CHANNELS } });
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [channelRow] })
      )
    );
    const channel = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(channel.result.current.policy?.revision).toBe(4));
    expect(channel.result.current.canEdit).toBe(true);
    act(() => usePermissionStore.setState({ channelPermissions: { 'channel-1': 0n } }));
    expect(channel.result.current.canEdit).toBe(false);
    act(() => usePermissionStore.setState({ channelPermissions: {} }));
    await waitFor(() => expect(channel.result.current.canEdit).toBe(true));
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () => HttpResponse.json({ conversations: [] }))
    );
    const dm = renderHook(() => useExpirationPolicy({ kind: 'dm', id: 'dm-1' }));
    await waitFor(() => expect(dm.result.current.policyState).toBe('unavailable'));
    expect(dm.result.current.canEdit).toBe(false);
    dm.unmount();
    channel.unmount();
  });

  it('marks unavailable data unavailable instead of selecting Off, and fences account changes', async () => {
    const started = deferred<void>();
    const pending = deferred<Response>();
    let reads = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        started.resolve();
        if (reads <= 2) return pending.promise;
        return HttpResponse.json({ channels: [{ ...channelRow, expiration_revision: 7 }] });
      })
    );
    useChannelStore.setState({ currentServerId: 'server-1' });
    const { result, unmount } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    try {
      await started.promise;
      const oldRead = result.current.onRefresh();
      act(() => useAuthStore.getState().beginAuthLifecycle('successor-token', 'session-2'));
      useUserStore.getState().setUser({ id: 'user-2', username: mockUser.username });
      pending.resolve(HttpResponse.json({ channels: [channelRow] }));
      await expect(oldRead).resolves.toEqual({ kind: 'superseded' });
      const currentRead = result.current.onRefresh();
      await waitFor(() => expect(reads).toBeGreaterThan(2), { timeout: 1000 });
      await expect(currentRead).resolves.toMatchObject({ kind: 'fresh' });
      await waitFor(() =>
        expect(useChannelStore.getState().channels[0]?.expirationPolicy?.revision).toBe(7)
      );
      expect(
        useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]
      ).toBeUndefined();
      expect(useChannelStore.getState().seenExpirationRevisionsByAccount['user-2']).toEqual({
        'channel-1': 7,
      });
    } finally {
      pending.resolve(HttpResponse.json({ channels: [] }));
      unmount();
    }
  });

  it('keeps a same-account read current when credentials rotate', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    const started = deferred<void>();
    const response = deferred<Response>();
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        started.resolve();
        return response.promise;
      })
    );
    const generation = useAuthStore.getState().authGeneration;
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    try {
      await started.promise;
      act(() =>
        useAuthStore.getState().rotateAuthCredentials(generation, 'rotated-token', 'session-2')
      );
      response.resolve(HttpResponse.json({ channels: [channelRow] }));
      await waitFor(() => expect(result.current.policy?.revision).toBe(4));
    } finally {
      response.resolve(HttpResponse.json({ channels: [channelRow] }));
    }
  });

  it('rejects retained callbacks from the prior auth generation for the same account', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    let reads = 0;
    let patches = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        return HttpResponse.json({
          channels: [
            {
              ...channelRow,
              expiration_revision: reads === 1 ? 4 : 5,
              expiration_window_seconds: reads === 1 ? 86400 : 2592000,
            },
          ],
        });
      }),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, () => {
        patches += 1;
        return HttpResponse.json({ ...policy, revision: 6, window_seconds: 3600 });
      })
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policy?.revision).toBe(4));
    const retained = {
      refresh: result.current.onRefresh,
      apply: result.current.onApplyPolicy,
      markSeen: result.current.onMarkSeen,
    };
    act(() => useAuthStore.getState().beginAuthLifecycle('successor-token', 'session-2'));
    await waitFor(() => expect(result.current.policy?.revision).toBe(5));
    const readsAfterSuccessor = reads;
    const markerBeforeRetainedCallbacks =
      useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]?.['channel-1'];
    await expect(retained.refresh()).resolves.toEqual({ kind: 'superseded' });
    await expect(
      retained.apply({ mode: 'set', window_seconds: 3600, retroactive: 'new_only' })
    ).resolves.toEqual({ kind: 'rejected', reason: 'unavailable' });
    retained.markSeen(5);
    expect(reads).toBe(readsAfterSuccessor);
    expect(patches).toBe(0);
    expect(
      useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]?.['channel-1']
    ).toBe(markerBeforeRetainedCallbacks);
  });

  it('keeps a newer mutation and seen marker when an older channel read completes late', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    const started = deferred<void>();
    const readResponse = deferred<Response>();
    let reads = 0;
    let patches = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        if (reads === 1) {
          started.resolve();
          return HttpResponse.json({ channels: [channelRow] });
        }
        return readResponse.promise;
      }),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, () => {
        patches += 1;
        return HttpResponse.json({ ...policy, revision: 5, window_seconds: 2592000 });
      })
    );
    const { result, unmount } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    try {
      await started.promise;
      await waitFor(() => expect(result.current.policy?.revision).toBe(4));
      const oldRead = result.current.onRefresh();
      await waitFor(() => expect(reads).toBe(2));
      await act(async () => {
        await expect(
          result.current.onApplyPolicy({
            mode: 'set',
            window_seconds: 2592000,
            retroactive: 'new_only',
          })
        ).resolves.toMatchObject({ kind: 'ok' });
      });
      expect(patches).toBe(1);
      await waitFor(() => expect(result.current.policy?.revision).toBe(5));
      expect(result.current.policyState).toBe('ready');
      expect(useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]).toEqual({
        'channel-1': 5,
      });
      readResponse.resolve(HttpResponse.json({ channels: [channelRow] }));
      await expect(oldRead).resolves.toEqual({
        kind: 'fresh',
        policy: expect.objectContaining({ revision: 5, windowSeconds: 2592000 }),
      });
      expect(result.current.policy?.revision).toBe(5);
      expect(result.current.policyState).toBe('ready');
      expect(result.current.policy?.windowSeconds).toBe(2592000);
      expect(useChannelStore.getState().seenExpirationRevisionsByAccount[mockUser.id]).toEqual({
        'channel-1': 5,
      });
    } finally {
      readResponse.resolve(HttpResponse.json({ channels: [channelRow] }));
      unmount();
    }
  });

  it('rearms its read lifecycle under StrictMode and settles the current policy', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    let reads = 0;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        return HttpResponse.json({ channels: [channelRow] });
      })
    );
    const { result } = renderHook(
      () => useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1'),
      { wrapper: StrictMode }
    );
    await waitFor(() => expect(result.current.policy?.revision).toBe(4));
    expect(result.current.policyState).toBe('ready');
    expect(reads).toBeGreaterThan(0);
  });

  it('does not fetch or mutate for personal, nontext, or closed contexts', async () => {
    let reads = 0;
    useChannelStore.setState({ channels: [{ ...mockChannel, type: 'voice' }] });
    useDMStore.setState({ conversations: [personalConversation] });
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () => {
        reads += 1;
        return HttpResponse.json({ channels: [channelRow] });
      }),
      http.get(`${API}/api/v1/dm/conversations`, () => {
        reads += 1;
        return HttpResponse.json({ conversations: [dmRow()] });
      })
    );
    const personal = renderHook(() =>
      useExpirationPolicy({ kind: 'dm', id: personalConversation.id })
    );
    const nontext = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    const closed = renderHook(() => useExpirationPolicy(null));
    await waitFor(() => expect(personal.result.current.policyState).toBe('unavailable'));
    await waitFor(() => expect(nontext.result.current.policyState).toBe('unavailable'));
    expect(closed.result.current.policyState).toBe('unavailable');
    expect(personal.result.current.canEdit).toBe(false);
    expect(nontext.result.current.canEdit).toBe(false);
    expect(reads).toBe(0);
    await expect(personal.result.current.onRefresh()).resolves.toEqual({ kind: 'superseded' });
    await expect(closed.result.current.onRefresh()).resolves.toEqual({ kind: 'superseded' });
    await expect(
      personal.result.current.onApplyPolicy({ mode: 'resume', revision: 4 })
    ).resolves.toMatchObject({ kind: 'rejected', reason: 'unavailable' });
    personal.result.current.onMarkSeen(4);
    nontext.result.current.onMarkSeen(4);
    closed.result.current.onMarkSeen(4);
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({});
    personal.unmount();
    nontext.unmount();
    closed.unmount();
  });

  it('does not issue a read from a callback retained after unmount', async () => {
    const get = vi.fn(() => HttpResponse.json({ channels: [channelRow] }));
    server.use(http.get(`${API}/api/v1/servers/server-1/channels`, get));
    useChannelStore.setState({ currentServerId: 'server-1' });
    const view = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(view.result.current.policy?.revision).toBe(4));
    const oldRefresh = view.result.current.onRefresh;
    view.unmount();
    const before = get.mock.calls.length;
    await oldRefresh();
    expect(get).toHaveBeenCalledTimes(before);
  });

  it('uses real group and direct-message membership when deciding editability', async () => {
    const group = dmRow({
      id: 'group-1',
      is_group: true,
      participants: [
        { user_id: mockUser.id, username: mockUser.username, role: 'admin' },
        { user_id: 'user-2', username: 'alex', role: 'member' },
      ],
    });
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () =>
        HttpResponse.json({ conversations: [group, dmRow()] })
      )
    );
    const admin = renderHook(() => useExpirationPolicy({ kind: 'dm', id: 'group-1' }));
    await waitFor(() => expect(admin.result.current.policy?.revision).toBe(4));
    expect(admin.result.current.canEdit).toBe(true);
    const directMember = renderHook(() => useExpirationPolicy({ kind: 'dm', id: 'dm-1' }));
    await waitFor(() => expect(directMember.result.current.policy?.revision).toBe(4));
    expect(directMember.result.current.canEdit).toBe(true);
    directMember.unmount();
    act(() => useUserStore.getState().setUser({ id: 'user-2', username: mockUser.username }));
    await waitFor(() => expect(admin.result.current.canEdit).toBe(false));
    admin.unmount();
    useUserStore.getState().setUser({ id: 'user-3', username: mockUser.username });
    const nonmember = renderHook(() => useExpirationPolicy({ kind: 'dm', id: 'dm-1' }));
    await waitFor(() => expect(nonmember.result.current.policy?.revision).toBe(4));
    expect(nonmember.result.current.canEdit).toBe(false);
    nonmember.unmount();
  });

  it('waits for the requested DM GET before returning a fresh policy', async () => {
    const started = deferred<void>();
    const initialResponse = deferred<Response>();
    const trailingResponse = deferred<Response>();
    let reads = 0;
    server.use(
      http.get(`${API}/api/v1/dm/conversations`, () => {
        reads += 1;
        if (reads === 1) {
          started.resolve();
          return initialResponse.promise;
        }
        return trailingResponse.promise;
      })
    );
    const { result, unmount } = renderHook(() => useExpirationPolicy({ kind: 'dm', id: 'dm-1' }));
    try {
      await started.promise;
      const read = result.current.onRefresh();
      await waitFor(() => expect(reads).toBe(1));
      initialResponse.resolve(HttpResponse.json({ conversations: [dmRow()] }));
      await waitFor(() => expect(reads).toBe(2));
      trailingResponse.resolve(
        HttpResponse.json({
          conversations: [
            dmRow({
              expiration_window_seconds: 2592000,
              expiration_revision: 5,
            }),
          ],
        })
      );
      await expect(read).resolves.toEqual({
        kind: 'fresh',
        policy: expect.objectContaining({ revision: 5, windowSeconds: 2592000 }),
      });
      expect(reads).toBe(2);
      expect(useDMStore.getState().conversations[0]?.expirationPolicy).toMatchObject({
        revision: 5,
        windowSeconds: 2592000,
      });
    } finally {
      initialResponse.resolve(HttpResponse.json({ conversations: [dmRow()] }));
      trailingResponse.resolve(
        HttpResponse.json({ conversations: [dmRow({ expiration_revision: 5 })] })
      );
      unmount();
    }
  });

  it('keeps a successful policy update visible when acknowledgement storage fails', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { 'channel-1': Permissions.MANAGE_CHANNELS },
    });
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [channelRow] })
      ),
      http.patch(`${API}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({ ...policy, revision: 5, window_seconds: 2592000 })
      )
    );
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    try {
      const { result } = renderHook(() =>
        useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
      );
      await waitFor(() => expect(result.current.policy?.revision).toBe(4));
      await act(async () => {
        await expect(
          result.current.onApplyPolicy({
            mode: 'set',
            window_seconds: 2592000,
            retroactive: 'new_only',
          })
        ).resolves.toMatchObject({ kind: 'ok' });
      });
      await waitFor(() => expect(result.current.policy?.windowSeconds).toBe(2592000));
    } finally {
      setItem.mockRestore();
    }
  });

  it('dismisses and persists a changed-revision notice per account', async () => {
    useChannelStore.setState({ currentServerId: 'server-1' });
    let row = channelRow;
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [row] })
      )
    );
    const { result } = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    await waitFor(() => expect(result.current.policy?.revision).toBe(4));
    expect(result.current.showChangedNotice).toBe(false);
    row = { ...channelRow, expiration_revision: 5, expiration_updated_at: '2026-09-08T06:00:00Z' };
    await act(async () => {
      await result.current.onRefresh();
    });
    await waitFor(() => expect(result.current.policy?.revision).toBe(5));
    expect(result.current.showChangedNotice).toBe(true);
    act(() => result.current.onDismissNotice());
    expect(result.current.showChangedNotice).toBe(false);
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({
      [mockUser.id]: { 'channel-1': 5 },
    });
  });

  it('rehydrates the same-account marker for notice state and isolates another account', async () => {
    localStorage.setItem(
      'concord-channels',
      JSON.stringify({
        state: {
          seenExpirationRevisionsByAccount: { [mockUser.id]: { 'channel-1': 4 } },
        },
        version: 0,
      })
    );
    await useChannelStore.persist.rehydrate();
    useChannelStore.setState({ currentServerId: 'server-1' });
    const row = { ...channelRow, expiration_revision: 5 };
    server.use(
      http.get(`${API}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [row] })
      )
    );
    const view = renderHook(() =>
      useExpirationPolicy({ kind: 'channel', id: 'channel-1' }, 'server-1')
    );
    try {
      await waitFor(() => expect(view.result.current.policy?.revision).toBe(5));
      expect(view.result.current.showChangedNotice).toBe(true);
      act(() => view.result.current.onDismissNotice());
      expect(view.result.current.showChangedNotice).toBe(false);
      act(() =>
        useUserStore.getState().setUser({ id: 'different-account', username: mockUser.username })
      );
      await waitFor(() => expect(view.result.current.policyState).toBe('ready'));
      await waitFor(() =>
        expect(
          useChannelStore.getState().seenExpirationRevisionsByAccount['different-account']?.[
            'channel-1'
          ]
        ).toBe(5)
      );
    } finally {
      view.unmount();
    }

    localStorage.setItem(
      'concord-channels',
      JSON.stringify({
        state: { seenExpirationRevisionsByAccount: { bad: { 'channel-1': 'five' } } },
        version: 0,
      })
    );
    await useChannelStore.persist.rehydrate();
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({});
  });
});
