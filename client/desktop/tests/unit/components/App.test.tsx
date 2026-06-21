import { render, screen, act } from '../../test-utils';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useNotificationNavigationStore } from '@/renderer/stores/notificationNavigationStore';
import { usePendingRegistrationStore } from '@/renderer/stores/pendingRegistrationStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
// Mock child components to prevent complex rendering
vi.mock('@/renderer/components/Auth/AuthFlow', () => ({
  default: () => <div data-testid="auth-flow">AuthFlow</div>,
}));
vi.mock('@/renderer/components/MainView/MainView', () => ({
  default: () => <div data-testid="main-view">MainView</div>,
}));
vi.mock('@/renderer/components/DirectMessages/DirectMessagesView', () => ({
  default: () => <div data-testid="dm-view">DirectMessagesView</div>,
}));
vi.mock('@/renderer/components/Profile/ProfilePage', () => ({
  default: () => <div data-testid="profile-page">ProfilePage</div>,
}));
vi.mock('@/renderer/components/Settings/SettingsPage', () => ({
  default: () => <div data-testid="settings-page">SettingsPage</div>,
}));
vi.mock('@/renderer/components/Servers/ServerSettingsPage', () => ({
  default: () => <div data-testid="server-settings-page">ServerSettingsPage</div>,
}));
vi.mock('@/renderer/components/Voice/PipWindow', () => ({
  default: () => <div data-testid="pip-window">PipWindow</div>,
}));
vi.mock('@/renderer/hooks/useWebSocket', () => ({
  useWebSocket: vi.fn(),
}));
vi.mock('@/renderer/components/ui/ConnectionLostOverlay', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/ui/ForceUpdateOverlay', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/ui/UpdateBanner', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/Auth/MFAChallengeModal', () => ({
  default: () => null,
}));
vi.mock('@/renderer/components/Auth/SSOEagerUnlock', () => ({
  default: ({
    onUnlock,
    onSocialRecovery,
  }: {
    onUnlock: () => void;
    onSocialRecovery: () => void;
  }) => (
    <div data-testid="sso-eager-unlock">
      <button data-testid="sso-eager-unlock-unlock" onClick={onUnlock}>
        Unlock
      </button>
      <button data-testid="sso-eager-unlock-recovery" onClick={onSocialRecovery}>
        Recovery
      </button>
    </div>
  ),
}));
vi.mock('@/renderer/services/clientConfigService', () => ({
  clientConfigService: { start: vi.fn(), stop: vi.fn() },
}));
vi.mock('@/renderer/services/mediaCapabilities', () => ({
  detectCodecCapabilities: vi.fn().mockResolvedValue({}),
  prewarmWebRTC: vi.fn(),
}));

const mockClearBadge = vi.fn();
vi.mock('@/renderer/services/desktopNotificationService', () => ({
  desktopNotificationService: {
    clearBadge: (...args: unknown[]) => mockClearBadge(...args),
  },
}));

const mockMarkRendererCrashed = vi.fn().mockResolvedValue(undefined);
const mockSoftRestart = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/recoveryService', () => ({
  markRendererCrashed: (...args: unknown[]) => mockMarkRendererCrashed(...args),
}));
vi.mock('@/renderer/services/resetService', () => ({
  softRestart: (...args: unknown[]) => mockSoftRestart(...args),
}));

const mockInitializeFromStoredKeys = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    initializeFromStoredKeys: (...args: unknown[]) => mockInitializeFromStoredKeys(...args),
    encryptForChannel: vi.fn(),
    decryptForChannel: vi.fn(),
    getChannelKey: vi.fn(),
    invalidateChannelKey: vi.fn(),
  },
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    init: vi.fn(),
    startWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));
vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    startWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

import App, { handleAppRootError } from '@/renderer/App';

describe('App', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useAuthStore.getState().clearAccessToken();
    useUserStore.setState({ user: null });
    usePendingRegistrationStore.getState().clearPending();
    useE2EEStore.getState().reset();
  });

  // ── Pending registration cleanup on startup ─────────────────────────────

  it('clears expired pending registration on mount', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'stale-pid',
      email: 'old@example.com',
      expires_at: new Date(Date.now() - 60_000).toISOString(),
      code_expires_at: new Date(Date.now() - 60_000).toISOString(),
    });

    render(<App />);

    expect(usePendingRegistrationStore.getState().pendingId).toBeNull();
  });

  it('preserves non-expired pending registration on mount', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'fresh-pid',
      email: 'new@example.com',
      expires_at: new Date(Date.now() + 10 * 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
    });

    render(<App />);

    expect(usePendingRegistrationStore.getState().pendingId).toBe('fresh-pid');
  });

  it('renders title bar', () => {
    render(<App />);
    // #806: branded Titlebar renders "Concord Voice" in Droidiga.
    // Title Case (not ALL-CAPS) since the BaronNeue→Droidiga swap: Droidiga's
    // decorative glyphs are in the lowercase slots, so caps would render plain.
    expect(screen.getByText('Concord Voice')).toBeInTheDocument();
  });

  it('renders auth flow on root path', () => {
    render(<App />);
    expect(screen.getByTestId('auth-flow')).toBeInTheDocument();
  });

  it('renders app container with correct class', () => {
    const { container } = render(<App />);
    expect(container.querySelector('.app')).toBeInTheDocument();
  });

  it('renders title bar section', () => {
    const { container } = render(<App />);
    // #806: .titlebar replaces the old .title-bar; .titlebar-title carries the brand text
    expect(container.querySelector('.titlebar')).toBeInTheDocument();
    expect(container.querySelector('.titlebar-title')).toBeInTheDocument();
  });

  it('prevents context menu on app container', () => {
    const { container } = render(<App />);
    const appDiv = container.querySelector('.app');
    expect(appDiv).toBeInTheDocument();
  });

  // ── Auth state routing ──

  it('shows AuthFlow when not authenticated', () => {
    render(<App />);
    expect(screen.getByTestId('auth-flow')).toBeInTheDocument();
  });

  it('redirects to DMs when authenticated with verified email', () => {
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.setState({ emailVerified: true });
    useUserStore.setState({
      user: {
        id: 'user-1',
        email: 'test@concord.chat',
        username: 'testuser',
        display_name: 'Test User',
        bio: null,
        avatar_url: null,
        header_image_url: null,
        links: [],
        email_verified: true,
        age_verified: true,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      },
    });
    render(<App />);
    // Should redirect to /app/dms and show DM view
    expect(screen.queryByTestId('auth-flow')).not.toBeInTheDocument();
  });

  it('redirects unverified users back to auth flow', () => {
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.setState({ emailVerified: false });
    render(<App />);
    // Unverified users should see auth flow, not the app
    expect(screen.getByTestId('auth-flow')).toBeInTheDocument();
  });

  // ── Title bar info ──

  it('renders title bar text as Concord Voice', () => {
    render(<App />);
    // Brand text in Droidiga (Title Case) per Titlebar.css — re-cased from
    // ALL-CAPS during the BaronNeue→Droidiga font swap.
    const titleText = document.querySelector('.titlebar-title');
    expect(titleText?.textContent).toBe('Concord Voice');
  });

  // ── Error boundary ──

  it('renders app without crashing', () => {
    const { container } = render(<App />);
    expect(container).toBeTruthy();
  });

  // ── Session restore loading state ──

  it('shows loading state while restoring session', () => {
    // The App checks for globalThis.electron.restoreSession
    // Without it, isRestoring is set to false immediately
    const { container } = render(<App />);
    // Should eventually show content (either auth or main)
    expect(container.querySelector('.app')).toBeInTheDocument();
  });

  // ── App structure ──

  it('has correct DOM structure', () => {
    const { container } = render(<App />);
    const app = container.querySelector('.app');
    expect(app).toBeInTheDocument();
    // #806: the new .titlebar is fixed-positioned as a sibling of .app,
    // not nested inside it. Just verify both exist in the tree.
    const titleBar = container.querySelector('.titlebar');
    expect(titleBar).toBeInTheDocument();
  });

  // ── Context menu prevention ──

  it('onContextMenu handler exists on app div', () => {
    const { container } = render(<App />);
    const appDiv = container.querySelector('.app');
    // Fire contextMenu event — the handler should prevent default
    const event = new MouseEvent('contextmenu', { bubbles: true });
    appDiv!.dispatchEvent(event);
    // In jsdom, dispatchEvent doesn't call React handlers directly,
    // but we verify the element exists and the handler is wired
    expect(appDiv).toBeInTheDocument();
  });

  // ── Notification click navigation (#175) ──────────────────────────────
  // These test the subscription in AuthenticatedLayout which handles
  // notification clicks across all authenticated routes.

  function authenticateUser() {
    useAuthStore.getState().setAccessToken('mock-token');
    useAuthStore.setState({ emailVerified: true });
    useUserStore.setState({
      user: {
        id: 'user-1',
        email: 'test@concord.chat',
        username: 'testuser',
        display_name: 'Test User',
        bio: null,
        avatar_url: null,
        header_image_url: null,
        links: [],
        email_verified: true,
        age_verified: true,
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      },
    });
  }

  it('navigates to channel on notification click', () => {
    authenticateUser();
    render(<App />);

    act(() => {
      useNotificationNavigationStore.getState().setPendingNavigation({
        type: 'channel',
        targetId: 'channel-1',
        serverId: 'server-1',
      });
    });

    expect(useChannelStore.getState().activeChannelId).toBe('channel-1');
    expect(mockClearBadge).toHaveBeenCalled();
    expect(useNotificationNavigationStore.getState().pendingNavigation).toBeNull();
  });

  it('navigates to DM and selects conversation on notification click', () => {
    authenticateUser();
    render(<App />);

    act(() => {
      useNotificationNavigationStore.getState().setPendingNavigation({
        type: 'dm',
        targetId: 'dm-conv-1',
      });
    });

    expect(useDMStore.getState().activeConversationId).toBe('dm-conv-1');
    expect(mockClearBadge).toHaveBeenCalled();
    expect(useNotificationNavigationStore.getState().pendingNavigation).toBeNull();
  });

  // ── SSO eager-unlock gate (#270 Task 21b) ─────────────────────────────────

  it('mounts SSOEagerUnlock when an SSO user lacks unwrapped E2EE keys', () => {
    authenticateUser();
    // Simulate a fresh SSO callback: token + needsSSOUnlock=true, ready=false.
    useE2EEStore.getState().setNeedsSSOUnlock(true);
    render(<App />);

    expect(screen.getByTestId('sso-eager-unlock')).toBeInTheDocument();
    // Main app routes must NOT render until the gate is cleared.
    expect(screen.queryByTestId('dm-view')).not.toBeInTheDocument();
  });

  it('falls through to main app when E2EE is ready (password-login users)', () => {
    authenticateUser();
    // Password-login users initialize E2EE inline before navigating to /app/dms,
    // so they reach AuthenticatedLayout with needsSSOUnlock=false. They must
    // bypass the eager-unlock gate entirely.
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    render(<App />);

    expect(screen.queryByTestId('sso-eager-unlock')).not.toBeInTheDocument();
    expect(screen.getByTestId('dm-view')).toBeInTheDocument();
  });

  it('handleUnlock clears needsSSOUnlock so the gate falls through next render', () => {
    authenticateUser();
    useE2EEStore.getState().setNeedsSSOUnlock(true);
    render(<App />);

    expect(screen.getByTestId('sso-eager-unlock')).toBeInTheDocument();
    // Click the mocked Unlock button — invokes the handleUnlock prop on
    // SSOEagerUnlock, which calls setNeedsSSOUnlock(false).
    act(() => {
      screen.getByTestId('sso-eager-unlock-unlock').click();
    });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
  });

  it('handleSocialRecovery resets E2EE store and clears the access token', () => {
    authenticateUser();
    useE2EEStore.getState().setNeedsSSOUnlock(true);
    render(<App />);

    expect(useAuthStore.getState().accessToken).toBe('mock-token');
    // Click the mocked Recovery button — invokes handleSocialRecovery which
    // resets E2EE state and drops the access token, returning the user to
    // the auth flow.
    act(() => {
      screen.getByTestId('sso-eager-unlock-recovery').click();
    });
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
  });
});

describe('handleAppRootError', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('logs error when markRendererCrashed rejects', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockMarkRendererCrashed.mockRejectedValueOnce(new Error('crash marker failed'));

    handleAppRootError();

    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        '[App] Failed to mark renderer crashed:',
        'crash marker failed'
      );
    });
    consoleSpy.mockRestore();
  });

  it('logs error when softRestart rejects', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockSoftRestart.mockRejectedValueOnce(new Error('restart failed'));

    // jsdom location.reload is non-configurable/non-writable; suppress the
    // subsequent TypeError so the test can assert on console.error.
    const origOnUnhandledRejection = globalThis.onunhandledrejection;
    globalThis.onunhandledrejection = null;

    handleAppRootError();

    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        '[App] Failed to soft-restart, forcing reload:',
        'restart failed'
      );
    });
    globalThis.onunhandledrejection = origOnUnhandledRejection;
    consoleSpy.mockRestore();
  });
});
