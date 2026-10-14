import { render, screen, fireEvent, act } from '../../../test-utils';
import { renderHook } from '@testing-library/react';
import { mockChannel, mockServer, mockServer2 } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import {
  isServerUnreadVisible,
  useNotificationPrefsStore,
} from '@/renderer/stores/ui/notificationPrefsStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { apiFetch } from '@/renderer/services/system/apiClient';
import { useServerChannelSubscriptions } from '@/renderer/hooks/messaging/useServerChannelSubscriptions';
import { resetAllStores } from '../../../helpers/store-helpers';
import { ADMIN_PERMISSIONS, MANAGE_CHANNELS } from '@/renderer/utils/policy/permissions';
import ChannelContextMenu from '@/renderer/components/Channels/ChannelContextMenu';
import type { Channel } from '@/renderer/types/chat';

// Mock apiFetch
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true }),
}));

vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({ subscribeServer: vi.fn(), unsubscribeServer: vi.fn() }),
}));

const SERVER_ID = 'server-1';
const OWNER_PERMS = ADMIN_PERMISSIONS | MANAGE_CHANNELS;

describe('ChannelContextMenu — extended coverage', () => {
  const mockOnClose = vi.fn();
  const mockOnEditChannel = vi.fn();
  const mockOnDeleteChannel = vi.fn();
  const mockOnChannelPermissions = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: OWNER_PERMS },
    });
    useServerStore.setState({ activeServerId: SERVER_ID });
    useUnreadStore.setState({ unreadCounts: new Map(), serverUnreadSet: new Set() });
    useNotificationPrefsStore.getState().clearAll();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = (channel = mockChannel) => {
    return render(
      <ChannelContextMenu
        channel={channel}
        position={{ x: 100, y: 100 }}
        serverId={SERVER_ID}
        onClose={mockOnClose}
        onEditChannel={mockOnEditChannel}
        onDeleteChannel={mockOnDeleteChannel}
        onChannelPermissions={mockOnChannelPermissions}
      />
    );
  };

  describe('Mark as Read', () => {
    it('clears unread count and closes menu', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      renderMenu();
      fireEvent.click(screen.getByText('Mark as Read'));
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
      expect(mockOnClose).toHaveBeenCalled();
    });

    it('clears server unread when all channel unreads are gone', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]), SERVER_ID);
      useUnreadStore.getState().markServerUnread(SERVER_ID);
      renderMenu();
      fireEvent.click(screen.getByText('Mark as Read'));
      expect(useUnreadStore.getState().serverUnreadSet.has(SERVER_ID)).toBe(false);
    });

    it('clears the server dot when every remaining unread channel is muted', () => {
      // channel-1 is being marked read; channel-2 stays unread but is muted.
      useUnreadStore.getState().setInitialUnreads(
        new Map([
          ['channel-1', 5],
          ['channel-2', 3],
        ]),
        SERVER_ID
      );
      useUnreadStore.getState().markServerUnread(SERVER_ID);
      useNotificationPrefsStore.getState().setMute('channel', 'channel-2', true, null);
      renderMenu();
      fireEvent.click(screen.getByText('Mark as Read'));
      // A plain size===0 check would leave the dot lit (channel-2 remains), but
      // its only remaining unread is muted so the server icon must go dark
      // (#84 / epic #1029 close audit, P2 follow-up).
      expect(useUnreadStore.getState().serverUnreadSet.has(SERVER_ID)).toBe(false);
    });

    it('keeps the server dot when an unmuted unread channel remains', () => {
      useUnreadStore.getState().setInitialUnreads(
        new Map([
          ['channel-1', 5],
          ['channel-2', 3],
        ]),
        SERVER_ID
      );
      useUnreadStore.getState().markServerUnread(SERVER_ID);
      // channel-2 stays unread and unmuted, so the dot must survive.
      renderMenu();
      fireEvent.click(screen.getByText('Mark as Read'));
      expect(useUnreadStore.getState().serverUnreadSet.has(SERVER_ID)).toBe(true);
    });
  });

  describe('Copy Link', () => {
    it('copies channel ID to clipboard', async () => {
      renderMenu();
      fireEvent.click(screen.getByText('Copy Link'));
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith('channel-1');
    });

    it('shows Copied! feedback after clicking', async () => {
      // Ensure clipboard mock returns a resolved promise
      vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue(undefined);
      renderMenu();
      await act(async () => {
        fireEvent.click(screen.getByText('Copy Link'));
      });
      expect(screen.getByText('Copied!')).toBeInTheDocument();
    });
  });

  describe('Channel Permissions', () => {
    it('calls onChannelPermissions when Channel Permissions is clicked', () => {
      renderMenu();
      fireEvent.click(screen.getByText('Channel Permissions'));
      expect(mockOnChannelPermissions).toHaveBeenCalledWith(mockChannel);
      expect(mockOnClose).toHaveBeenCalled();
    });
  });

  describe('channel type icon', () => {
    it('renders volume icon for voice channels', () => {
      const voiceChannel: Channel = { ...mockChannel, type: 'voice', name: 'voice-ch' };
      renderMenu(voiceChannel);
      expect(screen.getByText('voice-ch')).toBeInTheDocument();
    });

    it('renders pin icon for bulletin channels', () => {
      const bulletinChannel: Channel = { ...mockChannel, type: 'bulletin', name: 'bulletin-ch' };
      renderMenu(bulletinChannel);
      expect(screen.getByText('bulletin-ch')).toBeInTheDocument();
    });
  });

  describe('channel emoji', () => {
    it('renders channel emoji when set', () => {
      const channelWithEmoji: Channel = { ...mockChannel, emoji: '🎮' };
      renderMenu(channelWithEmoji);
      expect(screen.getByText('🎮')).toBeInTheDocument();
    });
  });
});

describe('ChannelContextMenu caller regression (#3773)', () => {
  beforeEach(() => {
    useUnreadStore.setState(useUnreadStore.getInitialState(), true);
    useChannelStore.setState(useChannelStore.getInitialState(), true);
    resetAllStores();
    vi.clearAllMocks();
    vi.mocked(apiFetch).mockReset();
    useAuthStore.getState().setAccessToken('test-caller-unread-token');
    useChatStore.setState({ isConnected: true });
    usePermissionStore.setState({ serverPermissions: { [SERVER_ID]: OWNER_PERMS } });
  });

  it.each([
    {
      scenario: 'keeps untouched late A2 visible',
      laterServerClear: false,
      expectedVisible: true,
      bulkBeforeRead: false,
    },
    {
      scenario: 'keeps already known eligible A2 visible',
      laterServerClear: false,
      expectedVisible: true,
      bulkBeforeRead: true,
    },
    {
      scenario: 'preserves a genuinely later server-only clear',
      laterServerClear: true,
      expectedVisible: false,
      bulkBeforeRead: false,
    },
  ])(
    '$scenario after the actual Mark as Read handler',
    async ({ laterServerClear, expectedVisible, bulkBeforeRead }) => {
      useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: SERVER_ID });
      useChannelStore.setState({ channels: [mockChannel], activeChannelId: null });
      useUnreadStore.getState().setInitialChannelUnreads([
        { channelId: 'channel-1', serverId: SERVER_ID, count: 1 },
        { channelId: 'channel-b1', serverId: 'server-2', count: 2 },
      ]);
      useUnreadStore.getState().markServerUnread(SERVER_ID, true);
      useUnreadStore.getState().markServerUnread('server-2', true);
      const bulkData = {
        server_ids: [SERVER_ID, 'server-2'],
        channels: [
          { channel_id: 'channel-1', server_id: SERVER_ID, unread_count: 1 },
          { channel_id: 'channel-a2', server_id: SERVER_ID, unread_count: 3 },
          { channel_id: 'channel-b1', server_id: 'server-2', unread_count: 2 },
          { channel_id: 'channel-b-control', server_id: 'server-2', unread_count: 4 },
        ],
      };
      let resolveBulk!: (response: Response) => void;
      let resolveA!: (response: Response) => void;
      let resolveB!: (response: Response) => void;
      const bulk = new Promise<Response>((resolve) => {
        resolveBulk = resolve;
      });
      const serverA = new Promise<Response>((resolve) => {
        resolveA = resolve;
      });
      const serverB = new Promise<Response>((resolve) => {
        resolveB = resolve;
      });
      vi.mocked(apiFetch).mockImplementation((path) => {
        if (path === '/api/v1/servers/unread-status') return bulk;
        if (path === '/api/v1/servers/server-1/unread') return serverA;
        if (path === '/api/v1/servers/server-2/unread') return serverB;
        if (path === '/api/v1/channels/channel-1/read') return Promise.resolve(new Response('{}'));
        throw new Error(`Unexpected caller endpoint: ${path}`);
      });
      const hook = renderHook(() => useServerChannelSubscriptions());
      let view: ReturnType<typeof render> | undefined;
      const visible = (serverId: string) => {
        const state = useUnreadStore.getState();
        return isServerUnreadVisible(
          serverId,
          state.serverUnreadSet,
          state.serverUnreadPreciseSet,
          useNotificationPrefsStore.getState().mutedServers
        );
      };
      try {
        if (bulkBeforeRead) {
          await act(async () => {
            resolveBulk(new Response(JSON.stringify(bulkData)));
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-a2')).toEqual({
            serverId: SERVER_ID,
            count: 3,
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-b-control')).toEqual({
            serverId: 'server-2',
            count: 4,
          });
          expect(visible(SERVER_ID)).toBe(true);
        }
        await act(async () => {
          resolveA(
            new Response(
              JSON.stringify({ unreads: [{ channel_id: 'channel-1', unread_count: 1 }] })
            )
          );
        });
        expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(1);
        expect(useUnreadStore.getState().unreadCountsServerId).toBe(SERVER_ID);
        view = render(
          <ChannelContextMenu
            channel={mockChannel}
            serverId={SERVER_ID}
            position={{ x: 100, y: 100 }}
            onClose={vi.fn()}
            onEditChannel={vi.fn()}
            onDeleteChannel={vi.fn()}
          />
        );
        fireEvent.click(screen.getByText('Mark as Read'));
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
        if (bulkBeforeRead) {
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-a2')).toEqual({
            serverId: SERVER_ID,
            count: 3,
          });
          expect
            .soft(
              visible(SERVER_ID),
              'the actual channel Mark as Read handler must immediately preserve the dot for already known eligible A2'
            )
            .toBe(true);
        } else {
          expect(visible(SERVER_ID)).toBe(false);
        }
        expect(apiFetch).toHaveBeenCalledWith('/api/v1/channels/channel-1/read', {
          method: 'POST',
        });
        act(() => {
          useChannelStore.setState({
            activeChannelId: null,
            channels: [{ ...mockChannel, id: 'channel-b1', server_id: 'server-2' }],
          });
          useServerStore.setState({ activeServerId: 'server-2' });
        });
        await act(async () => {
          resolveB(
            new Response(
              JSON.stringify({ unreads: [{ channel_id: 'channel-b1', unread_count: 2 }] })
            )
          );
        });
        expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
        expect(visible('server-2')).toBe(true);
        if (laterServerClear) {
          // An independent later server-level decision must still outrank the
          // detailed snapshot; it is distinct from the handler's read cleanup.
          act(() => {
            useUnreadStore.getState().clearServerUnread(SERVER_ID);
          });
        }
        if (!bulkBeforeRead) {
          await act(async () => {
            resolveBulk(new Response(JSON.stringify(bulkData)));
          });
        }
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-a2')).toEqual({
          serverId: SERVER_ID,
          count: 3,
        });
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-b1')).toEqual({
          serverId: 'server-2',
          count: 2,
        });
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-b-control')).toEqual({
          serverId: 'server-2',
          count: 4,
        });
        expect(visible('server-2')).toBe(true);
        expect(
          vi
            .mocked(apiFetch)
            .mock.calls.filter(([path]) => path === '/api/v1/servers/unread-status')
        ).toHaveLength(1);
        expect(
          visible(SERVER_ID),
          laterServerClear
            ? 'a genuinely later server-only clear must remain authoritative over late eligible detail'
            : bulkBeforeRead
              ? 'the actual channel Mark as Read handler must keep already known eligible A2’s server dot after reading A1'
              : 'the actual channel Mark as Read handler must preserve late eligible A2’s server dot after reading A1'
        ).toBe(expectedVisible);
      } finally {
        view?.unmount();
        hook.unmount();
        await act(async () => {
          resolveBulk(new Response('{}'));
          resolveA(new Response('{}'));
          resolveB(new Response('{}'));
        });
      }
    }
  );
});
