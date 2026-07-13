import { useChannelStore } from '@/renderer/stores/channelStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { clearIndex, indexMessage, isIndexed } from '@/renderer/services/searchService';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockEncryptedChannel, mockMessage } from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

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
