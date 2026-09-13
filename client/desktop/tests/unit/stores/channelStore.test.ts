import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { clearIndex, indexMessage, isIndexed } from '@/renderer/services/messaging/searchService';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockEncryptedChannel, mockMessage } from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import { deferred } from '../../helpers/deferred';
import { captureAuthLifecycle } from '@/renderer/services/system/postLoginHydrationLifecycle';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => {
  vi.restoreAllMocks();
  server.resetHandlers();
});

describe('channelStore', () => {
  beforeEach(() => {
    resetAllStores();
    clearIndex();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  describe('addChannel', () => {
    it('adds a channel', () => {
      useChannelStore.getState().addChannel(mockChannel);
      expect(useChannelStore.getState().channels).toHaveLength(1);
      expect(useChannelStore.getState().channels[0].name).toBe('general');
      expect(useChannelStore.getState().channelIdsByServer['server-1']).toEqual(['channel-1']);
    });
  });

  describe('updateChannel', () => {
    it('updates channel properties', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().updateChannel('channel-1', { name: 'renamed' });
      expect(useChannelStore.getState().channels[0].name).toBe('renamed');
    });

    it('does not affect other channels', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      useChannelStore.getState().updateChannel('channel-1', { name: 'renamed' });
      expect(useChannelStore.getState().channels[1].name).toBe('encrypted-chat');
    });
  });

  describe('removeChannel', () => {
    it('removes a channel', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      useChannelStore.getState().removeChannel('channel-1');
      expect(useChannelStore.getState().channels).toHaveLength(1);
      expect(useChannelStore.getState().channels[0].id).toBe('channel-2');
      expect(useChannelStore.getState().channelIdsByServer['server-1']).toEqual(['channel-2']);
    });

    it('cascades to clear messages and unreads', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useUnreadStore.getState().setUnreadCount('channel-1', 3);
      useChannelStore.getState().removeChannel('channel-1');
      expect(useChatStore.getState().messagesByChannel.has('channel-1')).toBe(false);
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
    });

    it('removes only the deleted channel from the in-memory search index', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      indexMessage('removed-channel-message', 'revoked plaintext', 'channel-1');
      indexMessage('retained-channel-message', 'retained plaintext', 'channel-2');

      useChannelStore.getState().removeChannel('channel-1');

      expect(isIndexed('removed-channel-message')).toBe(false);
      expect(isIndexed('retained-channel-message')).toBe(true);
    });

    it('invalidates the E2EE generation before removing the search scope', () => {
      useChannelStore.getState().addChannel(mockChannel);
      indexMessage('generation-order-message', 'revoked plaintext', 'channel-1');
      const searchWasPresentAtInvalidation: boolean[] = [];
      const invalidateSpy = vi
        .spyOn(e2eeService, 'invalidateChannelKey')
        .mockImplementation(() =>
          searchWasPresentAtInvalidation.push(isIndexed('generation-order-message'))
        );

      useChannelStore.getState().removeChannel('channel-1');

      expect(invalidateSpy).toHaveBeenCalledOnce();
      expect(invalidateSpy).toHaveBeenCalledWith('channel-1');
      expect(searchWasPresentAtInvalidation).toEqual([true]);
      expect(isIndexed('generation-order-message')).toBe(false);
    });

    it('clears activeChannelId if the active channel is removed', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().setActiveChannel('channel-1');
      useChannelStore.getState().removeChannel('channel-1');
      expect(useChannelStore.getState().activeChannelId).toBeNull();
    });

    it('preserves activeChannelId when a different channel is removed', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      useChannelStore.getState().setActiveChannel('channel-1');
      useChannelStore.getState().removeChannel('channel-2');
      expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    });

    it('cleans up lastChannelByServer references', () => {
      useChannelStore.setState({
        lastChannelByServer: { 'server-1': 'channel-1', 'server-2': 'channel-2' },
      });
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().removeChannel('channel-1');
      expect(useChannelStore.getState().lastChannelByServer['server-1']).toBeUndefined();
      expect(useChannelStore.getState().lastChannelByServer['server-2']).toBe('channel-2');
    });
  });

  describe('setActiveChannel', () => {
    it('sets active channel', () => {
      useChannelStore.getState().setActiveChannel('channel-1');
      expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    });

    it('clears active channel with null', () => {
      useChannelStore.getState().setActiveChannel('channel-1');
      useChannelStore.getState().setActiveChannel(null);
      expect(useChannelStore.getState().activeChannelId).toBeNull();
    });

    it('tracks last-viewed channel per server when currentServerId is set', () => {
      useChannelStore.setState({ currentServerId: 'server-1' });
      useChannelStore.getState().setActiveChannel('channel-1');
      expect(useChannelStore.getState().lastChannelByServer['server-1']).toBe('channel-1');
    });

    it('does not set lastChannelByServer when currentServerId is null', () => {
      useChannelStore.setState({ currentServerId: null, lastChannelByServer: {} });
      useChannelStore.getState().setActiveChannel('channel-1');
      expect(Object.keys(useChannelStore.getState().lastChannelByServer)).toHaveLength(0);
    });
  });

  describe('fetchChannels', () => {
    it('fetches channels from API and sets first text channel as active', async () => {
      await useChannelStore.getState().fetchChannels('server-1');
      const state = useChannelStore.getState();
      expect(state.channels).toHaveLength(1);
      expect(state.channels[0].name).toBe('general');
      expect(state.activeChannelId).toBe('channel-1');
      expect(state.isLoading).toBe(false);
    });

    it('sets error on fetch failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () =>
          HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
        )
      );
      await useChannelStore.getState().fetchChannels('server-1');
      expect(useChannelStore.getState().error).toBe('Forbidden');
      expect(useChannelStore.getState().isLoading).toBe(false);
    });

    it('saves current channel when switching servers', async () => {
      // Simulate being on server-1 with channel-1 active
      useChannelStore.setState({
        currentServerId: 'server-1',
        activeChannelId: 'channel-1',
      });

      // Switch to server-2
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-2/channels`, () =>
          HttpResponse.json({
            channels: [{ ...mockChannel, id: 'ch-2', server_id: 'server-2', name: 'lobby' }],
          })
        )
      );
      await useChannelStore.getState().fetchChannels('server-2');

      expect(useChannelStore.getState().lastChannelByServer['server-1']).toBe('channel-1');
      expect(useChannelStore.getState().currentServerId).toBe('server-2');
    });

    it('restores last-viewed channel when returning to a server', async () => {
      useChannelStore.setState({
        lastChannelByServer: { 'server-1': 'channel-1' },
      });

      await useChannelStore.getState().fetchChannels('server-1');
      expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    });

    it('falls back to first text channel if lastChannel is gone', async () => {
      useChannelStore.setState({
        lastChannelByServer: { 'server-1': 'nonexistent-channel' },
      });

      await useChannelStore.getState().fetchChannels('server-1');
      // Should pick first text channel from the API response
      expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    });

    it('handles empty channel list', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () =>
          HttpResponse.json({ channels: [] })
        )
      );
      await useChannelStore.getState().fetchChannels('server-1');
      expect(useChannelStore.getState().channels).toHaveLength(0);
      expect(useChannelStore.getState().activeChannelId).toBeNull();
    });

    it('does not clear activeChannelId when re-fetching same server', async () => {
      useChannelStore.setState({ currentServerId: 'server-1', activeChannelId: 'channel-1' });
      await useChannelStore.getState().fetchChannels('server-1');
      // Should not have cleared activeChannelId before fetching
      expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    });

    it('ignores malformed channel rows without purging cached channel state', async () => {
      useChannelStore.setState({
        channels: [mockChannel, mockEncryptedChannel],
        channelIdsByServer: { 'server-1': ['channel-1', 'channel-2'] },
        currentServerId: 'server-1',
        activeChannelId: 'channel-1',
      });
      useChatStore.getState().addMessage('channel-2', {
        ...mockMessage,
        id: 'retained-channel-message',
        channel_id: 'channel-2',
      });
      useUnreadStore.getState().setUnreadCount('channel-2', 3);
      indexMessage('retained-channel-message', 'retained plaintext', 'channel-2');
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () =>
          HttpResponse.json({ channels: [{}] })
        )
      );

      await useChannelStore.getState().fetchChannels('server-1');

      expect(useChannelStore.getState().channelIdsByServer['server-1']).toEqual([
        'channel-1',
        'channel-2',
      ]);
      expect(useChatStore.getState().messagesByChannel.has('channel-2')).toBe(true);
      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(3);
      expect(isIndexed('retained-channel-message')).toBe(true);
      expect(useChannelStore.getState().isLoading).toBe(false);
    });

    it('purges channels removed by an authoritative re-fetch', async () => {
      let requestCount = 0;
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () => {
          requestCount++;
          return HttpResponse.json({
            channels: requestCount === 1 ? [mockChannel, mockEncryptedChannel] : [mockChannel],
          });
        })
      );

      await useChannelStore.getState().fetchChannels('server-1');
      useChatStore.getState().addMessage('channel-2', {
        ...mockMessage,
        id: 'removed-channel-message',
        channel_id: 'channel-2',
      });
      useUnreadStore.getState().setUnreadCount('channel-2', 3);
      indexMessage('removed-channel-message', 'revoked plaintext', 'channel-2');

      await useChannelStore.getState().fetchChannels('server-1');

      expect(useChannelStore.getState().channelIdsByServer['server-1']).toEqual(['channel-1']);
      expect(useChatStore.getState().messagesByChannel.has('channel-2')).toBe(false);
      expect(useUnreadStore.getState().unreadCounts.has('channel-2')).toBe(false);
      expect(isIndexed('removed-channel-message')).toBe(false);
    });

    it.each([
      ['removeChannel', () => useChannelStore.getState().removeChannel('channel-1'), ['channel-2']],
      [
        'removeServerChannels',
        () => useChannelStore.getState().removeServerChannels('server-1'),
        [],
      ],
      ['clearChannels', () => useChannelStore.getState().clearChannels(), []],
    ])('reconciles a stale response after %s', async (_name, revokeAccess, expectedIds) => {
      const started = deferred();
      const release = deferred();
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/channels`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json({ channels: [mockChannel, mockEncryptedChannel] });
        })
      );

      const fetchPromise = useChannelStore.getState().fetchChannels('server-1');
      await started.promise;
      revokeAccess();
      release.resolve();
      await fetchPromise;

      expect(useChannelStore.getState().channels.map((channel) => channel.id)).toEqual(expectedIds);
      expect(
        useChannelStore.getState().channels.some((channel) => channel.id === 'channel-1')
      ).toBe(false);
      expect(useChannelStore.getState().isLoading).toBe(false);
    });
  });

  describe('removeServerChannels', () => {
    it('purges a non-active server without clearing the active server', async () => {
      await useChannelStore.getState().fetchChannels('server-1');
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-2/channels`, () =>
          HttpResponse.json({
            channels: [{ ...mockChannel, id: 'server-2-channel', server_id: 'server-2' }],
          })
        )
      );
      await useChannelStore.getState().fetchChannels('server-2');
      indexMessage('server-1-message', 'revoked plaintext', 'channel-1');

      useChannelStore.getState().removeServerChannels('server-1');

      expect(useChannelStore.getState().channels.map((channel) => channel.id)).toEqual([
        'server-2-channel',
      ]);
      expect(useChannelStore.getState().currentServerId).toBe('server-2');
      expect(useChannelStore.getState().channelIdsByServer['server-1']).toBeUndefined();
      expect(isIndexed('server-1-message')).toBe(false);
    });
  });

  describe('clearChannels', () => {
    it('clears all channels', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      useChannelStore.getState().clearChannels();
      expect(useChannelStore.getState().channels).toHaveLength(0);
      expect(useChannelStore.getState().activeChannelId).toBeNull();
      expect(useChannelStore.getState().currentServerId).toBeNull();
    });

    it('removes every cleared channel from the in-memory search index', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      indexMessage('first-channel-message', 'first plaintext', 'channel-1');
      indexMessage('second-channel-message', 'second plaintext', 'channel-2');

      useChannelStore.getState().clearChannels();

      expect(isIndexed('first-channel-message')).toBe(false);
      expect(isIndexed('second-channel-message')).toBe(false);
    });

    it('invalidates every E2EE generation before removing its search scope', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChannelStore.getState().addChannel(mockEncryptedChannel);
      indexMessage('first-generation-order-message', 'first plaintext', 'channel-1');
      indexMessage('second-generation-order-message', 'second plaintext', 'channel-2');
      const indexedMessageByChannel: Record<string, string> = {
        'channel-1': 'first-generation-order-message',
        'channel-2': 'second-generation-order-message',
      };
      const observations: Array<{ channelId: string; searchScopePresent: boolean }> = [];
      const invalidateSpy = vi
        .spyOn(e2eeService, 'invalidateChannelKey')
        .mockImplementation((channelId) => {
          observations.push({
            channelId,
            searchScopePresent: isIndexed(indexedMessageByChannel[channelId] ?? ''),
          });
        });

      useChannelStore.getState().clearChannels();

      expect(invalidateSpy).toHaveBeenCalledTimes(2);
      expect(observations).toEqual([
        { channelId: 'channel-1', searchScopePresent: true },
        { channelId: 'channel-2', searchScopePresent: true },
      ]);
      expect(isIndexed('first-generation-order-message')).toBe(false);
      expect(isIndexed('second-generation-order-message')).toBe(false);
    });

    it('purges tracked channels that are not currently rendered', () => {
      useChannelStore.setState({
        channelIdsByServer: { 'server-1': ['unloaded-channel'] },
      });
      indexMessage('unloaded-message', 'revoked plaintext', 'unloaded-channel');

      useChannelStore.getState().clearChannels();

      expect(useChannelStore.getState().channelIdsByServer).toEqual({});
      expect(isIndexed('unloaded-message')).toBe(false);
    });
  });

  describe('clearChannelView', () => {
    it('preserves known server access state when no server is selected', () => {
      useChannelStore.setState({
        channels: [mockChannel],
        channelGroups: [],
        activeChannelId: mockChannel.id,
        currentServerId: mockChannel.server_id,
        channelIdsByServer: { [mockChannel.server_id]: [mockChannel.id] },
      });
      indexMessage('retained-message', 'retained plaintext', mockChannel.id);

      useChannelStore.getState().clearChannelView();

      expect(useChannelStore.getState().channels).toEqual([]);
      expect(useChannelStore.getState().channelIdsByServer).toEqual({
        [mockChannel.server_id]: [mockChannel.id],
      });
      expect(isIndexed('retained-message')).toBe(true);
    });

    it('does not let a pending fetch repopulate the cleared view', async () => {
      const started = deferred();
      const release = deferred();
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/channels`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json({ channels: [mockChannel] });
        })
      );

      const fetchPromise = useChannelStore.getState().fetchChannels('server-1');
      await started.promise;
      indexMessage('retained-during-fetch', 'retained plaintext', mockChannel.id);
      useChannelStore.getState().clearChannelView();
      release.resolve();
      await fetchPromise;

      expect(useChannelStore.getState().channels).toEqual([]);
      expect(useChannelStore.getState().activeChannelId).toBeNull();
      expect(useChannelStore.getState().currentServerId).toBeNull();
      expect(useChannelStore.getState().channelIdsByServer['server-1']).toEqual(['channel-1']);
      expect(isIndexed('retained-during-fetch')).toBe(true);
    });
  });
});

describe('channelStore expiration policy contract', () => {
  beforeEach(() => {
    resetAllStores();
    clearIndex();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('returns a fresh result only after the requested GET and preserves a live selection', async () => {
    useChannelStore.setState({ currentServerId: 'server-1', activeChannelId: 'channel-1' });
    const started = deferred();
    const release = deferred();
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, async () => {
        started.resolve();
        await release.promise;
        return HttpResponse.json({
          channels: [
            {
              ...mockChannel,
              id: 'channel-1',
              expiration_window_seconds: 86400,
              expiration_updated_at: '2026-09-08T05:00:00Z',
              expiration_revision: 4,
              expiration_backfill_pending: false,
            },
            {
              ...mockChannel,
              id: 'channel-2',
              expiration_window_seconds: 86400,
              expiration_updated_at: '2026-09-08T05:00:00Z',
              expiration_revision: 4,
              expiration_backfill_pending: false,
            },
          ],
        });
      })
    );
    const fetchPromise = useChannelStore.getState().fetchChannels('server-1', {
      targetId: 'channel-1',
      lifecycle: captureAuthLifecycle(),
    });
    await started.promise;
    useChannelStore.getState().setActiveChannel('channel-2');
    try {
      release.resolve();
      await expect(fetchPromise).resolves.toEqual({
        kind: 'fresh',
        policy: {
          windowSeconds: 86400,
          updatedAt: '2026-09-08T05:00:00Z',
          revision: 4,
          backfillPending: false,
        },
      });
    } finally {
      release.resolve();
    }
    expect(useChannelStore.getState().activeChannelId).toBe('channel-2');
  });

  it('reports malformed channel rows as a fetch error for the active server view', async () => {
    const serverAChannel = { ...mockChannel, id: 'channel-a', server_id: 'server-a' };
    useChannelStore.setState({
      channels: [serverAChannel],
      currentServerId: 'server-a',
      activeChannelId: 'channel-a',
      channelIdsByServer: { 'server-a': ['channel-a'] },
    });
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-b/channels`, () =>
        HttpResponse.json({ channels: [{}] })
      )
    );

    const result = await useChannelStore.getState().fetchChannels('server-b', {
      targetId: 'channel-b',
      lifecycle: captureAuthLifecycle(),
    });

    expect(result).toEqual({ kind: 'unavailable' });
    expect(useChannelStore.getState().currentServerId).toBe('server-b');
    expect(useChannelStore.getState().channels).toEqual([serverAChannel]);
    expect(useChannelStore.getState().error).toBe('Failed to load channels');
    expect(useChannelStore.getState().isLoading).toBe(false);
  });

  it.each([
    ['malformed 200', { channels: 'malformed' }],
    ['valid channel list', { channels: [{ ...mockChannel, server_id: 'server-a' }] }],
  ])(
    'supersedes a stale server-A read after selecting server-B (%s)',
    async (_label, staleResponse) => {
      const started = deferred();
      const release = deferred();
      const serverAChannel = { ...mockChannel, id: 'channel-a', server_id: 'server-a' };
      const serverBChannel = { ...mockChannel, id: 'channel-b', server_id: 'server-b' };
      useChannelStore.setState({
        channels: [serverAChannel],
        currentServerId: 'server-a',
        activeChannelId: 'channel-a',
        channelIdsByServer: { 'server-a': ['channel-a'] },
      });
      indexMessage('server-a-message', 'retained server A message', 'channel-a');
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-a/channels`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json(staleResponse);
        }),
        http.get(`${API_BASE}/api/v1/servers/server-b/channels`, () =>
          HttpResponse.json({ channels: [serverBChannel] })
        )
      );

      const staleRead = useChannelStore.getState().fetchChannels('server-a', {
        targetId: 'channel-a',
        lifecycle: captureAuthLifecycle(),
      });
      await started.promise;
      try {
        await useChannelStore.getState().fetchChannels('server-b');
        expect(useChannelStore.getState().currentServerId).toBe('server-b');
        expect(useChannelStore.getState().activeChannelId).toBe('channel-b');
        release.resolve();
        await expect(staleRead).resolves.toEqual({ kind: 'superseded' });
      } finally {
        release.resolve();
      }
      expect(useChannelStore.getState().currentServerId).toBe('server-b');
      expect(useChannelStore.getState().activeChannelId).toBe('channel-b');
      expect(useChannelStore.getState().channelIdsByServer['server-a']).toEqual(['channel-a']);
      expect(isIndexed('server-a-message')).toBe(true);
    }
  );

  it('keeps the current policy when a lower revision is applied', () => {
    useChannelStore.getState().addChannel(mockChannel);
    useChannelStore.getState().applyExpirationPolicy('channel-1', {
      windowSeconds: 86400,
      updatedAt: '2026-09-08T05:00:00Z',
      revision: 4,
      backfillPending: false,
    });
    useChannelStore.getState().applyExpirationPolicy('channel-1', {
      windowSeconds: 3600,
      updatedAt: '2026-09-08T04:00:00Z',
      revision: 3,
      backfillPending: true,
    });
    expect(useChannelStore.getState().channels[0].expirationPolicy).toEqual({
      windowSeconds: 86400,
      updatedAt: '2026-09-08T05:00:00Z',
      revision: 4,
      backfillPending: false,
    });
  });

  it.each([
    ['missing target', { channels: [{ ...mockChannel, id: 'channel-2' }] }, 'missing'],
    [
      'invalid policy',
      {
        channels: [
          {
            ...mockChannel,
            expiration_window_seconds: 300,
            expiration_updated_at: 'bad',
            expiration_revision: 4,
            expiration_backfill_pending: false,
          },
        ],
      },
      'unavailable',
    ],
  ])('reports optional read %s', async (_label, body, expected) => {
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () => HttpResponse.json(body))
    );
    await expect(
      useChannelStore.getState().fetchChannels('server-1', {
        targetId: 'channel-1',
        lifecycle: captureAuthLifecycle(),
      })
    ).resolves.toEqual({ kind: expected });
  });

  it('supersedes a stale generation but accepts ordinary credential rotation', async () => {
    const started = deferred();
    const release = deferred();
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, async () => {
        started.resolve();
        await release.promise;
        return HttpResponse.json({ channels: [mockChannel] });
      })
    );
    const staleLifecycle = captureAuthLifecycle();
    const staleRead = useChannelStore
      .getState()
      .fetchChannels('server-1', { targetId: 'channel-1', lifecycle: staleLifecycle });
    await started.promise;
    useAuthStore.getState().clearAccessToken();
    try {
      release.resolve();
      await expect(staleRead).resolves.toEqual({ kind: 'superseded' });
    } finally {
      release.resolve();
    }

    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({
          channels: [
            {
              ...mockChannel,
              expiration_window_seconds: 86400,
              expiration_updated_at: '2026-09-08T05:00:00Z',
              expiration_revision: 4,
              expiration_backfill_pending: false,
            },
          ],
        })
      )
    );
    const lifecycle = captureAuthLifecycle();
    useAuthStore
      .getState()
      .rotateAuthCredentials(lifecycle.authGeneration, 'rotated-token', 'rotated-session');
    await expect(
      useChannelStore.getState().fetchChannels('server-1', { targetId: 'channel-1', lifecycle })
    ).resolves.toEqual({
      kind: 'fresh',
      policy: {
        windowSeconds: 86400,
        updatedAt: '2026-09-08T05:00:00Z',
        revision: 4,
        backfillPending: false,
      },
    });
  });

  it('acknowledges seen revisions monotonically and clears markers with channels', () => {
    useChannelStore.getState().markExpirationSeen('account-a', 'channel-1', 4);
    useChannelStore.getState().markExpirationSeen('account-a', 'channel-1', 3);
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({
      'account-a': { 'channel-1': 4 },
    });
    useChannelStore.getState().clearChannels();
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({});
  });

  it('persists only markers/navigation and survives a storage quota failure in memory', () => {
    useChannelStore.setState({ currentServerId: 'server-1', activeChannelId: 'channel-1' });
    useChannelStore.getState().addChannel({
      ...mockChannel,
      expirationPolicy: {
        windowSeconds: 86400,
        updatedAt: '2026-09-08T05:00:00Z',
        revision: 4,
        backfillPending: false,
      },
    });
    useChannelStore.getState().markExpirationSeen('account-a', 'channel-1', 4);
    const stored = JSON.parse(localStorage.getItem('concord-channels') ?? '{}');
    expect(stored.state).toMatchObject({
      activeChannelId: 'channel-1',
      currentServerId: 'server-1',
      seenExpirationRevisionsByAccount: { 'account-a': { 'channel-1': 4 } },
    });
    expect(stored.state).not.toHaveProperty('channels');

    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementationOnce(() => {
      throw new DOMException('quota', 'QuotaExceededError');
    });
    try {
      expect(() =>
        useChannelStore.getState().markExpirationSeen('account-a', 'channel-1', 5)
      ).not.toThrow();
      expect(setItem).toHaveBeenCalled();
      expect(
        useChannelStore.getState().seenExpirationRevisionsByAccount['account-a']['channel-1']
      ).toBe(5);
    } finally {
      setItem.mockRestore();
    }
  });

  it('does not throw or lose a definitive policy when persistence repeatedly fails', () => {
    useChannelStore.getState().addChannel(mockChannel);
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('quota', 'QuotaExceededError');
    });
    try {
      expect(() =>
        useChannelStore.getState().applyExpirationPolicy('channel-1', {
          windowSeconds: 86400,
          updatedAt: '2026-09-08T05:00:00Z',
          revision: 4,
          backfillPending: false,
        })
      ).not.toThrow();
      expect(setItem).toHaveBeenCalled();
      expect(useChannelStore.getState().channels[0].expirationPolicy?.revision).toBe(4);
    } finally {
      setItem.mockRestore();
    }
  });

  it('rehydrates valid markers and preserves navigation, but discards corrupt marker maps', async () => {
    localStorage.setItem(
      'concord-channels',
      JSON.stringify({
        state: {
          activeChannelId: 'channel-1',
          currentServerId: 'server-1',
          seenExpirationRevisionsByAccount: { 'account-a': { 'channel-1': 4 } },
        },
        version: 0,
      })
    );
    await useChannelStore.persist.rehydrate();
    expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({
      'account-a': { 'channel-1': 4 },
    });

    localStorage.setItem(
      'concord-channels',
      JSON.stringify({
        state: {
          activeChannelId: 'channel-2',
          currentServerId: 'server-1',
          seenExpirationRevisionsByAccount: { bad: { channel: 'four' } },
        },
        version: 0,
      })
    );
    await useChannelStore.persist.rehydrate();
    expect(useChannelStore.getState().activeChannelId).toBe('channel-2');
    expect(useChannelStore.getState().seenExpirationRevisionsByAccount).toEqual({});
  });

  it.each([
    ['missing list', {}],
    ['non-array list', { channels: 'nope' }],
    ['non-object row', { channels: [null] }],
  ])('reports %s unavailable and preserves cached rows', async (_label, body) => {
    useChannelStore.getState().addChannel({
      ...mockChannel,
      expirationPolicy: {
        windowSeconds: 86400,
        updatedAt: '2026-09-08T05:00:00Z',
        revision: 4,
        backfillPending: false,
      },
    });
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () => HttpResponse.json(body))
    );
    await expect(
      useChannelStore
        .getState()
        .fetchChannels('server-1', { targetId: 'channel-1', lifecycle: captureAuthLifecycle() })
    ).resolves.toEqual({ kind: 'unavailable' });
    expect(useChannelStore.getState().channels[0].expirationPolicy?.revision).toBe(4);
  });

  it('reports a valid empty list as missing', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () =>
        HttpResponse.json({ channels: [] })
      )
    );
    await expect(
      useChannelStore
        .getState()
        .fetchChannels('server-1', { targetId: 'channel-1', lifecycle: captureAuthLifecycle() })
    ).resolves.toEqual({ kind: 'missing' });
  });

  it('supersedes an obsolete snapshot before making a network request', async () => {
    let called = false;
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, () => {
        called = true;
        return HttpResponse.json({ channels: [mockChannel] });
      })
    );
    const lifecycle = captureAuthLifecycle();
    useAuthStore.getState().clearAccessToken();
    await expect(
      useChannelStore.getState().fetchChannels('server-1', { targetId: 'channel-1', lifecycle })
    ).resolves.toEqual({ kind: 'superseded' });
    expect(called).toBe(false);
  });

  it('supersedes a discarded view even when its HTTP request fails', async () => {
    const started = deferred();
    const release = deferred();
    server.use(
      http.get(`${API_BASE}/api/v1/servers/server-1/channels`, async () => {
        started.resolve();
        await release.promise;
        return HttpResponse.json({ error: 'down' }, { status: 503 });
      })
    );
    const read = useChannelStore
      .getState()
      .fetchChannels('server-1', { targetId: 'channel-1', lifecycle: captureAuthLifecycle() });
    await started.promise;
    try {
      useChannelStore.getState().clearChannelView();
      release.resolve();
      await expect(read).resolves.toEqual({ kind: 'superseded' });
    } finally {
      release.resolve();
    }
  });
});
