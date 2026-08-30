import { render, screen, fireEvent, act } from '../../../test-utils';
import { mockChannel } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { ADMIN_PERMISSIONS, MANAGE_CHANNELS } from '@/renderer/utils/permissions';
import ChannelContextMenu from '@/renderer/components/Channels/ChannelContextMenu';
import type { Channel } from '@/renderer/types/chat';

// Mock apiFetch
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({ ok: true }),
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
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().markServerUnread(SERVER_ID);
      renderMenu();
      fireEvent.click(screen.getByText('Mark as Read'));
      expect(useUnreadStore.getState().serverUnreadSet.has(SERVER_ID)).toBe(false);
    });

    it('clears the server dot when every remaining unread channel is muted', () => {
      // channel-1 is being marked read; channel-2 stays unread but is muted.
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-2', 3);
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
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-2', 3);
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
