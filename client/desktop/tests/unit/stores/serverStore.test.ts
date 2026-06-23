import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import {
  mockServer,
  mockServer2,
  mockChannel,
  mockEncryptedChannel,
  mockMessage,
} from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('serverStore', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  describe('fetchServers', () => {
    it('fetches servers from API', async () => {
      await useServerStore.getState().fetchServers();
      const state = useServerStore.getState();
      expect(state.servers.length).toBeGreaterThan(0);
      expect(state.isLoading).toBe(false);
    });

    it('sets error on fetch failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers`, () =>
          HttpResponse.json({ error: 'Server error' }, { status: 500 })
        )
      );
      await useServerStore.getState().fetchServers();
      expect(useServerStore.getState().error).toBe('Server error');
      expect(useServerStore.getState().isLoading).toBe(false);
    });

    it('handles empty server list', async () => {
      server.use(http.get(`${API_BASE}/api/v1/servers`, () => HttpResponse.json({ servers: [] })));
      await useServerStore.getState().fetchServers();
      expect(useServerStore.getState().servers).toHaveLength(0);
    });

    it('preserves a restored activeServerId when the fetched server still exists', async () => {
      useServerStore.getState().setActiveServer(mockServer.id);

      await useServerStore.getState().fetchServers();

      expect(useServerStore.getState().activeServerId).toBe(mockServer.id);
    });

    it('clears a restored activeServerId when the fetched server no longer exists', async () => {
      useServerStore.getState().setActiveServer('removed-server');

      await useServerStore.getState().fetchServers();

      expect(useServerStore.getState().activeServerId).toBeNull();
    });

    it('waits for persisted hydration before validating the restored active server', async () => {
      const hasHydratedSpy = vi.spyOn(useServerStore.persist, 'hasHydrated').mockReturnValue(false);
      const rehydrateSpy = vi
        .spyOn(useServerStore.persist, 'rehydrate')
        .mockImplementation(async () => {
          useServerStore.setState({ activeServerId: mockServer.id });
        });

      try {
        await useServerStore.getState().fetchServers();

        expect(rehydrateSpy).toHaveBeenCalledTimes(1);
        expect(useServerStore.getState().activeServerId).toBe(mockServer.id);
      } finally {
        hasHydratedSpy.mockRestore();
        rehydrateSpy.mockRestore();
      }
    });
  });

  describe('addServer', () => {
    it('adds a server to the list', () => {
      useServerStore.getState().addServer(mockServer);
      expect(useServerStore.getState().servers).toHaveLength(1);
      expect(useServerStore.getState().servers[0].id).toBe('server-1');
    });

    it('prepends new server', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().addServer(mockServer2);
      expect(useServerStore.getState().servers[0].id).toBe('server-2');
    });
  });

  describe('updateServer', () => {
    it('updates server properties', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().updateServer('server-1', { name: 'Renamed' });
      expect(useServerStore.getState().servers[0].name).toBe('Renamed');
    });

    it('does not affect other servers', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().addServer(mockServer2);
      useServerStore.getState().updateServer('server-1', { name: 'Renamed' });
      expect(useServerStore.getState().servers.find((s) => s.id === 'server-2')?.name).toBe(
        'Second Server'
      );
    });
  });

  describe('updateOnlineCounts', () => {
    it('updates online counts for servers', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().updateOnlineCounts({ 'server-1': 5 });
      expect(useServerStore.getState().servers[0].online_count).toBe(5);
    });

    it('ignores unknown server IDs', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().updateOnlineCounts({ 'unknown-server': 5 });
      expect(useServerStore.getState().servers[0].online_count).toBe(1);
    });

    it('updates multiple servers at once', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().addServer(mockServer2);
      useServerStore.getState().updateOnlineCounts({ 'server-1': 10, 'server-2': 20 });
      expect(useServerStore.getState().servers.find((s) => s.id === 'server-1')?.online_count).toBe(
        10
      );
      expect(useServerStore.getState().servers.find((s) => s.id === 'server-2')?.online_count).toBe(
        20
      );
    });
  });

  describe('removeServer', () => {
    it('removes server from list', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().addServer(mockServer2);
      useServerStore.getState().removeServer('server-1');
      expect(useServerStore.getState().servers).toHaveLength(1);
      expect(useServerStore.getState().servers[0].id).toBe('server-2');
    });

    it('clears activeServerId if the removed server was active', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().setActiveServer('server-1');
      useServerStore.getState().removeServer('server-1');
      expect(useServerStore.getState().activeServerId).toBeNull();
    });

    it('preserves activeServerId if a different server is removed', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().addServer(mockServer2);
      useServerStore.getState().setActiveServer('server-1');
      useServerStore.getState().removeServer('server-2');
      expect(useServerStore.getState().activeServerId).toBe('server-1');
    });

    it('cascades to clear channels and messages when currentServerId matches', () => {
      useServerStore.getState().addServer(mockServer);
      useChannelStore.setState({
        channels: [mockChannel, mockEncryptedChannel],
        currentServerId: 'server-1',
      });
      useChatStore.getState().addMessage('channel-1', mockMessage);
      useUnreadStore.getState().setUnreadCount('channel-1', 3);

      useServerStore.getState().removeServer('server-1');

      expect(useChannelStore.getState().channels).toHaveLength(0);
      expect(useChatStore.getState().messagesByChannel.has('channel-1')).toBe(false);
    });

    it('clears server-level unreads', () => {
      useServerStore.getState().addServer(mockServer);
      useUnreadStore.getState().markServerUnread('server-1');
      useServerStore.getState().removeServer('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(false);
    });

    it('cleans up lastChannelByServer for the removed server', () => {
      useServerStore.getState().addServer(mockServer);
      useChannelStore.setState({
        lastChannelByServer: { 'server-1': 'channel-1', 'server-2': 'channel-2' },
      });
      useServerStore.getState().removeServer('server-1');
      expect(useChannelStore.getState().lastChannelByServer['server-1']).toBeUndefined();
      expect(useChannelStore.getState().lastChannelByServer['server-2']).toBe('channel-2');
    });
  });

  describe('setActiveServer', () => {
    it('sets the active server', () => {
      useServerStore.getState().setActiveServer('server-1');
      expect(useServerStore.getState().activeServerId).toBe('server-1');
    });

    it('clears active server with null', () => {
      useServerStore.getState().setActiveServer('server-1');
      useServerStore.getState().setActiveServer(null);
      expect(useServerStore.getState().activeServerId).toBeNull();
    });

    it('persists activeServerId', () => {
      useServerStore.getState().setActiveServer('server-1');
      const stored = JSON.parse(localStorage.getItem('concord-servers') || '{}');
      expect(stored.state?.activeServerId).toBe('server-1');
    });
  });

  describe('clearServers', () => {
    it('clears all servers', () => {
      useServerStore.getState().addServer(mockServer);
      useServerStore.getState().clearServers();
      expect(useServerStore.getState().servers).toHaveLength(0);
      expect(useServerStore.getState().activeServerId).toBeNull();
    });
  });
});
