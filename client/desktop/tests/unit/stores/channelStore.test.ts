import { useChannelStore } from '@/renderer/stores/channelStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockEncryptedChannel, mockMessage } from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('channelStore', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  describe('addChannel', () => {
    it('adds a channel', () => {
      useChannelStore.getState().addChannel(mockChannel);
      expect(useChannelStore.getState().channels).toHaveLength(1);
      expect(useChannelStore.getState().channels[0].name).toBe('general');
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
    });

    it('cascades to clear messages and unreads', () => {
      useChannelStore.getState().addChannel(mockChannel);
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useUnreadStore.getState().setUnreadCount('channel-1', 3);
      useChannelStore.getState().removeChannel('channel-1');
      expect(useChatStore.getState().messagesByChannel.has('channel-1')).toBe(false);
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
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
  });
});
