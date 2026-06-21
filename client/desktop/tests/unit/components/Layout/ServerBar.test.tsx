import { render, screen, fireEvent } from '../../../test-utils';
import ServerBar from '@/renderer/components/Layout/ServerBar';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
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
    expect(screen.getByLabelText('Toggle channel panel')).toBeInTheDocument();
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

  // ── Pin state is NOT toggled by icon clicks (#188) ──────────────────────
  // Regression guard: clicking the active-server icon (or PM icon) while the
  // channel panel is pinned used to call toggleChannelPin() → surprising
  // unpin, and it bypassed the interface lock. Now those clicks never change
  // the pin state; when unpinned they only peek (show the hover overlay).
  describe('channel-panel pin is not toggled by sticky-icon clicks (#188)', () => {
    beforeEach(() => {
      useServerStore.setState({
        servers: [mockServer],
        activeServerId: mockServer.id,
        isLoading: false,
        fetchServers: vi.fn() as unknown as () => Promise<void>,
      });
    });

    it('does not unpin when the pinned active-server icon is clicked (on /app)', () => {
      window.history.pushState({}, '', '/app');
      useLayoutStore.setState({ channelPanelPinned: true, channelPanelHoverVisible: false });
      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
      fireEvent.click(screen.getByLabelText('Toggle channel panel'));
      expect(useLayoutStore.getState().channelPanelPinned).toBe(true);
    });

    it('peeks without pinning when the unpinned active-server icon is clicked (on /app)', () => {
      window.history.pushState({}, '', '/app');
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: false });
      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
      fireEvent.click(screen.getByLabelText('Toggle channel panel'));
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(true);
      expect(useLayoutStore.getState().channelPanelPinned).toBe(false);
    });

    it('does not unpin when the pinned PM icon is clicked (on /app/dms)', () => {
      window.history.pushState({}, '', '/app/dms');
      useLayoutStore.setState({ channelPanelPinned: true, channelPanelHoverVisible: false });
      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
      fireEvent.click(screen.getByLabelText('Direct Messages'));
      expect(useLayoutStore.getState().channelPanelPinned).toBe(true);
    });

    it('peeks without pinning when the unpinned PM icon is clicked (on /app/dms)', () => {
      window.history.pushState({}, '', '/app/dms');
      useLayoutStore.setState({ channelPanelPinned: false, channelPanelHoverVisible: false });
      render(<ServerBar onOpenActionModal={onOpenActionModal} onContextMenu={onContextMenu} />);
      fireEvent.click(screen.getByLabelText('Direct Messages'));
      expect(useLayoutStore.getState().channelPanelHoverVisible).toBe(true);
      expect(useLayoutStore.getState().channelPanelPinned).toBe(false);
    });
  });
});
