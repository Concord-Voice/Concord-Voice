import { render, screen, fireEvent, act, waitFor } from '../../../test-utils';
import ServerBar from '@/renderer/components/Layout/ServerBar';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useDMStore, type DMConversation } from '@/renderer/stores/dmStore';
import { useNotificationPrefsStore } from '@/renderer/stores/notificationPrefsStore';
import { resetAllStores } from '../../../helpers/store-helpers';
import { mockServer, mockServer2 } from '../../../mocks/fixtures';
import { vi } from 'vitest';

// Mock apiClient to prevent real API calls from fetchServers useEffect
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve([]),
  }),
  API_BASE: 'http://localhost:3001',
}));

describe('ServerBar', () => {
  const onOpenActionModal = vi.fn();
  const onContextMenu = vi.fn();
  const mockDMConversation: DMConversation = {
    id: 'dm-1',
    isGroup: false,
    isPersonal: true,
    name: null,
    participants: [
      {
        userId: 'user-1',
        username: 'alex',
      },
    ],
    lastMessage: null,
    unreadCount: 0,
    createdAt: '2026-06-23T00:00:00.000Z',
  };

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    // Override fetchServers to prevent store from clearing manually-set servers
    useServerStore.setState({ fetchServers: vi.fn() as unknown as () => Promise<void> });
  });

  it('renders PM button', () => {
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    expect(screen.getByLabelText('Direct Messages')).toBeInTheDocument();
  });

  it('renders add server button', () => {
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    expect(screen.getByLabelText('Add Server')).toBeInTheDocument();
  });

  it('calls onOpenActionModal when add button clicked', async () => {
    const { userEvent } = await import('../../../test-utils');
    const user = userEvent.setup();
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    await user.click(screen.getByLabelText('Add Server'));
    expect(onOpenActionModal).toHaveBeenCalled();
  });

  it('renders server icons when servers exist', () => {
    useServerStore.setState({
      servers: [mockServer, mockServer2],
      activeServerId: mockServer.id,
      isLoading: false,
    });
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    // mockServer2 should be in the bar (non-active)
    expect(screen.getByLabelText('Second Server server')).toBeInTheDocument();
  });

  it('shows active server icon', () => {
    useServerStore.setState({
      servers: [mockServer],
      activeServerId: mockServer.id,
      isLoading: false,
    });
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    expect(screen.getByLabelText('Test Server server')).toBeInTheDocument();
  });

  it('shows placeholder when no active server', () => {
    useServerStore.setState({ servers: [], isLoading: false, activeServerId: null });
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    expect(screen.getByLabelText('No active server')).toBeInTheDocument();
  });

  it('shows unread badge for servers with unreads', () => {
    useServerStore.setState({
      servers: [mockServer, mockServer2],
      activeServerId: mockServer.id,
      isLoading: false,
    });
    useUnreadStore.getState().markServerUnread(mockServer2.id);
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    const badge = document.querySelector('.server-bar-badge');
    expect(badge).toBeInTheDocument();
  });

  it('shows an unread badge on the PM button when any DM has unreads', () => {
    useDMStore.setState({
      conversations: [{ ...mockDMConversation, unreadCount: 2 }],
    });

    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

    const pmButton = screen.getByLabelText('Direct Messages');
    expect(pmButton.querySelector('.server-bar-badge')).toBeInTheDocument();
  });

  it('clears the PM unread badge when all DM conversations are read', () => {
    useDMStore.setState({
      conversations: [{ ...mockDMConversation, unreadCount: 2 }],
    });

    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

    act(() => {
      useDMStore.getState().clearUnread(mockDMConversation.id);
    });

    const pmButton = screen.getByLabelText('Direct Messages');
    return waitFor(() => {
      expect(pmButton.querySelector('.server-bar-badge')).not.toBeInTheDocument();
    });
  });

  it('shows loading skeletons when loading', () => {
    useServerStore.setState({ servers: [], isLoading: true });
    render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
    const skeletons = document.querySelectorAll('.skeleton');
    expect(skeletons.length).toBeGreaterThan(0);
  });

  // ─── Muted-server visual state (#84) ────────────────────────────────────

  describe('muted server visual state', () => {
    it('paints data-muted + the bell-slash overlay on a muted server', () => {
      // mockServer2 is in the scrollable list (not the active-server slot),
      // which is where the muted styling lives. mockServer is active.
      useServerStore.setState({
        servers: [mockServer, mockServer2],
        activeServerId: mockServer.id,
        isLoading: false,
      });
      useNotificationPrefsStore.getState().setMute('server', mockServer2.id, true, null);

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      // The muted server's icon button carries data-muted="true" so the CSS
      // selector (.server-bar-icon[data-muted='true']) lands the 60% opacity.
      const btn = screen.getByLabelText('Second Server server (muted)');
      expect(btn).toHaveAttribute('data-muted', 'true');

      // The corner overlay element is rendered as a sibling — verify by class.
      // (No accessible name on purpose — it's a decorative cue, aria-hidden.)
      const overlay = btn.parentElement?.querySelector('.server-bar-mute-overlay');
      expect(overlay).toBeInTheDocument();
    });

    it('omits data-muted and the overlay when the server is not muted', () => {
      useServerStore.setState({
        servers: [mockServer, mockServer2],
        activeServerId: mockServer.id,
        isLoading: false,
      });
      // Deliberately no setMute call — the server should render with no
      // muted treatment at all.

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const btn = screen.getByLabelText('Second Server server');
      expect(btn).not.toHaveAttribute('data-muted');
      const overlay = btn.parentElement?.querySelector('.server-bar-mute-overlay');
      expect(overlay).not.toBeInTheDocument();
    });

    it('treats an expired timed mute as unmuted (no overlay) before the sweep', () => {
      // A timed mute that has already expired stays in the store until the
      // 60s sweep prunes it, but the inline expiry check in
      // isEntryCurrentlyMuted should make the UI render as unmuted.
      useServerStore.setState({
        servers: [mockServer, mockServer2],
        activeServerId: mockServer.id,
        isLoading: false,
      });
      const past = new Date(Date.now() - 60_000);
      useNotificationPrefsStore.getState().setMute('server', mockServer2.id, true, past);

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const btn = screen.getByLabelText('Second Server server');
      expect(btn).not.toHaveAttribute('data-muted');
    });
  });

  // ─── Per-target mutes gate the unread badges (#84 / epic #1029 close audit) ──
  // A muted target must not light its unread indicator UNLESS the unread is
  // precise (mute-resolved). The server dot renders when hasUnread &&
  // (isUnreadPrecise || !isMuted): an approximate (bulk-seed) unread is
  // suppressed under a server mute, but a precise unread — e.g. a channel
  // explicitly unmuted under a muted server — still shows its dot. The DM
  // aggregate badge selector skips conversations whose isEntryCurrentlyMuted is
  // true.

  describe('muted targets suppress unread badges', () => {
    it('renders no unread dot for a muted server with an approximate unread but keeps the mute overlay', () => {
      useServerStore.setState({
        servers: [mockServer, mockServer2],
        activeServerId: mockServer.id,
        isLoading: false,
      });
      // Approximate (bulk-seed-style) mark: no channel-wins evidence.
      useUnreadStore.getState().markServerUnread(mockServer2.id);
      useNotificationPrefsStore.getState().setMute('server', mockServer2.id, true, null);

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const btn = screen.getByLabelText('Second Server server (muted)');
      // The approximate unread dot is gated on !isMuted, so it is absent…
      expect(btn.parentElement?.querySelector('.server-bar-badge')).not.toBeInTheDocument();
      // …but the mute overlay still marks the server as muted.
      expect(btn.parentElement?.querySelector('.server-bar-mute-overlay')).toBeInTheDocument();
    });

    it('renders the unread dot for a muted server with a PRECISE unread (channel-wins override)', () => {
      useServerStore.setState({
        servers: [mockServer, mockServer2],
        activeServerId: mockServer.id,
        isLoading: false,
      });
      // Precise mark: a mute-aware source already resolved channel-wins and
      // found an unmuted unread channel under this server-level-muted server.
      useUnreadStore.getState().markServerUnread(mockServer2.id, true);
      useNotificationPrefsStore.getState().setMute('server', mockServer2.id, true, null);

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const btn = screen.getByLabelText('Second Server server (muted)');
      // The dot shows because the unread is precise, honoring the explicit
      // channel-level unmute even though the parent server is muted…
      expect(btn.parentElement?.querySelector('.server-bar-badge')).toBeInTheDocument();
      // …and the mute overlay still marks the server as muted.
      expect(btn.parentElement?.querySelector('.server-bar-mute-overlay')).toBeInTheDocument();
    });

    it('renders the unread dot for an unread server that is not muted', () => {
      useServerStore.setState({
        servers: [mockServer, mockServer2],
        activeServerId: mockServer.id,
        isLoading: false,
      });
      useUnreadStore.getState().markServerUnread(mockServer2.id);
      // No setMute — the unread dot must still light.

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const btn = screen.getByLabelText('Second Server server');
      expect(btn.parentElement?.querySelector('.server-bar-badge')).toBeInTheDocument();
    });

    it('hides the PM aggregate badge when the only unread DM is muted', () => {
      useDMStore.setState({
        conversations: [{ ...mockDMConversation, unreadCount: 2 }],
      });
      useNotificationPrefsStore.getState().setMute('dm', mockDMConversation.id, true, null);

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const pmButton = screen.getByLabelText('Direct Messages');
      expect(pmButton.querySelector('.server-bar-badge')).not.toBeInTheDocument();
    });

    it('shows the PM aggregate badge when an unread DM is not muted', () => {
      useDMStore.setState({
        conversations: [{ ...mockDMConversation, unreadCount: 2 }],
      });
      // No setMute — an unmuted unread DM still lights the aggregate badge.

      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);

      const pmButton = screen.getByLabelText('Direct Messages');
      expect(pmButton.querySelector('.server-bar-badge')).toBeInTheDocument();
    });
  });
});
