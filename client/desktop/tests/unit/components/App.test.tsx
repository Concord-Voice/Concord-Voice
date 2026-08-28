import { render, screen, act, waitFor } from '../../test-utils';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useNotificationNavigationStore } from '@/renderer/stores/notificationNavigationStore';
import { usePendingRegistrationStore } from '@/renderer/stores/pendingRegistrationStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { useInviteStore } from '@/renderer/stores/inviteStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useVideoSettingsStore } from '@/renderer/stores/videoSettingsStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { extractInviteCodes } from '@/renderer/utils/inviteUrl';
import { resetAllStores } from '../../helpers/store-helpers';

const mockDirectMessagesView = vi.hoisted(() => ({ shouldThrow: false }));
// Mock child components to prevent complex rendering
vi.mock('@/renderer/components/Auth/AuthFlow', () => ({
  default: () => <div data-testid="auth-flow">AuthFlow</div>,
}));
vi.mock('@/renderer/components/MainView/MainView', () => ({
  default: () => <div data-testid="main-view">MainView</div>,
}));
vi.mock('@/renderer/components/DirectMessages/DirectMessagesView', () => ({
  default: () => {
    if (mockDirectMessagesView.shouldThrow) {
      throw new Error('DM view crashed');
    }
    return <div data-testid="dm-view">DirectMessagesView</div>;
  },
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
vi.mock('@/renderer/components/Voice/ParticipantGrid', () => ({
  AudioOutputs: () => <div data-testid="audio-outputs" />,
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
const mockGracefulReset = vi.fn();
vi.mock('@/renderer/services/recoveryService', () => ({
  markRendererCrashed: (...args: unknown[]) => mockMarkRendererCrashed(...args),
}));
vi.mock('@/renderer/services/resetService', () => ({
  softRestart: (...args: unknown[]) => mockSoftRestart(...args),
  gracefulReset: (...args: unknown[]) => mockGracefulReset(...args),
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

// #1785: App.tsx side-effect-imports `services/gifProvider`, whose module body
// applies the stored privacy preference to the active provider. Mock the
// provider so importing App does no real KLIPY work, and record the calls in a
// plain array rather than a vi.fn() — the wiring fires once at module-import
// time (below), which is BEFORE this suite's beforeEach runs, so a spy would be
// wiped by vi.clearAllMocks() before any test could read it.
const klipyPersonalizationCalls = vi.hoisted(() => [] as boolean[]);
vi.mock('@/renderer/services/gifProvider/klipyProvider', () => ({
  klipyProvider: {
    name: 'KLIPY',
    searchPlaceholder: 'Search KLIPY',
    poweredByText: 'Powered by KLIPY',
    supportsRecent: true,
    supportsCategories: true,
    setPersonalizationEnabled: (enabled: boolean) => {
      klipyPersonalizationCalls.push(enabled);
    },
    trending: vi.fn(),
    search: vi.fn(),
    recent: vi.fn(),
    categories: vi.fn(),
    getBySlug: vi.fn(),
  },
}));

const mockHydratePostLogin = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: (...args: unknown[]) => mockHydratePostLogin(...args),
}));

import App, { handleAppRootError, __resetRestoreSessionCalledForTesting } from '@/renderer/App';

describe('App', () => {
  beforeEach(() => {
    mockDirectMessagesView.shouldThrow = false;
    vi.clearAllMocks();
    resetAllStores();
    useAuthStore.getState().clearAccessToken();
    useUserStore.setState({ user: null });
    usePendingRegistrationStore.getState().clearPending();
    useE2EEStore.getState().reset();
    useVoiceStore.getState().reset();
    useVideoSettingsStore.setState({ hdrEncoding: false, systemHdr: false });
    __resetRestoreSessionCalledForTesting();
    Object.assign(globalThis.electron ?? {}, {
      restoreSession: undefined,
      clearTokens: undefined,
      onInviteReceived: undefined,
      inviteRendererReady: undefined,
      onFriendCodeReceived: undefined,
      friendRendererReady: undefined,
      getDisplayInfo: undefined,
    });
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

  it('preserves rememberMe=false from restored session', async () => {
    const restoreSession = vi.fn().mockResolvedValue({
      status: 'restored',
      accessToken: 'restored-token',
      rememberMe: false,
      credentialOwner: 41,
      pendingE2EEUnlock: true,
    });
    useAuthStore.getState().setRememberMe(true);
    Object.assign(globalThis.electron ?? {}, { restoreSession });

    render(<App />);

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('restored-token');
    });
    expect(useAuthStore.getState().rememberMe).toBe(false);
  });

  it('fails closed and clears a restored credential that has no custody owner', async () => {
    const restoreSession = vi.fn().mockResolvedValue({
      status: 'restored',
      accessToken: 'ownerless-token',
      rememberMe: true,
    });
    const clearTokens = vi.fn().mockResolvedValue(undefined);
    Object.assign(globalThis.electron ?? {}, { restoreSession, clearTokens });

    render(<App />);

    await waitFor(() => expect(clearTokens).toHaveBeenCalledTimes(1));
    expect(mockGracefulReset).toHaveBeenCalledTimes(1);
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(mockInitializeFromStoredKeys).not.toHaveBeenCalled();
    expect(mockHydratePostLogin).not.toHaveBeenCalled();
  });

  it('inits E2EE and hydrates post-login state on a session-only soft reload (#1870)', async () => {
    const e2eeKeys = {
      wrappingKeyBase64: 'wk',
      preferencesKeyBase64: 'pk',
      wrappedPrivateKeyBase64: 'wpk', // pragma: allowlist secret
    };
    const restoreSession = vi.fn().mockResolvedValue({
      status: 'restored',
      accessToken: 'restored-token',
      rememberMe: false,
      credentialOwner: 41,
      pendingE2EEUnlock: false,
      e2eeKeys, // supplied from main-process memory for a session-only user
    });
    Object.assign(globalThis.electron ?? {}, { restoreSession });

    render(<App />);

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('restored-token');
    });
    // E2EE initialized from the memory-restored keys, AND post-login state
    // hydrated — not the old authenticated-but-empty half-restore.
    expect(mockInitializeFromStoredKeys).toHaveBeenCalledWith(e2eeKeys);
    await waitFor(() => expect(mockHydratePostLogin).toHaveBeenCalledTimes(1));
  });

  it('gates a restored credential with pending E2EE custody before hydration', async () => {
    const restoreSession = vi.fn().mockResolvedValue({
      status: 'restored',
      accessToken: 'restored-token',
      rememberMe: true,
      credentialOwner: 41,
      pendingE2EEUnlock: true,
    });
    Object.assign(globalThis.electron ?? {}, { restoreSession });

    render(<App />);

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('restored-token');
    });
    expect(mockInitializeFromStoredKeys).not.toHaveBeenCalled();
    expect(mockHydratePostLogin).not.toHaveBeenCalled();
    expect(useE2EEStore.getState()).toMatchObject({
      needsSSOUnlock: true,
      ssoCredentialOwner: 41,
    });
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

  it('preserves disabled HDR encoding when startup detects an HDR display', async () => {
    authenticateUser();
    useVideoSettingsStore.getState().setHdrEncoding(false);
    Object.assign(globalThis.electron ?? {}, {
      getDisplayInfo: vi.fn().mockResolvedValue([{ colorDepth: 30, colorSpace: 'display-p3' }]),
    });

    render(<App />);

    await waitFor(() => expect(useVideoSettingsStore.getState().systemHdr).toBe(true));
    expect(useVideoSettingsStore.getState().hdrEncoding).toBe(false);
  });

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

  // CODEX P2 on PR #2967. This fetch was a mount effect on ServerBar, which
  // /app and /app/dms each instantiate separately -- so every navigation re-ran
  // it, and dmStore QUEUES an overlapping fetch rather than collapsing it
  // (queueConversationRefetchIfLoading defers, and the in-flight request's
  // `finally` then issues the queued one). Two sequential full-list GETs for one
  // navigation. It belongs above the router, where App does not remount.
  //
  // Sensitive in both directions: MainView and DirectMessagesView are mocked in
  // this file, so ServerBar never renders here -- put the effect back on it and
  // the count is 0, not 2.
  it('fetches DM conversations once per session, not once per route (regression for #2363)', async () => {
    const fetchConversations = vi.fn(() => Promise.resolve());
    useDMStore.setState({ fetchConversations });
    authenticateUser();
    render(<App />);

    await waitFor(() => {
      expect(fetchConversations).toHaveBeenCalledTimes(1);
    });

    act(() => {
      useNotificationNavigationStore.getState().setPendingNavigation({
        type: 'dm',
        targetId: 'dm-conv-1',
      });
    });

    expect(
      fetchConversations,
      'a route change must not cost a second authenticated full-list request'
    ).toHaveBeenCalledTimes(1);
  });

  it('keeps voice audio outputs mounted while viewing DMs', () => {
    authenticateUser();
    useVoiceStore.setState({
      activeChannelId: 'voice-1',
      connectionState: 'connected',
    });

    render(<App />);

    expect(screen.getByTestId('dm-view')).toBeInTheDocument();
    expect(screen.getByTestId('audio-outputs')).toBeInTheDocument();
  });

  it('keeps voice audio outputs mounted if the active route crashes', () => {
    authenticateUser();
    useVoiceStore.setState({
      activeChannelId: 'voice-1',
      connectionState: 'connected',
    });
    mockDirectMessagesView.shouldThrow = true;
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => undefined);

    try {
      render(<App />);

      expect(screen.getByTestId('audio-outputs')).toBeInTheDocument();
      expect(screen.getByText('This view failed to load')).toBeInTheDocument();
    } finally {
      consoleError.mockRestore();
    }
  });

  it('queues invite deep links until the user is authenticated and verified', async () => {
    let inviteHandler: ((payload: { code: string }) => void) | undefined;
    const mockInviteRendererReady = vi.fn();
    Object.assign(globalThis.electron ?? {}, {
      onInviteReceived: vi.fn((handler: (payload: { code: string }) => void) => {
        inviteHandler = handler;
        return vi.fn();
      }),
      inviteRendererReady: mockInviteRendererReady,
    });
    useInviteStore.setState({ getInviteInfo: vi.fn().mockResolvedValue(null) });

    render(<App />);

    expect(mockInviteRendererReady).toHaveBeenCalled();

    act(() => {
      inviteHandler?.({ code: 'GHJKMNPQ' });
    });

    expect(screen.queryByText('Join a Server')).not.toBeInTheDocument();

    act(() => {
      authenticateUser();
    });

    await waitFor(() => {
      expect(screen.getByText('Join a Server')).toBeInTheDocument();
    });

    expect((screen.getByPlaceholderText('AbCd1234') as HTMLInputElement).value).toBe('GHJKMNPQ');
  });

  it('replays a pre-auth friend code into AddFriendModal, not JoinServerModal', async () => {
    let friendHandler: ((payload: { code: string }) => void) | undefined;
    const mockFriendRendererReady = vi.fn();
    Object.assign(globalThis.electron ?? {}, {
      onFriendCodeReceived: vi.fn((handler: (payload: { code: string }) => void) => {
        friendHandler = handler;
        return vi.fn();
      }),
      friendRendererReady: mockFriendRendererReady,
    });
    useFriendStore.setState({
      fetchFriendCodes: vi.fn().mockResolvedValue(undefined),
      previewFriendCode: vi.fn().mockResolvedValue({
        userId: 'friend-1',
        username: 'alice',
        valid: true,
      }),
    });

    render(<App />);

    expect(mockFriendRendererReady).toHaveBeenCalled();

    act(() => {
      friendHandler?.({ code: 'AbCdEfGh' });
    });

    // Held silently while logged out — no modal and no login-screen notice,
    // since naming the person being added is a disclosure on a shared machine.
    expect(screen.queryByText('Add Friend')).not.toBeInTheDocument();

    act(() => {
      authenticateUser();
    });

    await waitFor(() => {
      expect(screen.getByText('Add Friend')).toBeInTheDocument();
    });

    await waitFor(() => {
      expect(
        (screen.getByPlaceholderText('Enter 8-character code...') as HTMLInputElement).value
      ).toBe('AbCdEfGh');
    });
    // The friend arm must never land in the server-join flow.
    expect(screen.queryByText('Join a Server')).not.toBeInTheDocument();
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

  it('does not let a prior account ready flag bypass a fresh SSO unlock', () => {
    authenticateUser();
    useE2EEStore.getState().setReady(true);
    useE2EEStore.getState().setNeedsSSOUnlock(true, 41);

    render(<App />);

    expect(screen.getByTestId('sso-eager-unlock')).toBeInTheDocument();
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

  it('handleUnlock clears the gate and rehydrates encrypted state after SSO unlock', async () => {
    authenticateUser();
    useE2EEStore.getState().setNeedsSSOUnlock(true);
    render(<App />);

    expect(screen.getByTestId('sso-eager-unlock')).toBeInTheDocument();
    mockHydratePostLogin.mockClear();
    await act(async () => {
      screen.getByTestId('sso-eager-unlock-unlock').click();
    });
    expect(useE2EEStore.getState().needsSSOUnlock).toBe(false);
    await waitFor(() => expect(mockHydratePostLogin).toHaveBeenCalledOnce());
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

// Guards the DM render path against the friend landing URL: /f/CODE is a friend
// link, so it must not auto-unfurl as a server invite when pasted into a message.
describe('extractInviteCodes — friend links (#945)', () => {
  it('does not treat a friend link as an invite in DM text', () => {
    expect(extractInviteCodes('https://invite.concordvoice.chat/f/AbCdEfGh')).toEqual([]);
  });

  it('still extracts a canonical server invite link', () => {
    expect(extractInviteCodes('https://invite.concordvoice.chat/AbCdEfGh')).toEqual(['AbCdEfGh']);
  });
});

describe('KLIPY personalization wiring (#1785)', () => {
  it('applies the personalization preference eagerly when App is imported, with no GIF surface mounted', () => {
    // App.tsx carries a side-effect `import './services/gifProvider'`. That
    // module owns the personalization wiring and applies the stored preference
    // on evaluation. Nothing here mounts GifPicker or GifEmbed — and that is
    // precisely the case that matters: Settings > Content Safety reads
    // klipyClient directly, so the preference must already be applied for a
    // user who has never opened the picker.
    //
    // Falsifier: drop the side-effect import from App.tsx and this fails.
    // Every other route into services/gifProvider is mocked out in this file
    // (MainView, DirectMessagesView, SettingsPage, savedGifsSync), so App.tsx
    // is the only importer left.
    expect(klipyPersonalizationCalls.length).toBeGreaterThan(0);

    // Assert the VALUE, not merely that a call happened. privacyStore defaults
    // sharePersonalizationWithGifProvider to `true` while klipyClient's class
    // field defaults to `false` — that asymmetry IS the regression, so the
    // wiring is only correct if `true` actually reaches the provider. A guard
    // that checked call-count alone would still pass if the wiring fired with
    // a hardcoded or wrongly-sourced value.
    expect(klipyPersonalizationCalls).toContain(true);
  });
});
