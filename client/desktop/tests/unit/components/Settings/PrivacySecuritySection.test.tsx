import { render, screen, fireEvent, within, act, userEvent } from '../../../test-utils';
import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { useDraftSettingsStore } from '@/renderer/stores/ui/draftSettingsStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { onTestFinished, vi } from 'vitest';
import { deferred } from '../../../helpers/deferred';

const gifIdMock = vi.hoisted(() => ({
  currentId: 'mock-customer-id-123' as string | null,
  listeners: new Set<() => void>(),
}));

const mockApiFetch = vi.fn();
// `GET /api/v1/mfa/step-up`, served by path like the GETs below: the session and
// backup-code dialogs read it when they open, and a queued `*Once` response would
// be consumed by it instead of the request the case is about.
const mockStepUpRead = vi.fn();
// SSO-identities GET fixture for LinkedAccountsList (issue #270 / Task 20).
// Defined before the vi.mock factory so the factory can capture it; the
// factory is hoisted to the top of the file by vi.mock semantics. Tracked
// through `mockSsoIdentitiesFetch` so individual tests can opt into a
// non-empty list without touching the general fixture queue.
const mockSsoIdentitiesFetch = vi.fn(async () => ({
  ok: true,
  status: 200,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  json: async () => ({ identities: [] }),
  text: async () => JSON.stringify({ identities: [] }),
}));
// Mount-time hydration GET for the SSO security toggles (follow-up #1 from
// PR #808). Short-circuited like /sso-identities so it doesn't consume
// entries from the FIFO `mockResolvedValueOnce` queue used by the rest of
// the suite. Individual tests can swap out `mockSecurityGetFetch` to
// simulate server-side ON state.
const mockSecurityGetFetch = vi.fn(async () => ({
  ok: true,
  status: 200,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  json: async () => ({ password_login_disabled: false, trust_sso_security: false }),
  text: async () => JSON.stringify({ password_login_disabled: false, trust_sso_security: false }),
}));
// Mount-time hydration GET for the custom-status visibility tier (#1233 B6,
// PresenceSettingsSection). Short-circuited like the SSO/identities GETs above
// so its mount fetch doesn't consume FIFO `mockResolvedValueOnce` entries or
// inflate `mockApiFetch` call-count assertions in the pre-existing tests.
const mockPresenceGetFetch = vi.fn(async () => ({
  ok: true,
  status: 200,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  json: async () => ({
    master_enabled: true,
    server_voice_tier: 1,
    server_voice_show_details: true,
    private_call_tier: 0,
    private_call_show_details: false,
    custom_text_tier: 0,
    custom_text: null,
    custom_text_emoji: null,
  }),
  text: async () =>
    JSON.stringify({
      master_enabled: true,
      server_voice_tier: 1,
      server_voice_show_details: true,
      private_call_tier: 0,
      private_call_show_details: false,
      custom_text_tier: 0,
      custom_text: null,
      custom_text_emoji: null,
    }),
}));
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => {
    // Short-circuit LinkedAccountsList's GET and the hydration GET so they
    // don't consume entries from the existing FIFO `mockResolvedValueOnce`
    // queue used by all the pre-existing session/MFA tests. Survives
    // mockReset() because it's wired at the module factory layer, not on
    // `mockApiFetch` itself. Note: this only matches the GET (`init` is
    // undefined or method is GET) — the PATCH still falls through to
    // `mockApiFetch` and is asserted by the SSO toggle tests.
    const [path, init] = args;
    const method = (init as RequestInit | undefined)?.method ?? 'GET';
    if (path === '/api/v1/users/me/sso-identities') {
      return mockSsoIdentitiesFetch();
    }
    if (path === '/api/v1/mfa/step-up') {
      return mockStepUpRead();
    }
    if (path === '/api/v1/users/me/security' && method === 'GET') {
      return mockSecurityGetFetch();
    }
    if (path === '/api/v1/users/me/presence-settings' && method === 'GET') {
      return mockPresenceGetFetch();
    }
    // The friend store's mount fetch (PresenceExceptions, RecoveryCircle) runs
    // until the store is hydrated, so it used to fire in whichever case ran
    // first and take that case's sessions response: the first case to run
    // alone then read its MFA status from the wrong reply.
    if (path === '/api/v1/friends' && method === 'GET') {
      return Promise.resolve({ ok: true, status: 200, json: async () => ({ friends: [] }) });
    }
    return mockApiFetch(...args);
  },
  API_BASE: 'http://localhost:8080',
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
}));
vi.mock('@/renderer/stores/auth/authStore', () => ({
  useAuthStore: Object.assign(
    vi.fn((s) => s({ accessToken: 'mock-token', authGeneration: 0 })),
    {
      subscribe: vi.fn(),
      getState: vi.fn(() => ({ accessToken: 'mock-token', authGeneration: 0 })),
    }
  ),
}));
vi.mock('@/renderer/stores/auth/userStore', () => ({
  // `getState` is read by the step-up hook when a run begins (the account the
  // answer belongs to).
  useUserStore: Object.assign(
    vi.fn((s) => s({ logout: vi.fn() })),
    { getState: vi.fn(() => ({ user: { id: 'user-1' } })) }
  ),
}));
const mockFetchPrivacy = vi.fn().mockResolvedValue(undefined);
const mockUpdatePrivacy = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/stores/ui/privacyStore', () => ({
  usePrivacyStore: vi.fn((s) =>
    s({
      settings: {
        messagesFriendsOnly: true,
        messagesServerMembers: true,
        dmPrivacyLevel: 2 as const,
        dmFriendsOfFriends: false,
        autoAcceptFriendCodes: false,
        searchableByUsername: false,
        searchableByEmail: false,
        searchableByPhone: false,
        allowEmbeddedContent: false,
        loadGifsAutomatically: true,
        sharePersonalizationWithGifProvider: true,
      },
      // #1241: the friend-request control disables itself until the server has
      // confirmed the settings at least once. Without this the control renders
      // disabled, clicks are no-ops, and any test that drives it passes for the
      // wrong reason.
      loaded: true,
      fetchPrivacy: mockFetchPrivacy,
      updatePrivacy: mockUpdatePrivacy,
    })
  ),
  DMPrivacyLevel: {},
}));
// The selector mock above is a bare vi.fn(), so it carries no `getState`.
// commitPrivacyTier's rejection path reads usePrivacyStore.getState() to find
// the last server-confirmed value, and without this the call threw inside the
// catch and swallowed the error the test was asserting on.
vi.mocked(usePrivacyStore).getState = vi.fn(() => ({
  settings: { dmPrivacyLevel: 2, allowFriendRequestsFrom: 'everyone' },
})) as unknown as typeof usePrivacyStore.getState;
vi.mock('@/renderer/stores/voice/osPermissionStore', () => ({
  useOsPermissionStore: vi.fn((s) =>
    s({
      microphone: 'granted',
      camera: 'granted',
      screen: 'granted',
      secureStorage: 'granted',
      notifications: 'granted',
      isLoaded: true,
      fetchAll: vi.fn().mockResolvedValue(undefined),
      requestOne: vi.fn().mockResolvedValue('granted'),
      openSettings: vi.fn().mockResolvedValue(undefined),
    })
  ),
}));
vi.mock('@/renderer/services/messaging/gifProvider/klipyClient', () => ({
  klipyClient: {
    getCurrentCustomerId: vi.fn(() => gifIdMock.currentId),
    getCustomerID: vi.fn(() => Promise.resolve(gifIdMock.currentId)),
    setPersonalizationEnabled: vi.fn(),
    subscribeCustomerId: vi.fn((listener: () => void) => {
      gifIdMock.listeners.add(listener);
      return () => gifIdMock.listeners.delete(listener);
    }),
    rotateCustomerId: vi.fn(async () => {
      gifIdMock.currentId = 'mock-rotated-id-456';
      for (const listener of gifIdMock.listeners) listener();
      return gifIdMock.currentId;
    }),
  },
}));
vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  // Enhanced (#2017): expose the setup callbacks as buttons so tests can drive
  // mfaSetupMethod into its email-sms / non-email branches (renderMfaSetupArea).
  default: (props: {
    onSetupTOTP: () => void;
    onSetupEmailSms: () => void;
    onSetupWebAuthn: (t: 'hardware' | 'platform') => void;
  }) => (
    <div data-testid="mfa-tier-selector">
      MFATierSelector
      <button data-testid="stub-setup-totp" onClick={() => props.onSetupTOTP()}>
        setup totp
      </button>
      <button data-testid="stub-setup-emailsms" onClick={() => props.onSetupEmailSms()}>
        setup email-sms
      </button>
      <button data-testid="stub-setup-webauthn" onClick={() => props.onSetupWebAuthn('hardware')}>
        setup webauthn
      </button>
    </div>
  ),
  WebAuthnCredential: {},
}));
vi.mock('@/renderer/components/Settings/MFASetup', () => ({
  default: (props: { onComplete: () => void; onCancel: () => void }) => (
    <div data-testid="mfa-setup">
      MFASetup
      <button data-testid="mfa-setup-complete" onClick={() => props.onComplete()}>
        complete
      </button>
      <button data-testid="mfa-setup-cancel" onClick={() => props.onCancel()}>
        cancel
      </button>
    </div>
  ),
}));
vi.mock('@/renderer/components/Settings/BackupCodeDisplay', () => ({
  default: (props: { onConfirm: () => void }) => (
    <div data-testid="backup-code-display">
      BackupCodeDisplay
      <button data-testid="backup-code-confirm" onClick={() => props.onConfirm()}>
        confirm
      </button>
    </div>
  ),
}));
vi.mock('@/renderer/components/Settings/EmailSmsSetup', () => ({
  default: (props: { onComplete: () => void; onCancel: () => void }) => (
    <div data-testid="email-sms-setup">
      EmailSmsSetup
      <button data-testid="email-sms-complete" onClick={() => props.onComplete()}>
        complete
      </button>
      <button data-testid="email-sms-cancel" onClick={() => props.onCancel()}>
        cancel
      </button>
    </div>
  ),
}));
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: ({ size }: { size?: string }) => (
    <div data-testid="loading-spinner" data-size={size}>
      Loading...
    </div>
  ),
}));
vi.mock('@/renderer/components/Settings/ActivityHistoryCard', () => ({
  default: () => <div aria-label="Activity History controls" />,
}));
vi.mock('@/renderer/components/Profile/PresenceHistorySection', () => ({
  default: () => <section aria-label="Activity History feed" />,
}));
vi.mock('@/renderer/components/ui/Modal', () => ({
  default: ({
    isOpen,
    children,
    onClose,
    title,
  }: {
    isOpen: boolean;
    children: React.ReactNode;
    onClose: () => void;
    title?: string;
  }) =>
    isOpen ? (
      <div data-testid="modal" role="dialog">
        {title && <h2>{title}</h2>}
        {children}
        <button onClick={onClose}>Close</button>
      </div>
    ) : null,
}));

vi.mock('@/renderer/stores/ui/clientConfigStore', () => ({
  useClientConfigStore: vi.fn((s) =>
    s({ activityHistoryCapability: { status: 'confirmed-unsupported' } })
  ),
}));

import PrivacySecuritySection, {
  resolveSSOToggleError,
} from '@/renderer/components/Settings/PrivacySecuritySection';

// Named fixture: the pre-commit detect-secrets hook flags a credential-shaped key
// beside a quoted literal regardless of the value.
const FIXTURE_PW = 'fixture-password-do-not-persist';

describe('resolveSSOToggleError', () => {
  it('maps invalid_credentials to a passphrase error', () => {
    expect(resolveSSOToggleError('invalid_credentials')).toBe('Incorrect passphrase.');
  });

  it('maps would_lock_out to the lock-out warning', () => {
    expect(resolveSSOToggleError('would_lock_out')).toBe(
      'That change would lock you out. Link an SSO provider first.'
    );
  });

  it('falls back to a generic message for unknown or missing codes', () => {
    expect(resolveSSOToggleError('something_else')).toBe('Failed to update security setting.');
    expect(resolveSSOToggleError(undefined)).toBe('Failed to update security setting.');
  });
});

// Drain the *Once queues: vi.clearAllMocks() does not, so an unconsumed queued value
// is served to the next test. See [internal]rules/tests.md § The *Once queue outlives
// the test that queued it. This file's own beforeEach queues three responses per
// test, so any case rendering fewer than three fetches leaves the remainder behind
// and shifts the next test's whole sequence by one — across a describe boundary too,
// which is why every top-level describe calls this rather than just the first.
// mockReset() restores an implementation given to vi.fn(impl); a default attached
// afterwards with .mockResolvedValue() is wiped and re-established here.
function drainOnceQueues(): void {
  mockApiFetch.mockReset();
  mockSecurityGetFetch.mockReset();
  mockUpdatePrivacy.mockReset();
  mockUpdatePrivacy.mockResolvedValue(undefined);
  readOffers([]);
}

type Reply = { ok: boolean; status: number; json: () => Promise<unknown> };
const reply = (status: number, body: unknown = {}): Reply => ({
  ok: status >= 200 && status < 300,
  status,
  json: async () => body,
});

/** What `GET /api/v1/mfa/step-up` answers: the inline methods the account can use. */
function readOffers(methods: string[], backupCodeAvailable = false): void {
  mockStepUpRead.mockReset().mockImplementation(async () =>
    reply(200, {
      methods,
      default_method: methods[0] ?? null,
      backup_code_available: backupCodeAvailable,
    })
  );
}

const sessionRow = (id: string, isCurrent: boolean) => ({
  id,
  device_name: 'Device',
  ip_address: '5.6.7.8',
  user_agent: 'Mozilla/5.0 Chrome/100',
  expires_at: '2026-12-01T00:00:00Z',
  created_at: '2026-02-01T00:00:00Z',
  last_used: new Date().toISOString(),
  is_current: isCurrent,
});

interface SectionOptions {
  sessions?: ReturnType<typeof sessionRow>[];
  /** `users.mfa_methods`, as `/mfa/status` reports it: NOT what a step-up accepts (C2). */
  mfaMethods?: string[];
  /** Every call that is not a GET; the default accepts it. */
  onWrite?: (path: string, init: RequestInit) => Reply | Promise<Reply>;
  /** Every `GET /api/v1/sessions` after the mount's; the default lists `sessions` again. */
  relist?: () => Reply | Promise<Reply>;
}

/** Answers the mount GETs by path, so a case never depends on fetch order. */
function serveSection({
  sessions = [],
  mfaMethods = [],
  onWrite,
  relist,
}: SectionOptions = {}): void {
  let listed = 0;
  mockApiFetch.mockReset().mockImplementation(async (path: string, init?: RequestInit) => {
    if (init?.method && init.method !== 'GET') return onWrite ? onWrite(path, init) : reply(200);
    if (path === '/api/v1/sessions') {
      if (++listed > 1 && relist) return relist();
      return reply(200, { sessions, past_sessions: [], revocation_mode: 'secure' });
    }
    if (path === '/api/v1/mfa/status') {
      return reply(200, {
        methods: mfaMethods,
        recovery_only_methods: [],
        recovery_hardened: false,
        backup_codes_remaining: mfaMethods.length > 0 ? 5 : 0,
        backup_email: '',
      });
    }
    if (path === '/api/v1/mfa/webauthn/credentials') return reply(200, { credentials: [] });
    return reply(404);
  });
}

/** How many times the sessions list was read. */
const sessionListReads = () =>
  mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/sessions').length;

/** Signs in as another account: the auth generation moves on until the case ends. */
async function switchAccount(): Promise<void> {
  const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
  const getState = vi.mocked(useAuthStore.getState);
  const original = getState.getMockImplementation();
  getState.mockImplementation(() => ({ accessToken: 'mock-token', authGeneration: 1 }) as never);
  onTestFinished(() => {
    if (original) getState.mockImplementation(original);
  });
}

/** The parsed JSON bodies sent to `path`, in order. */
function bodiesTo(path: string): Record<string, unknown>[] {
  return mockApiFetch.mock.calls
    .filter((c) => c[0] === path && typeof (c[1] as RequestInit | undefined)?.body === 'string')
    .map((c) => JSON.parse((c[1] as { body: string }).body) as Record<string, unknown>);
}

const primaryOf = (name: string) => screen.getByRole('button', { name });

describe('PrivacySecuritySection', () => {
  beforeEach(() => {
    gifIdMock.currentId = 'mock-customer-id-123';
    gifIdMock.listeners.clear();
    vi.clearAllMocks();
    drainOnceQueues();
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: [],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 0,
          backup_email: '',
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
  });

  it('renders privacy section heading', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Privacy')).toBeInTheDocument());
  });
  it('places Activity History immediately before Active Sessions', async () => {
    render(<PrivacySecuritySection />);

    await vi.waitFor(() =>
      expect(document.getElementById('section-presence-history')).not.toBeNull()
    );
    const history = document.getElementById('section-presence-history')!;
    const sessions = document.getElementById('section-active-sessions')!;
    expect(screen.getByLabelText('Activity History controls')).toBeInTheDocument();
    expect(screen.getByLabelText('Activity History feed')).toBeInTheDocument();
    expect(
      history.compareDocumentPosition(sessions) & Node.DOCUMENT_POSITION_FOLLOWING
    ).toBeTruthy();
  });
  it('renders privacy description', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(
        screen.getByText('Control who can message you and how others can find you.')
      ).toBeInTheDocument()
    );
  });
  it('renders DM privacy labels', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());
    // Scope the label lookups to the DM tier control. "Friends" is now also a
    // presence-tier option (#1233 PresenceSettingsSection composed alongside),
    // so a bare getByText('Friends') is ambiguous — query the DM tier labels.
    const dmTierLabels = Array.from(
      document.querySelectorAll<HTMLButtonElement>('.settings-tier-label')
    ).map((el) => el.textContent);
    expect(dmTierLabels).toContain('No One');
    expect(dmTierLabels).toContain('Friends');
    expect(dmTierLabels).toContain('Everyone');
  });
  it('renders friends-of-friends toggle', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Allow Friends-of-Friends')).toBeInTheDocument()
    );
  });
  it('explains Rich Presence expansion for Friends tier', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(
        screen.getByText(
          'This also expands who can see your rich presence when set to Friends tier.'
        )
      ).toBeInTheDocument()
    );
  });
  it('renders auto-accept friend codes toggle', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Auto-Accept Friend Requests from Codes')).toBeInTheDocument()
    );
  });
  it('renders search visibility settings', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Search Visibility')).toBeInTheDocument());
    expect(screen.getByText('Searchable by Username')).toBeInTheDocument();
    expect(screen.getByText('Searchable by Email')).toBeInTheDocument();
    expect(screen.getByText('Searchable by Phone Number')).toBeInTheDocument();
  });
  it('renders content safety', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Allow Embedded Content')).toBeInTheDocument());
  });
  it('toggles searchable by username', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Searchable by Username')).toBeInTheDocument());
    fireEvent.click(
      screen
        .getByText('Searchable by Username')
        .closest('.settings-row')!
        .querySelector('input[type="checkbox"]')!
    );
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ searchableByUsername: true });
  });
  it('toggles searchable by email', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Searchable by Email')).toBeInTheDocument());
    fireEvent.click(
      screen
        .getByText('Searchable by Email')
        .closest('.settings-row')!
        .querySelector('input[type="checkbox"]')!
    );
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ searchableByEmail: true });
  });
  it('toggles searchable by phone', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Searchable by Phone Number')).toBeInTheDocument()
    );
    fireEvent.click(
      screen
        .getByText('Searchable by Phone Number')
        .closest('.settings-row')!
        .querySelector('input[type="checkbox"]')!
    );
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ searchableByPhone: true });
  });
  it('toggles embedded content', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Allow Embedded Content')).toBeInTheDocument());
    fireEvent.click(
      screen
        .getByText('Allow Embedded Content')
        .closest('.settings-row')!
        .querySelector('input[type="checkbox"]')!
    );
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ allowEmbeddedContent: true });
  });
  it('toggles auto-accept friend codes', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Auto-Accept Friend Requests from Codes')).toBeInTheDocument()
    );
    fireEvent.click(
      screen
        .getByText('Auto-Accept Friend Requests from Codes')
        .closest('.settings-row')!
        .querySelector('input[type="checkbox"]')!
    );
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ autoAcceptFriendCodes: true });
  });
  it('highlights current DM privacy level', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Friends + Server')).toHaveAttribute('aria-pressed', 'true')
    );
  });
  it('renders system permissions section', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('System Permissions')).toBeInTheDocument());
  });
  it('System Permissions section is collapsed by default (#4)', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('System Permissions')).toBeInTheDocument());
    const details = screen.getByText('System Permissions').closest('details');
    expect(details?.hasAttribute('open')).toBe(false);
  });
  it('shows granted badges', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Microphone')).toBeInTheDocument());
    expect(screen.getAllByText('Granted').length).toBe(5);
  });
  it('renders MFA section', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Multi-Factor Authentication')).toBeInTheDocument()
    );
  });
  it('renders MFA description', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText(/Add an extra layer of security/)).toBeInTheDocument()
    );
  });
  it('renders security keys counter', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Security Keys:/)).toBeInTheDocument());
  });
  it('renders active sessions section', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Active Sessions')).toBeInTheDocument());
  });
  it('shows session description', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText(/These are the devices currently logged/)).toBeInTheDocument()
    );
  });
  it('renders sessions from API', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '192.168.1.x',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('This Device')).toBeInTheDocument());
    expect(screen.getByText('Concord Voice Desktop')).toBeInTheDocument();
  });
  it('calls fetchPrivacy on mount', () => {
    render(<PrivacySecuritySection />);
    expect(mockFetchPrivacy).toHaveBeenCalled();
  });
  it('fetches sessions on mount', () => {
    render(<PrivacySecuritySection />);
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/sessions');
  });
  it('fetches MFA status on mount', () => {
    render(<PrivacySecuritySection />);
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/mfa/status');
  });
  it('shows session fetch error', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: false,
        json: async () => ({ error: 'Failed to fetch sessions' }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ methods: [] }) })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Failed to fetch sessions')).toBeInTheDocument()
    );
  });

  // ── Session revocation mode ──────────────────────────────────────────────

  it('renders revocation mode toggle after loading', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());
    expect(screen.getByText('Secure')).toBeInTheDocument();
    expect(screen.getByText('Simple')).toBeInTheDocument();
  });

  it('shows Secure mode description when revocationMode is secure', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText(/Authentication via Password or MFA is required/)).toBeInTheDocument()
    );
  });

  // ── Session with multiple sessions and Revoke All ────────────────────────

  it('renders Revoke All Sessions button when sessions exist', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Revoke All Sessions')).toBeInTheDocument());
  });

  it('renders Revoke button for non-current sessions', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => {
      const revokeButtons = screen.getAllByText('Revoke');
      expect(revokeButtons.length).toBeGreaterThanOrEqual(2);
    });
  });

  it('shows confirmation for revoking current session', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('This Device')).toBeInTheDocument());
    // Click Revoke on current session — should show confirmation
    fireEvent.click(screen.getAllByText('Revoke')[0]);
    await vi.waitFor(() =>
      expect(screen.getByText(/This is your current active session/)).toBeInTheDocument()
    );
  });

  // ── User agent parsing ───────────────────────────────────────────────────

  it('parses Chrome user agent', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Test',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());
  });

  it('parses Firefox user agent', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Test',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Firefox/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Firefox Browser')).toBeInTheDocument());
  });

  // ── DM privacy descriptions ──────────────────────────────────────────────

  it('renders DM privacy level description for Friends + Server', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Friends + Server Members')).toBeInTheDocument()
    );
  });

  // ── MFA status display ───────────────────────────────────────────────────

  it('shows Requires MFA for backup codes when no MFA active', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Requires MFA')).toBeInTheDocument());
  });

  it('shows backup code count when MFA is active', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 5,
          backup_email: '',
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('5')).toBeInTheDocument());
  });

  // ── MFA tier selector rendering ──────────────────────────────────────────

  it('renders MFATierSelector when no setup method is active', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByTestId('mfa-tier-selector')).toBeInTheDocument());
  });

  // ── MFA setup area + backup-reset modal ──────────────────────────────────
  // Coverage for the #2017 render-helper extraction (renderMfaSetupArea /
  // renderBackupResetModal). These MFA states were previously untested.

  // Completing a wizard refetches the MFA status. Since F8 a failed refetch
  // is reported and hides the tier controls, so these cases answer it.
  const queueStatusRefetch = () =>
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: ['totp'], recovery_only_methods: [] }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });

  it('renders EmailSmsSetup when the email-sms setup method is selected', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByTestId('mfa-tier-selector')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('stub-setup-emailsms'));
    expect(screen.getByTestId('email-sms-setup')).toBeInTheDocument();
    expect(screen.queryByTestId('mfa-tier-selector')).not.toBeInTheDocument();
    // completing returns to the tier selector (covers the onComplete callback)
    queueStatusRefetch();
    fireEvent.click(screen.getByTestId('email-sms-complete'));
    expect(await screen.findByTestId('mfa-tier-selector')).toBeInTheDocument();
  });

  it('renders MFASetup when a non-email setup method is selected', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByTestId('mfa-tier-selector')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('stub-setup-totp'));
    expect(screen.getByTestId('mfa-setup')).toBeInTheDocument();
    expect(screen.queryByTestId('mfa-tier-selector')).not.toBeInTheDocument();
    // cancelling returns to the tier selector (covers the onCancel callback)
    fireEvent.click(screen.getByTestId('mfa-setup-cancel'));
    expect(await screen.findByTestId('mfa-tier-selector')).toBeInTheDocument();
  });

  it('renders MFASetup for a webauthn setup and completes it', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByTestId('mfa-tier-selector')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('stub-setup-webauthn'));
    expect(screen.getByTestId('mfa-setup')).toBeInTheDocument();
    // completing returns to the tier selector (covers the onComplete callback)
    queueStatusRefetch();
    fireEvent.click(screen.getByTestId('mfa-setup-complete'));
    expect(await screen.findByTestId('mfa-tier-selector')).toBeInTheDocument();
  });

  // The "Back to …" return leaves App Settings, which tears its unapplied
  // changes down: while one is pending the button holds, as "Back to app" does.
  describe('the verification return with an unapplied settings change', () => {
    const NO_DRAFTS = { appearance: {}, audio: {}, video: {}, tts: {} };

    async function finishSetupWithReturnPending() {
      useSettingsOverlayStore.setState({ open: 'app', verificationReturn: { kind: 'chat' } });
      onTestFinished(() => {
        useSettingsOverlayStore.setState({ open: null, verificationReturn: null });
        useDraftSettingsStore.setState({ drafts: NO_DRAFTS });
      });
      render(<PrivacySecuritySection />);
      await screen.findByTestId('mfa-tier-selector');
      fireEvent.click(screen.getByTestId('stub-setup-totp'));
      queueStatusRefetch();
      fireEvent.click(screen.getByTestId('mfa-setup-complete'));
      return screen.findByRole('button', { name: 'Back to chat' });
    }

    // Mutant: the button's own `!pendingChanges` guard or its aria-disabled dropped (the helper alone would still hold it, silently).
    it('stays focusable, says why, and goes nowhere', async () => {
      const back = await finishSetupWithReturnPending();
      act(() => {
        useDraftSettingsStore.setState({ drafts: { ...NO_DRAFTS, appearance: { uiScale: 1.25 } } });
      });

      expect(back).toHaveAttribute('aria-disabled', 'true');
      expect(back).toHaveAccessibleDescription('Apply or revert your settings changes first.');
      await userEvent.click(back);

      expect(useSettingsOverlayStore.getState().open).toBe('app');
      expect(useSettingsOverlayStore.getState().verificationReturn).toEqual({ kind: 'chat' });
    });

    // Control: once the change is applied or reverted the same button goes back.
    it('goes back once nothing is pending', async () => {
      const back = await finishSetupWithReturnPending();

      expect(back).not.toHaveAttribute('aria-disabled');
      expect(screen.queryByText('Apply or revert your settings changes first.')).toBeNull();
      await userEvent.click(back);

      await vi.waitFor(() => expect(useSettingsOverlayStore.getState().open).toBeNull());
    });
  });

  it('opens the backup-code reset modal and shows the credential form', async () => {
    serveSection({ mfaMethods: ['totp'] });
    render(<PrivacySecuritySection />);
    fireEvent.click(await screen.findByRole('button', { name: /reset\s*codes/i }));
    expect(screen.getByText('Reset Backup Codes')).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
    expect(screen.getByLabelText('Authenticator app code')).toBeInTheDocument();
  });

  it('regenerates backup codes and shows the code display', async () => {
    serveSection({
      mfaMethods: ['totp'],
      onWrite: () => reply(200, { backup_codes: ['aaaa-bbbb', 'cccc-dddd'] }),
    });
    render(<PrivacySecuritySection />);
    fireEvent.click(await screen.findByRole('button', { name: /reset\s*codes/i }));
    await userEvent.type(screen.getByLabelText('Password'), FIXTURE_PW);
    await userEvent.type(screen.getByLabelText('Authenticator app code'), '123456');
    await userEvent.click(screen.getByRole('button', { name: /regenerate codes/i }));
    await vi.waitFor(() => expect(screen.getByTestId('backup-code-display')).toBeInTheDocument());
    // The route binds the code as `code`, not the seam's `mfa_code`.
    expect(bodiesTo('/api/v1/mfa/backup-codes/regenerate')).toEqual([
      { password: FIXTURE_PW, code: '123456' },
    ]);
    // confirming closes the modal (covers the BackupCodeDisplay onConfirm callback)
    fireEvent.click(screen.getByTestId('backup-code-confirm'));
    await vi.waitFor(() =>
      expect(screen.queryByTestId('backup-code-display')).not.toBeInTheDocument()
    );
  });

  // ── Permission status badges ─────────────────────────────────────────────

  it('shows permission labels', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Microphone')).toBeInTheDocument());
    expect(screen.getByText('Camera')).toBeInTheDocument();
    expect(screen.getByText('Screen Recording')).toBeInTheDocument();
    expect(screen.getByText('Notifications')).toBeInTheDocument();
    expect(screen.getByText(/Secure Storage/)).toBeInTheDocument();
  });

  it('shows permission descriptions', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Used for voice channels and calls.')).toBeInTheDocument()
    );
    expect(screen.getByText('Used for video in voice channels and calls.')).toBeInTheDocument();
  });

  // ── Revoke All modal password input ──────────────────────────────────────

  // ── Permission status badge mapping ──────────────────────────────────────

  it('shows Denied badge for denied permission', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'denied',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Denied')).toBeInTheDocument());
  });

  it('shows Restricted badge for restricted permission', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'restricted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Restricted')).toBeInTheDocument());
  });

  it('shows Not Requested badge for not-determined permission', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'not-determined',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Not Requested')).toBeInTheDocument());
  });

  it('shows Unavailable badge for unavailable permission', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'unavailable',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Unavailable')).toBeInTheDocument());
  });

  // ── System Permissions: every row has a navigable action (#1743) ─────────

  it('shows "Open System Settings" for every granted row and no dead ends (#1743)', async () => {
    // Set the all-granted impl explicitly — vi.clearAllMocks() does NOT reset a
    // mockImplementation a prior test installed, so the default factory mock does
    // not reliably leak through as all-granted when this test runs mid-suite.
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Microphone')).toBeInTheDocument());
    expect(screen.getAllByText('Open System Settings').length).toBe(5);
    // Status is still shown as a read-only badge, not a toggle.
    expect(screen.getAllByText('Granted').length).toBe(5);
    // The OS-managed nature is made explicit per row.
    expect(screen.getAllByText('Managed by your operating system.').length).toBe(5);
  });

  it('shows "Request" (not "Open System Settings") for a not-determined row (#1743)', async () => {
    const requestOneMock = vi.fn().mockResolvedValue('granted');
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'not-determined',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: requestOneMock,
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Request')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Request'));
    await vi.waitFor(() => expect(requestOneMock).toHaveBeenCalledWith('microphone'));
  });

  it('"Open System Settings" calls openSettings for the row type (#1743)', async () => {
    const openSettingsMock = vi.fn().mockResolvedValue(undefined);
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'denied',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: openSettingsMock,
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Denied')).toBeInTheDocument());
    // Scope to the Microphone row — row order is secureStorage, microphone, …, so
    // a bare index 0 would be secureStorage.
    const micRow = screen.getByText('Microphone').closest('.settings-row')!;
    const micBtn = Array.from(micRow.querySelectorAll('button')).find(
      (b) => b.textContent === 'Open System Settings'
    )!;
    fireEvent.click(micBtn);
    await vi.waitFor(() => expect(openSettingsMock).toHaveBeenCalledWith('microphone'));
  });

  it('does not crash if openSettings rejects (#1743)', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockRejectedValue(new Error('no settings panel')),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getAllByText('Open System Settings').length).toBe(5));
    expect(() => fireEvent.click(screen.getAllByText('Open System Settings')[0])).not.toThrow();
  });

  // ── Past sessions rendering ─────────────────────────────────────────────

  it('renders past sessions when they exist', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [],
          past_sessions: [
            {
              id: 'ps1',
              device_name: 'Old Device',
              ip_address: '10.0.0.1',
              user_agent: 'Mozilla/5.0 Firefox/100',
              created_at: '2026-01-01T00:00:00Z',
              last_used: '2026-01-15T00:00:00Z',
              revoked_at: '2026-01-20T00:00:00Z',
            },
          ],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Past Sessions')).toBeInTheDocument());
    expect(screen.getByText('Revoked')).toBeInTheDocument();
  });

  // ── Session revoke flow (non-current) ───────────────────────────────────

  it('revokes a non-current session successfully', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Safari/600',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'simple',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Safari Browser')).toBeInTheDocument());

    // Mock the DELETE call for session revoke + subsequent fetchSessions
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) }).mockResolvedValueOnce({
      ok: true,
      json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'simple' }),
    });

    // Click Revoke on the non-current session (second revoke button)
    const revokeButtons = screen.getAllByText('Revoke');
    fireEvent.click(revokeButtons[1]);

    // Sent under the capture its answer is checked against.
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/s2',
        expect.objectContaining({ method: 'DELETE' }),
        { context: expect.objectContaining({ authLifecycle: expect.anything() }) }
      )
    );
  });

  // ── Session revoke 403 password_required flow ───────────────────────────

  it('shows password modal on 403 password_required', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());

    // Mock 403 response
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'password_required' }),
    });

    fireEvent.click(screen.getByText('Revoke'));

    await vi.waitFor(() => expect(screen.getByText('Verify Your Identity')).toBeInTheDocument());
  });

  // ── MFA enabled state ──────────────────────────────────────────────────

  it('shows backup code count and Reset button when MFA active', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 7,
          backup_email: 'test@example.com',
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('7')).toBeInTheDocument());
    const resetBtn = screen.getByText(/Reset/);
    expect(resetBtn).not.toBeDisabled();
  });

  it('disables Reset Codes button when no MFA is active', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Requires MFA')).toBeInTheDocument());
    const resetBtn = screen.getByText(/Reset/);
    expect(resetBtn).toBeDisabled();
  });

  // ── DM privacy level descriptions ──────────────────────────────────────

  it('renders DM description for level 0 (No One)', async () => {
    vi.mocked(
      await import('@/renderer/stores/ui/privacyStore').then((m) => m.usePrivacyStore)
    ).mockImplementation((s) =>
      s({
        settings: {
          messagesFriendsOnly: true,
          messagesServerMembers: true,
          dmPrivacyLevel: 0 as const,
          dmFriendsOfFriends: false,
          autoAcceptFriendCodes: false,
          searchableByUsername: false,
          searchableByEmail: false,
          searchableByPhone: false,
          allowEmbeddedContent: false,
        },
        loaded: true,
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Hermit mode/)).toBeInTheDocument());
    expect(screen.getByText(/DMs are disabled/)).toBeInTheDocument();
  });

  // ── Revoke All modal ───────────────────────────────────────────────────

  it('opens Revoke All modal when button clicked', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Revoke All Sessions')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Revoke All Sessions'));
    await vi.waitFor(() =>
      expect(screen.getByText(/revoke all of your active session tokens/)).toBeInTheDocument()
    );
  });

  // ── GIF settings rendering ─────────────────────────────────────────────

  it('renders GIF auto-load toggle', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Load GIFs from KLIPY automatically')).toBeInTheDocument()
    );
  });

  it('renders GIF personalization toggle', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Share GIF personalization with provider')).toBeInTheDocument()
    );
  });

  it('renders personalization ID row with customer ID', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Personalization ID')).toBeInTheDocument());
    expect(screen.getByText('mock-customer-id-123')).toBeInTheDocument();
  });

  it('renders Rotate button for personalization ID', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Rotate')).toBeInTheDocument());
  });

  it('renders GIF settings section labels', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Load GIFs from KLIPY automatically')).toBeInTheDocument()
    );
    expect(screen.getByText('Share GIF personalization with provider')).toBeInTheDocument();
    expect(screen.getByText('Personalization ID')).toBeInTheDocument();
    expect(screen.getByText('Content Safety')).toBeInTheDocument();
  });

  it('renders GIF disabled hints when settings are off', async () => {
    vi.mocked(
      await import('@/renderer/stores/ui/privacyStore').then((m) => m.usePrivacyStore)
    ).mockImplementation((s) =>
      s({
        settings: {
          messagesFriendsOnly: true,
          messagesServerMembers: true,
          dmPrivacyLevel: 2 as const,
          dmFriendsOfFriends: false,
          autoAcceptFriendCodes: false,
          searchableByUsername: false,
          searchableByEmail: false,
          searchableByPhone: false,
          allowEmbeddedContent: false,
          loadGifsAutomatically: false,
          sharePersonalizationWithGifProvider: false,
        },
        loaded: true,
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Click to load/)).toBeInTheDocument());
    expect(screen.getByText(/GIF browsing uses a temporary ID/)).toBeInTheDocument();
    expect(screen.getByText(/This temporary value changes/)).toBeInTheDocument();
  });

  // ── Mode change modal ──────────────────────────────────────────────────

  it('opens mode change modal when clicking Simple', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() => expect(screen.getByText('Change Revocation Mode')).toBeInTheDocument());
    expect(screen.getByText(/Switching to Simple Revocation/)).toBeInTheDocument();
  });

  it('shows simple revocation mode description', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [],
          past_sessions: [],
          revocation_mode: 'simple',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: [],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 0,
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText(/Authenticate once to freely manage sessions/)).toBeInTheDocument()
    );
  });

  // ── DM privacy level 3 (Everyone) ─────────────────────────────────────

  it('disables friends-of-friends toggle at DM level 3', async () => {
    vi.mocked(
      await import('@/renderer/stores/ui/privacyStore').then((m) => m.usePrivacyStore)
    ).mockImplementation((s) =>
      s({
        settings: {
          messagesFriendsOnly: true,
          messagesServerMembers: true,
          dmPrivacyLevel: 3 as const,
          dmFriendsOfFriends: false,
          autoAcceptFriendCodes: false,
          searchableByUsername: false,
          searchableByEmail: false,
          searchableByPhone: false,
          allowEmbeddedContent: false,
          loadGifsAutomatically: true,
          sharePersonalizationWithGifProvider: true,
        },
        loaded: true,
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Allow Friends-of-Friends')).toBeInTheDocument()
    );
    const row = screen.getByText('Allow Friends-of-Friends').closest('.settings-row')!;
    expect(row.classList.contains('settings-row-disabled')).toBe(true);
    expect(screen.getByText(/Everyone can already DM you/)).toBeInTheDocument();
  });

  // ── DM privacy level 1 (Friends Only) ─────────────────────────────────

  it('renders DM description for level 1 (Friends Only)', async () => {
    vi.mocked(
      await import('@/renderer/stores/ui/privacyStore').then((m) => m.usePrivacyStore)
    ).mockImplementation((s) =>
      s({
        settings: {
          messagesFriendsOnly: true,
          messagesServerMembers: true,
          dmPrivacyLevel: 1 as const,
          dmFriendsOfFriends: false,
          autoAcceptFriendCodes: false,
          searchableByUsername: false,
          searchableByEmail: false,
          searchableByPhone: false,
          allowEmbeddedContent: false,
          loadGifsAutomatically: true,
          sharePersonalizationWithGifProvider: true,
        },
        loaded: true,
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Inner circle only/)).toBeInTheDocument());
  });

  // ── Cancel revoke confirmation ─────────────────────────────────────────

  it('cancels revoke confirmation for current session', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('This Device')).toBeInTheDocument());
    // Click Revoke to trigger confirmation
    fireEvent.click(screen.getAllByText('Revoke')[0]);
    await vi.waitFor(() =>
      expect(screen.getByText(/This is your current active session/)).toBeInTheDocument()
    );
    // Click Cancel
    fireEvent.click(screen.getByText('Cancel'));
    await vi.waitFor(() =>
      expect(screen.queryByText(/This is your current active session/)).not.toBeInTheDocument()
    );
  });

  // ── Safari user agent parsing ──────────────────────────────────────────

  it('parses Safari user agent', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Test',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Safari/600',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Safari Browser')).toBeInTheDocument());
  });

  // ── Unknown user agent parsing ─────────────────────────────────────────

  it('parses unknown user agent', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Test',
              ip_address: '1.2.3.4',
              user_agent: 'Some Unknown Agent',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Unknown Device')).toBeInTheDocument());
  });

  // ── Backup code warning styling ────────────────────────────────────────

  it('shows warning style when backup codes remaining is low', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 1,
          backup_email: '',
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => {
      const countEl = screen.getByText('1');
      expect(countEl.classList.contains('mfa-status-warn')).toBe(true);
    });
  });

  // ── Friends-of-Friends toggle interaction at level 2 ───────────────────

  it('allows toggling friends-of-friends at DM level 2', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Allow Friends-of-Friends')).toBeInTheDocument()
    );
    fireEvent.click(
      screen
        .getByText('Allow Friends-of-Friends')
        .closest('.settings-row')!
        .querySelector('input[type="checkbox"]')!
    );
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ dmFriendsOfFriends: true });
  });

  // ── DM level click interaction ─────────────────────────────────────────

  it('changes DM level when clicking a tier label', async () => {
    render(<PrivacySecuritySection />);
    // #1241 added a second tier control to this section, so both "No One"
    // buttons exist. Scope to the DM group by its accessible name rather than
    // by DOM order, which would silently follow a reorder.
    const dmGroup = await screen.findByRole('group', { name: /who can dm you/i });
    fireEvent.click(within(dmGroup).getByRole('button', { name: 'No One' }));
    // The local level changes — verify the description updates
    await vi.waitFor(() => expect(screen.getByText(/Hermit mode/)).toBeInTheDocument());
  });

  // ── Relative time formatting ───────────────────────────────────────────

  it('formats session times', async () => {
    const oneHourAgo = new Date(Date.now() - 3600000).toISOString();
    const threeDaysAgo = new Date(Date.now() - 3 * 86400000).toISOString();
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: threeDaysAgo,
              last_used: oneHourAgo,
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Active 1h ago')).toBeInTheDocument());
    expect(screen.getByText('Created 3d ago')).toBeInTheDocument();
  });

  // ── Revoke All with password flow ──────────────────────────────────────

  it('submits Revoke All with password and logs out', async () => {
    const mockLogout = vi.fn().mockResolvedValue(undefined);
    vi.mocked(
      await import('@/renderer/stores/auth/userStore').then((m) => m.useUserStore)
    ).mockImplementation((s) => s({ logout: mockLogout }));
    serveSection({ sessions: [sessionRow('s1', true)] });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke All Sessions' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), FIXTURE_PW);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Yes, Revoke All Sessions' }));

    await vi.waitFor(() => expect(mockLogout).toHaveBeenCalled());
    expect(bodiesTo('/api/v1/sessions/revoke-all')).toEqual([
      { include_current: true, password: FIXTURE_PW },
    ]);
  });

  // ── Revoke All modal 403 error ─────────────────────────────────────────

  it('shows a refused password on its field and empties it (revoke all)', async () => {
    serveSection({
      sessions: [sessionRow('s1', true)],
      onWrite: () => reply(403, { error: 'Incorrect password' }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke All Sessions' }));
    const dialog = await screen.findByRole('dialog');
    const field = await within(dialog).findByLabelText('Password');
    await userEvent.type(field, 'wrong-pw');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Yes, Revoke All Sessions' }));

    expect(await within(dialog).findByText('That password is not correct.')).toBeInTheDocument();
    expect(field).toHaveValue('');
  });

  // ── Mode change submission ─────────────────────────────────────────────

  it('submits mode change with password', async () => {
    serveSection({ onWrite: () => reply(200, { revocation_mode: 'simple' }) });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Simple'));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), FIXTURE_PW);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));

    await vi.waitFor(() =>
      expect(screen.getByText('Simple').closest('.revocation-mode-btn')).toHaveClass('active')
    );
    expect(bodiesTo('/api/v1/sessions/revocation-mode')).toEqual([
      { mode: 'simple', password: FIXTURE_PW },
    ]);
    expect(screen.queryByText('Change Revocation Mode')).not.toBeInTheDocument();
  });

  // ── Mode change 403 error ─────────────────────────────────────────────

  it('shows the server text of a 403 the adapter does not own (mode change)', async () => {
    serveSection({ onWrite: () => reply(403, { error: 'Authentication failed' }) });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Simple'));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), 'wrong-pw');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));

    expect(await within(dialog).findByText('Authentication failed')).toBeInTheDocument();
    // Not a field error: the adapter reads exact strings only (C5).
    expect(within(dialog).queryByText('That password is not correct.')).not.toBeInTheDocument();
  });

  // ── Session password modal submission ──────────────────────────────────

  // Mutant: `applyRevoked` not filtering the row out, or not refetching.
  it('submits session password and revokes', async () => {
    const deletes: RequestInit[] = [];
    serveSection({
      sessions: [sessionRow('s2', false)],
      onWrite: (_path, init) => {
        deletes.push(init);
        // The first, credential-less attempt is refused; the second carries the password.
        return deletes.length === 1 ? reply(403, { error: 'password_required' }) : reply(200);
      },
      // The refetch fails, so the row can only have left through the local filter.
      // (A held refetch would not do: the list shows a spinner while it loads.)
      relist: () => reply(500, { error: 'Failed to fetch sessions' }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Revoke'));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText('Verify Your Identity')).toBeInTheDocument();
    await userEvent.type(await within(dialog).findByLabelText('Password'), FIXTURE_PW);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm & Revoke' }));

    await vi.waitFor(() => expect(deletes).toHaveLength(2));
    // Single revoke sends no credential first (D12).
    expect(deletes[0].body).toBeUndefined();
    expect(JSON.parse(deletes[1].body as string)).toEqual({ password: FIXTURE_PW });
    await vi.waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await screen.findByText('Failed to fetch sessions');
    expect(sessionListReads()).toBe(2);
    expect(screen.queryByText('Chrome Browser')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Revoke' })).not.toBeInTheDocument();
  });

  // Mutant: the step-up path's `applyRevoked` handed another id, so a revoked
  // current session leaves this client signed in to a dead session.
  it('revoking the current session through the step-up dialog logs out', async () => {
    const mockLogout = vi.fn().mockResolvedValue(undefined);
    vi.mocked(
      await import('@/renderer/stores/auth/userStore').then((m) => m.useUserStore)
    ).mockImplementation((s) => s({ logout: mockLogout }));
    serveSection({
      sessions: [sessionRow('s1', true)],
      onWrite: (_path, init) =>
        typeof init.body === 'string' ? reply(200) : reply(403, { error: 'password_required' }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));
    fireEvent.click(await screen.findByRole('button', { name: 'Confirm' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), FIXTURE_PW);
    expect(mockLogout).not.toHaveBeenCalled();
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm & Revoke' }));

    await vi.waitFor(() => expect(mockLogout).toHaveBeenCalledTimes(1));
    expect(bodiesTo('/api/v1/sessions/s1')).toEqual([{ password: FIXTURE_PW }]);
  });

  // Two credential-free DELETEs in flight: the first refusal opens the dialog,
  // and the second must not re-point it at another session while the user is
  // typing into it (X dropped, Y revoked).
  // Mutant: `openStepUp` replacing an open dialog's action.
  it('a second Revoke refused while the first dialog is open does not re-point it', async () => {
    const held: Record<string, ReturnType<typeof deferred<Reply>>> = {};
    serveSection({
      // An older s3, so the list order (s2 first) does not hang on a clock tick.
      sessions: [sessionRow('s2', false), { ...sessionRow('s3', false), last_used: '2020-01-01' }],
      onWrite: (path, init) => {
        if (typeof init.body === 'string') return reply(200);
        held[path] = deferred<Reply>();
        return held[path].promise;
      },
    });
    render(<PrivacySecuritySection />);

    const [revokeS2, revokeS3] = await screen.findAllByRole('button', { name: 'Revoke' });
    fireEvent.click(revokeS2);
    fireEvent.click(revokeS3);
    await vi.waitFor(() => expect(Object.keys(held)).toHaveLength(2));
    await act(async () =>
      held['/api/v1/sessions/s2'].resolve(reply(403, { error: 'password_required' }))
    );
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), FIXTURE_PW);
    await act(async () =>
      held['/api/v1/sessions/s3'].resolve(reply(403, { error: 'password_required' }))
    );

    expect(screen.getAllByRole('dialog')).toHaveLength(1);
    // The typed password survives: the open dialog was not replaced.
    expect(within(dialog).getByLabelText('Password')).toHaveValue(FIXTURE_PW);
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm & Revoke' }));

    await vi.waitFor(() =>
      expect(bodiesTo('/api/v1/sessions/s2')).toEqual([{ password: FIXTURE_PW }])
    );
    expect(bodiesTo('/api/v1/sessions/s3')).toEqual([]);
  });

  // The credential-free DELETE is sent under a capture taken before it, and its
  // answer is dropped once the account or server has changed: it belongs to the
  // old one. Each answer runs twice, the unswitched run being the control that
  // shows the effect the switched run must not have.
  // Mutant: the currentness check after the answer deleted.
  describe.each([
    ['a step-up refusal', () => reply(403, { error: 'password_required' })],
    ['an acceptance', () => reply(200)],
    ['a failure', () => reply(500, { error: 'Internal error' })],
  ])('single revoke: %s', (_name, answer) => {
    async function revokeHeld(switched: boolean): Promise<void> {
      const held = deferred<Reply>();
      serveSection({
        sessions: [sessionRow('s2', false)],
        onWrite: () => held.promise,
        relist: () => reply(200, { sessions: [], past_sessions: [] }),
      });
      render(<PrivacySecuritySection />);
      fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));
      await vi.waitFor(() => expect(bodiesTo('/api/v1/sessions/s2')).toHaveLength(0));
      const sent = mockApiFetch.mock.calls.find((c) => c[0] === '/api/v1/sessions/s2');
      expect(sent?.[2]).toEqual({
        context: expect.objectContaining({
          authLifecycle: expect.objectContaining({ authGeneration: 0 }),
        }),
      });
      if (switched) await switchAccount();
      await act(async () => held.resolve(answer()));
    }

    /** Something the answer did: a dialog, a removed row, or a banner. */
    const anyEffect = () =>
      screen.queryByRole('dialog') !== null ||
      screen.queryByText('Chrome Browser') === null ||
      screen.queryByText('Internal error') !== null;

    it('acts when the account is unchanged', async () => {
      await revokeHeld(false);
      await vi.waitFor(() => expect(anyEffect()).toBe(true));
    });

    it('is dropped when the account changed', async () => {
      await revokeHeld(true);
      await act(async () => {
        await Promise.resolve();
      });
      expect(anyEffect()).toBe(false);
      expect(sessionListReads()).toBe(1);
      expect(screen.getByRole('button', { name: 'Revoke' })).not.toBeDisabled();
    });
  });

  // A proxy's HTML error page is not JSON; parsing it must not put a parse error
  // in the banner.
  // Mutant: `response.json()` without its `.catch`.
  it('a single revoke answered with a non-JSON body shows the generic sentence', async () => {
    serveSection({
      sessions: [sessionRow('s2', false)],
      onWrite: () => ({
        ok: false,
        status: 502,
        json: async () => {
          throw new SyntaxError('Unexpected token < in JSON at position 0');
        },
      }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));

    expect(await screen.findByText('Failed to revoke session')).toBeInTheDocument();
    expect(screen.queryByText(/Unexpected token/)).not.toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  // ── Revoke All empty password validation ──────────────────────────────

  it('keeps the primary aria-disabled until a password is typed, and sends nothing before', async () => {
    serveSection({ sessions: [sessionRow('s1', true)] });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke All Sessions' }));
    const dialog = await screen.findByRole('dialog');
    await within(dialog).findByLabelText('Password');
    const confirm = within(dialog).getByRole('button', { name: 'Yes, Revoke All Sessions' });

    // Not natively disabled (it stays reachable), but inert: a click names what is missing.
    expect(confirm).toHaveAttribute('aria-disabled', 'true');
    await userEvent.click(confirm);
    expect(await within(dialog).findByText('Enter your password to continue.')).toBeInTheDocument();
    expect(bodiesTo('/api/v1/sessions/revoke-all')).toEqual([]);
  });

  // ── Session revoke 403 with incorrect password ─────────────────────────

  it('shows a refused password on its field in the single-revoke dialog', async () => {
    let n = 0;
    serveSection({
      sessions: [sessionRow('s2', false)],
      onWrite: () =>
        ++n === 1
          ? reply(403, { error: 'password_required' })
          : reply(403, { error: 'Incorrect password' }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Revoke'));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), 'wrong');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm & Revoke' }));

    expect(await within(dialog).findByText('That password is not correct.')).toBeInTheDocument();
  });

  // ── DM slider interaction ──────────────────────────────────────────────

  it('changes DM level via slider', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());
    const slider = screen.getByRole('slider', { name: /who can dm you/i });
    fireEvent.change(slider, { target: { value: '0' } });
    await vi.waitFor(() => expect(screen.getByText(/Hermit mode/)).toBeInTheDocument());
  });

  // ── Embedded content description ───────────────────────────────────────

  it('renders embedded content description', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText(/Render link previews, image thumbnails/)).toBeInTheDocument()
    );
  });

  // ── Revoke All non-ok non-403 error ────────────────────────────────────

  it('handles non-403 error on revoke all', async () => {
    serveSection({
      sessions: [sessionRow('s1', true)],
      onWrite: () => reply(500, { error: 'Internal error' }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke All Sessions' }));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), 'pw123');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Yes, Revoke All Sessions' }));

    expect(await within(dialog).findByText('Internal error')).toBeInTheDocument();
  });

  // ── Mode change non-ok non-403 error ───────────────────────────────────

  it('handles non-403 error on mode change', async () => {
    serveSection({ onWrite: () => reply(500, { error: 'Server error' }) });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Simple'));
    const dialog = await screen.findByRole('dialog');
    await userEvent.type(await within(dialog).findByLabelText('Password'), 'pw');
    await userEvent.click(within(dialog).getByRole('button', { name: 'Confirm' }));

    expect(await within(dialog).findByText('Server error')).toBeInTheDocument();
  });

  // ── Close Revoke All modal ─────────────────────────────────────────────

  it('closes Revoke All modal when cancel clicked', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Revoke All Sessions')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Revoke All Sessions'));
    await vi.waitFor(() => expect(screen.getByText('No, Cancel')).toBeInTheDocument());
    fireEvent.click(screen.getByText('No, Cancel'));
    await vi.waitFor(() =>
      expect(screen.queryByText(/revoke all of your active session tokens/)).not.toBeInTheDocument()
    );
  });

  // ── Close session password modal ───────────────────────────────────────

  it('closes session password modal when cancel clicked', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());

    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'password_required' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Verify Your Identity')).toBeInTheDocument());

    // Find and click Cancel in the modal
    const dialogs = screen.getAllByRole('dialog');
    const sessionDialog = dialogs.find((d) => d.textContent?.includes('Verify Your Identity'))!;
    const cancelBtn = sessionDialog.querySelector(
      '.revoke-all-modal-cancel-btn'
    ) as HTMLButtonElement;
    fireEvent.click(cancelBtn);
    await vi.waitFor(() =>
      expect(screen.queryByText('Verify Your Identity')).not.toBeInTheDocument()
    );
  });

  // ── Close mode change modal ────────────────────────────────────────────

  it('closes mode change modal when cancel clicked', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());

    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() => expect(screen.getByText('Change Revocation Mode')).toBeInTheDocument());

    const dialogs = screen.getAllByRole('dialog');
    const modeDialog = dialogs.find((d) => d.textContent?.includes('Change Revocation Mode'))!;
    const cancelBtn = modeDialog.querySelector('.revoke-all-modal-cancel-btn') as HTMLButtonElement;
    fireEvent.click(cancelBtn);
    await vi.waitFor(() =>
      expect(screen.queryByText('Change Revocation Mode')).not.toBeInTheDocument()
    );
  });

  // ── Enter key in password fields ───────────────────────────────────────

  // ── Mode change Enter key ─────────────────────────────────────────────

  // ── formatRelativeTime edge cases ──────────────────────────────────────

  it('formats Just now for very recent times', async () => {
    const justNow = new Date().toISOString();
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: justNow,
              last_used: justNow,
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Active Just now')).toBeInTheDocument());
  });

  it('formats minutes for recent times', async () => {
    const fiveMinAgo = new Date(Date.now() - 5 * 60000).toISOString();
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: fiveMinAgo,
              last_used: fiveMinAgo,
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Active 5m ago')).toBeInTheDocument());
  });

  // ── Revoke current session (logout flow) ──────────────────────────────

  it('revokes current session, confirms, and logs out', async () => {
    const mockLogout = vi.fn().mockResolvedValue(undefined);
    vi.mocked(
      await import('@/renderer/stores/auth/userStore').then((m) => m.useUserStore)
    ).mockImplementation((s) => s({ logout: mockLogout }));
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Desktop',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: true,
            },
          ],
          past_sessions: [],
          revocation_mode: 'simple',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('This Device')).toBeInTheDocument());

    // Click Revoke on current session — triggers confirmation
    fireEvent.click(screen.getAllByText('Revoke')[0]);
    await vi.waitFor(() =>
      expect(screen.getByText(/This is your current active session/)).toBeInTheDocument()
    );

    // Mock successful DELETE
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });

    // Click Confirm to actually revoke
    fireEvent.click(screen.getByText('Confirm'));
    await vi.waitFor(() => expect(mockLogout).toHaveBeenCalled());
  });

  // ── Empty user agent ──────────────────────────────────────────────────

  it('handles empty user agent string', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Test',
              ip_address: '1.2.3.4',
              user_agent: '',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-01-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Unknown Device')).toBeInTheDocument());
  });

  // ── Session revoke with auth_required ──────────────────────────────────

  it('shows password modal on 403 auth_required', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());

    // Trigger 403 with auth_required
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'auth_required' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Verify Your Identity')).toBeInTheDocument());
  });

  // ── Session revoke non-403 error ───────────────────────────────────────

  it('shows error on non-403 session revoke failure', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());

    // Mock 500 error
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => ({ error: 'Server exploded' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Server exploded')).toBeInTheDocument());
  });

  // ── Permission Request button click (notifications) ────────────────────

  it('calls requestOne when notification Request button clicked', async () => {
    const mockRequestOne = vi.fn().mockResolvedValue('granted');
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'not-determined',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: mockRequestOne,
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Request')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Request'));
    await vi.waitFor(() => expect(mockRequestOne).toHaveBeenCalledWith('notifications'));
  });

  // ── Permission "Open System Settings" click (secure storage) ───────────

  it('calls openSettings when Open System Settings clicked for unavailable secure storage', async () => {
    const mockOpenSettings = vi.fn().mockResolvedValue(undefined);
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'unavailable',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: mockOpenSettings,
      })
    );
    render(<PrivacySecuritySection />);
    // "Fix" was unified into a universal "Open System Settings" action (#1743).
    // Scope to the Secure Storage row via its unique required-warning copy.
    await vi.waitFor(() =>
      expect(screen.getByText(/Secure storage is required for login/)).toBeInTheDocument()
    );
    const secureRow = screen
      .getByText(/Secure storage is required for login/)
      .closest('.settings-row')!;
    const secureBtn = Array.from(secureRow.querySelectorAll('button')).find(
      (b) => b.textContent === 'Open System Settings'
    )!;
    fireEvent.click(secureBtn);
    expect(mockOpenSettings).toHaveBeenCalledWith('secureStorage');
  });

  // ── Rotate personalization ID ───────────────────────────────────────────

  it('rotates personalization ID when Rotate clicked', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Rotate')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Rotate'));
    await vi.waitFor(() => expect(screen.getByText('mock-rotated-id-456')).toBeInTheDocument());
  });

  // ── DM privacy slider debounce ─────────────────────────────────────────

  it('debounces DM privacy level API call', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());
    // Click the DM group's "No One" (#1241 added a second one to this section)
    const dmGroup = screen.getByRole('group', { name: /who can dm you/i });
    fireEvent.click(within(dmGroup).getByRole('button', { name: 'No One' }));
    // The API call should not happen immediately
    expect(mockUpdatePrivacy).not.toHaveBeenCalledWith({ dmPrivacyLevel: 0 });
    // Advance timers past the 300ms debounce
    vi.advanceTimersByTime(350);
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ dmPrivacyLevel: 0 });
    vi.useRealTimers();
  });

  // ── Session revoke general 403 (unknown error code) ────────────────────

  it('throws error for unrecognized 403 on session revoke', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's2',
              device_name: 'Phone',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: '2026-02-01T00:00:00Z',
              last_used: new Date().toISOString(),
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());

    // Trigger 403 with unrecognized error code
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'unknown_403_code' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('unknown_403_code')).toBeInTheDocument());
  });

  // ── MFA fetch failure is non-critical ──────────────────────────────────

  it('handles MFA status fetch failure gracefully', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
      })
      .mockRejectedValueOnce(new Error('Network error'))
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    // Should still render without errors
    await vi.waitFor(() =>
      expect(screen.getByText('Multi-Factor Authentication')).toBeInTheDocument()
    );
  });

  // ── Secure Storage unavailable shows a navigable action ───────────────

  it('shows Open System Settings for unavailable secure storage', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'unavailable',
        notifications: 'granted',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Unavailable')).toBeInTheDocument());
    // Secure storage shows the required-warning plus a navigable action (#1743).
    expect(screen.getByText(/Secure storage is required for login/)).toBeInTheDocument();
    const secureRow = screen
      .getByText(/Secure storage is required for login/)
      .closest('.settings-row')!;
    const secureBtn = Array.from(secureRow.querySelectorAll('button')).find(
      (b) => b.textContent === 'Open System Settings'
    );
    expect(secureBtn).toBeTruthy();
  });

  // ── Notification not-determined shows Request ──────────────────────────

  it('shows Request button for not-determined notifications', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'not-determined',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Not Requested')).toBeInTheDocument());
    expect(screen.getByText('Request')).toBeInTheDocument();
  });

  // ── Backup code reset flow ─────────────────────────────────────────────

  it('opens backup code reset modal and renders form', async () => {
    serveSection({ mfaMethods: ['totp'] });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Reset/)).not.toBeDisabled());
    fireEvent.click(screen.getByText(/Reset/));
    await vi.waitFor(() => expect(screen.getByText('Reset Backup Codes')).toBeInTheDocument());
    expect(screen.getByText(/This will invalidate all existing backup codes/)).toBeInTheDocument();
    expect(screen.getByLabelText('Password')).toBeInTheDocument();
  });

  // RegenerateBackupCodes checks an authenticator-app code and nothing else, so
  // the stage must not offer a backup code or a security key, and it has no
  // purpose to read for (the floor is TOTP).
  it('offers only an authenticator code when resetting backup codes', async () => {
    // Were the stage to read, this account would be offered all three.
    readOffers(['webauthn', 'totp'], true);
    serveSection({ mfaMethods: ['totp'] });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Reset/)).not.toBeDisabled());
    fireEvent.click(screen.getByText(/Reset/));
    await vi.waitFor(() => expect(screen.getByText('Reset Backup Codes')).toBeInTheDocument());

    expect(screen.getByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(screen.queryByLabelText('Backup code')).not.toBeInTheDocument();
    expect(
      screen.queryByRole('group', { name: 'Passkey or security key' })
    ).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /instead/i })).not.toBeInTheDocument();
    expect(mockStepUpRead).not.toHaveBeenCalled();
  });

  // ── Backup code reset submission ────────────────────────────────────────

  it('submits backup code reset and shows new codes', async () => {
    serveSection({
      mfaMethods: ['totp'],
      onWrite: () => reply(200, { backup_codes: ['CODE1', 'CODE2', 'CODE3'] }),
    });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Reset/)).not.toBeDisabled());
    fireEvent.click(screen.getByText(/Reset/));
    await vi.waitFor(() => expect(screen.getByText('Reset Backup Codes')).toBeInTheDocument());

    expect(screen.getByRole('button', { name: 'Regenerate Codes' })).toHaveAttribute(
      'aria-disabled',
      'true'
    );
    await userEvent.type(screen.getByLabelText('Password'), FIXTURE_PW);
    await userEvent.type(screen.getByLabelText('Authenticator app code'), '123456');
    expect(screen.getByRole('button', { name: 'Regenerate Codes' })).not.toHaveAttribute(
      'aria-disabled'
    );
    await userEvent.click(screen.getByRole('button', { name: 'Regenerate Codes' }));

    await vi.waitFor(() => expect(screen.getByTestId('backup-code-display')).toBeInTheDocument());
    // The status is re-read after the new codes are issued.
    expect(
      mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/status').length
    ).toBeGreaterThanOrEqual(2);
  });

  // Q7: TOTP was turned off after the modal opened, so the status the page shows is
  // stale. The stage says so, and the page re-reads `/mfa/status`.
  it('says the authenticator app was turned off and re-reads the MFA status', async () => {
    serveSection({
      mfaMethods: ['totp'],
      onWrite: () => reply(400, { error: 'TOTP is not enabled' }),
    });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Reset/)).not.toBeDisabled());
    const statusReads = () =>
      mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/status').length;
    await vi.waitFor(() => expect(statusReads()).toBe(1));
    fireEvent.click(screen.getByText(/Reset/));
    await userEvent.type(await screen.findByLabelText('Password'), FIXTURE_PW);
    await userEvent.type(screen.getByLabelText('Authenticator app code'), '123456');
    await userEvent.click(screen.getByRole('button', { name: 'Regenerate Codes' }));

    expect(
      await screen.findByText(
        'Your authenticator app was turned off. Close this and check your security settings.'
      )
    ).toBeInTheDocument();
    expect(screen.queryByLabelText('Password')).not.toBeInTheDocument();
    await vi.waitFor(() => expect(statusReads()).toBe(2));
    expect(screen.queryByTestId('backup-code-display')).not.toBeInTheDocument();
  });

  it('handles backup reset error', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 5,
          backup_email: '',
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => {
      const resetBtn = screen.getByText(/Reset/);
      expect(resetBtn).not.toBeDisabled();
    });
    fireEvent.click(screen.getByText(/Reset/));
    await vi.waitFor(() => expect(screen.getByText('Reset Backup Codes')).toBeInTheDocument());
    // Cancel button should be present
    expect(screen.getByText('Cancel')).toBeInTheDocument();
    // Click cancel to close
    fireEvent.click(screen.getByText('Cancel'));
    await vi.waitFor(() =>
      expect(screen.queryByText('Reset Backup Codes')).not.toBeInTheDocument()
    );
  });

  // ── Session sorting (both non-current) ─────────────────────────────────

  it('sorts sessions by last_used when neither is current', async () => {
    const older = new Date(Date.now() - 7200000).toISOString();
    const newer = new Date(Date.now() - 3600000).toISOString();
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [
            {
              id: 's1',
              device_name: 'Older',
              ip_address: '1.2.3.4',
              user_agent: 'Mozilla/5.0 Electron',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: older,
              last_used: older,
              is_current: false,
            },
            {
              id: 's2',
              device_name: 'Newer',
              ip_address: '5.6.7.8',
              user_agent: 'Mozilla/5.0 Chrome/100',
              expires_at: '2026-12-01T00:00:00Z',
              created_at: newer,
              last_used: newer,
              is_current: false,
            },
          ],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ methods: [], backup_codes_remaining: 0 }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());
    // Both sessions should render; the Chrome (newer) should be first
    const cards = document.querySelectorAll('.session-card');
    expect(cards.length).toBe(2);
  });

  // ── PermissionRow requesting state ─────────────────────────────────────

  it('shows Requesting state when requesting notification permission', async () => {
    let resolveRequest: (value: string) => void;
    const requestPromise = new Promise<string>((res) => {
      resolveRequest = res;
    });
    const mockRequestOne = vi.fn().mockReturnValue(requestPromise);
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'not-determined',
        isLoaded: true,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: mockRequestOne,
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Request')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Request'));
    await vi.waitFor(() => expect(screen.getByText('Requesting...')).toBeInTheDocument());
    // Resolve the promise to clean up
    resolveRequest!('granted');
    await vi.waitFor(() => expect(screen.getByText('Request')).toBeInTheDocument());
  });

  // ── handleRevokeAll validation (no password) ───────────────────────────

  // ── MFA-enabled modals use MFA verify prompt ───────────────────────────

  it('revoke all asks an authenticator account for its code, not a password', async () => {
    readOffers(['totp']);
    serveSection({ sessions: [sessionRow('s1', true)], mfaMethods: ['totp'] });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByRole('button', { name: 'Revoke All Sessions' }));
    const dialog = await screen.findByRole('dialog');

    expect(await within(dialog).findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(within(dialog).queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('mode change asks an authenticator account for its code, not a password', async () => {
    readOffers(['totp']);
    serveSection({ mfaMethods: ['totp'] });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Simple'));
    const dialog = await screen.findByRole('dialog');

    expect(await within(dialog).findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(within(dialog).queryByLabelText('Password')).not.toBeInTheDocument();
  });

  it('single revoke on auth_required lets the read decide: an authenticator account gets its code field', async () => {
    readOffers(['totp']);
    serveSection({
      sessions: [sessionRow('s2', false)],
      mfaMethods: ['totp'],
      onWrite: () => reply(403, { error: 'auth_required', methods: ['email'] }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Revoke'));
    const dialog = await screen.findByRole('dialog');

    // `methods` on this refusal lists users.mfa_methods; it is never read (C2).
    expect(await within(dialog).findByLabelText('Authenticator app code')).toBeInTheDocument();
    expect(within(dialog).queryByLabelText('Password')).not.toBeInTheDocument();
  });

  // C2: the dialogs never read `mfaMethods`. An account whose only method is email
  // (`mfaMethods: ['email']`) can use no inline factor, so the read says [] and the
  // password field is what it gets; a dialog that derived the leg from `mfaMethods`
  // would show a code box this account can never fill.
  it.each([
    ['revoke all', () => screen.findByRole('button', { name: 'Revoke All Sessions' })],
    ['mode change', () => screen.findByText('Simple')],
  ])('%s gives an email-only account the password field (C2)', async (_name, open) => {
    readOffers([]);
    serveSection({ sessions: [sessionRow('s1', true)], mfaMethods: ['email'] });
    render(<PrivacySecuritySection />);

    fireEvent.click(await open());
    const dialog = await screen.findByRole('dialog');

    expect(await within(dialog).findByLabelText('Password')).toBeInTheDocument();
    expect(within(dialog).queryByLabelText('Authenticator app code')).not.toBeInTheDocument();
  });

  it('single revoke on password_required keeps the password field when the read fails', async () => {
    // A failed read leaves the seed standing: the server found no inline method.
    mockStepUpRead.mockReset().mockImplementation(async () => reply(503));
    serveSection({
      sessions: [sessionRow('s2', false)],
      onWrite: () => reply(403, { error: 'password_required' }),
    });
    render(<PrivacySecuritySection />);

    fireEvent.click(await screen.findByText('Revoke'));
    const dialog = await screen.findByRole('dialog');

    expect(await within(dialog).findByLabelText('Password')).toBeInTheDocument();
  });

  // ── Permissions loading state ──────────────────────────────────────────

  it('shows loading state when permissions not loaded', async () => {
    vi.mocked(
      await import('@/renderer/stores/voice/osPermissionStore').then((m) => m.useOsPermissionStore)
    ).mockImplementation((s) =>
      s({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
        isLoaded: false,
        fetchAll: vi.fn().mockResolvedValue(undefined),
        requestOne: vi.fn().mockResolvedValue('granted'),
        openSettings: vi.fn().mockResolvedValue(undefined),
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Loading permission statuses...')).toBeInTheDocument()
    );
  });

  // ─── SSO Security toggles (issue #270) ─────────────────────────────
  it('renders SSO Security controls as provider-generic switches', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('SSO Security')).toBeInTheDocument());

    expect(screen.getByText('Trust SSO provider verification')).toBeInTheDocument();
    expect(screen.getByText('Require SSO for sign-in')).toBeInTheDocument();
    expect(
      screen.getByText(/Only enable this if your SSO provider enforces MFA/i)
    ).toBeInTheDocument();
    expect(screen.queryByText(/Google account/i)).not.toBeInTheDocument();

    expect(
      screen.getByRole('switch', { name: /Trust SSO provider verification/i })
    ).toBeInTheDocument();
    expect(screen.getByRole('switch', { name: /Require SSO for sign-in/i })).toBeInTheDocument();
  });

  it('Trust SSO toggle reveals passphrase confirm and PATCHes on submit', async () => {
    // After the 4 default mocks (sessions, mfa/status, webauthn, sso-identities),
    // the 5th call is the PATCH /users/me/security from the toggle confirm.
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ ok: true }),
      text: async () => JSON.stringify({ ok: true }),
    });

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('SSO Security')).toBeInTheDocument());

    // Click the trust-SSO switch
    const trustToggle = screen.getByRole('switch', {
      name: /Trust SSO provider verification/i,
    }) as HTMLInputElement;
    fireEvent.click(trustToggle);

    // Inline confirm UI appears
    const passInput = await screen.findByLabelText(/enter your passphrase to confirm/i);
    fireEvent.change(passInput, { target: { value: 'CorrectPW!' } }); // pragma: allowlist secret

    fireEvent.click(screen.getByRole('button', { name: /^confirm$/i }));

    await vi.waitFor(() => {
      // 4 calls on mockApiFetch: sessions, mfa/status, webauthn/credentials, PATCH /security.
      // (The sso-identities GET goes through a separate mock — see top of file.)
      expect(mockApiFetch).toHaveBeenCalledTimes(4);
    });

    const patchCall = mockApiFetch.mock.calls.find((c) => c[0] === '/api/v1/users/me/security');
    expect(patchCall).toBeDefined();
    const init = patchCall![1] as RequestInit;
    expect(init.method).toBe('PATCH');
    const body = JSON.parse(init.body as string);
    expect(body.trust_sso_security).toBe(true);
    expect(body.current_passphrase).toBe('CorrectPW!'); // pragma: allowlist secret
  });

  it('Disable-password-login toggle PATCHes password_login_disabled with passphrase', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ ok: true }),
      text: async () => JSON.stringify({ ok: true }),
    });

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('SSO Security')).toBeInTheDocument());

    const pwToggle = screen.getByRole('switch', {
      name: /Require SSO for sign-in/i,
    }) as HTMLInputElement;
    fireEvent.click(pwToggle);

    const passInput = await screen.findByLabelText(/enter your passphrase to confirm/i);
    fireEvent.change(passInput, { target: { value: 'AnotherPW!' } }); // pragma: allowlist secret
    fireEvent.click(screen.getByRole('button', { name: /^confirm$/i }));

    await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(4));

    const patchCall = mockApiFetch.mock.calls.find((c) => c[0] === '/api/v1/users/me/security');
    expect(patchCall).toBeDefined();
    const body = JSON.parse((patchCall![1] as RequestInit).body as string);
    expect(body.password_login_disabled).toBe(true);
    expect(body.current_passphrase).toBe('AnotherPW!'); // pragma: allowlist secret
  });

  it('hydrates SSO toggles from GET /users/me/security on mount', async () => {
    // Override the default off/off fixture to return on/on so the toggles
    // should reflect the server state instead of defaulting to false.
    mockSecurityGetFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      json: async () => ({ password_login_disabled: true, trust_sso_security: true }),
      text: async () => JSON.stringify({ password_login_disabled: true, trust_sso_security: true }),
    });

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('SSO Security')).toBeInTheDocument());

    const trustToggle = screen.getByRole('switch', {
      name: /Trust SSO provider verification/i,
    }) as HTMLInputElement;
    const pwToggle = screen.getByRole('switch', {
      name: /Require SSO for sign-in/i,
    }) as HTMLInputElement;

    await vi.waitFor(() => expect(trustToggle.checked).toBe(true));
    expect(pwToggle.checked).toBe(true);
    expect(mockSecurityGetFetch).toHaveBeenCalled();
  });
});

// ── #1241: a superseded tier PATCH must not act on its own outcome ───────────
//
// CodeRabbit on PR #2888. The 300 ms debounce coalesces rapid clicks, but two
// selections further apart put two PATCHes in flight, and a slow rejection of
// the FIRST would otherwise revert the user's newer choice and surface an error
// for a request they had already replaced.
describe('PrivacySecuritySection — superseded tier mutations (#1241)', () => {
  // Earlier tests in this file install permanent mockImplementation overrides on
  // usePrivacyStore (see ~line 885 onward), so a block at the end of the file
  // inherits whichever one ran last — none of which carry `loaded`. Install a
  // complete store shape here rather than depending on file ordering. Without
  // `loaded: true` the friend-request control renders disabled, its clicks are
  // no-ops, and the cross-control test below passes for the wrong reason.
  beforeEach(() => {
    drainOnceQueues();
    vi.mocked(usePrivacyStore).mockImplementation((s: (st: unknown) => unknown) =>
      s({
        settings: {
          messagesFriendsOnly: true,
          messagesServerMembers: true,
          dmPrivacyLevel: 2 as const,
          dmFriendsOfFriends: false,
          autoAcceptFriendCodes: false,
          searchableByUsername: false,
          searchableByEmail: false,
          searchableByPhone: false,
          allowEmbeddedContent: false,
          loadGifsAutomatically: true,
          sharePersonalizationWithGifProvider: true,
          allowFriendRequestsFrom: 'everyone' as const,
        },
        loaded: true,
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
  });

  // POSITIVE CONTROL. Without this, the negative test below cannot be trusted:
  // if the error can never render in this harness, "no error appeared" passes
  // whether or not the guard exists.
  it('surfaces the error when a tier PATCH rejects and is NOT superseded', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mockUpdatePrivacy.mockRejectedValueOnce(new Error('lonely failure'));

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());
    const dmGroup = screen.getByRole('group', { name: /who can dm you/i });

    fireEvent.click(within(dmGroup).getByRole('button', { name: 'No One' }));
    await act(async () => {
      vi.advanceTimersByTime(350);
    });

    expect(await screen.findByText('lonely failure')).toBeInTheDocument();
    vi.useRealTimers();
  });

  it('ignores a stale rejection once a newer tier selection has been issued', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let rejectFirst!: (e: Error) => void;
    mockUpdatePrivacy
      .mockReturnValueOnce(
        new Promise((_res, rej) => {
          rejectFirst = rej as (e: Error) => void;
        })
      )
      .mockResolvedValueOnce(undefined);

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());
    const dmGroup = screen.getByRole('group', { name: /who can dm you/i });

    fireEvent.click(within(dmGroup).getByRole('button', { name: 'No One' }));
    vi.advanceTimersByTime(350);
    fireEvent.click(within(dmGroup).getByRole('button', { name: 'Everyone' }));
    vi.advanceTimersByTime(350);

    // The FIRST request now fails, after the second has superseded it.
    await act(async () => {
      rejectFirst(new Error('stale failure'));
    });

    // No error surfaced for the abandoned request.
    expect(screen.queryByText('stale failure')).not.toBeInTheDocument();
    vi.useRealTimers();
  });
});

// ── Cross-control supersession must NOT occur (#1241) ────────────────────────
//
// Gitar and CodeRabbit both caught this independently on PR #2888. Both tier
// controls route through commitPrivacyTier, so a single shared generation
// counter let one supersede the other: selecting a friend-request mode while a
// DM PATCH was in flight marked the DM rejection "superseded", swallowing its
// error and skipping its revert.
//
// The same-control test above cannot detect this — it only ever supersedes a DM
// update with another DM update. That shared blind spot is why the bug survived
// falsification of that test.
describe('PrivacySecuritySection — cross-control supersession (#1241)', () => {
  // Earlier tests in this file install permanent mockImplementation overrides on
  // usePrivacyStore (see ~line 885 onward), so a block at the end of the file
  // inherits whichever one ran last — none of which carry `loaded`. Install a
  // complete store shape here rather than depending on file ordering. Without
  // `loaded: true` the friend-request control renders disabled, its clicks are
  // no-ops, and the cross-control test below passes for the wrong reason.
  beforeEach(() => {
    drainOnceQueues();
    vi.mocked(usePrivacyStore).mockImplementation((s: (st: unknown) => unknown) =>
      s({
        settings: {
          messagesFriendsOnly: true,
          messagesServerMembers: true,
          dmPrivacyLevel: 2 as const,
          dmFriendsOfFriends: false,
          autoAcceptFriendCodes: false,
          searchableByUsername: false,
          searchableByEmail: false,
          searchableByPhone: false,
          allowEmbeddedContent: false,
          loadGifsAutomatically: true,
          sharePersonalizationWithGifProvider: true,
          allowFriendRequestsFrom: 'everyone' as const,
        },
        loaded: true,
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
  });

  it('a friend-request selection does NOT suppress a failing DM save', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let rejectDm!: (e: Error) => void;
    mockUpdatePrivacy
      .mockReturnValueOnce(
        new Promise((_res, rej) => {
          rejectDm = rej as (e: Error) => void;
        })
      )
      .mockResolvedValueOnce(undefined);

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());

    // 1. Start a DM save (left in flight).
    const dmGroup = screen.getByRole('group', { name: /who can dm you/i });
    fireEvent.click(within(dmGroup).getByRole('button', { name: 'No One' }));
    vi.advanceTimersByTime(350);

    // 2. Change an UNRELATED control while the DM PATCH is still outstanding.
    const frGroup = screen.getByRole('group', { name: /who can send you friend requests/i });
    // Guard the harness: if this control is disabled the click is a no-op and the
    // test passes for the wrong reason — it must actually issue a second PATCH.
    expect(within(frGroup).getByRole('button', { name: 'No One' })).not.toBeDisabled();
    fireEvent.click(within(frGroup).getByRole('button', { name: 'No One' }));
    vi.advanceTimersByTime(350);

    // 3. The DM PATCH now fails. It was never superseded by a newer DM write,
    //    so its error MUST surface.
    await act(async () => {
      rejectDm(new Error('dm save rejected'));
    });

    expect(await screen.findByText('dm save rejected')).toBeInTheDocument();
    vi.useRealTimers();
  });
});

// ── #1241: the friend-request WRITE path ─────────────────────────────────────
//
// The controls are asserted extensively at the presentation layer, but nothing
// connected "user picks a tier" to "a PATCH goes out". Replacing the whole
// debounce callback body with `void mode;` left the suite green, so the entire
// write path was unevidenced.
const LOADED_PRIVACY_STORE = {
  settings: {
    messagesFriendsOnly: true,
    messagesServerMembers: true,
    dmPrivacyLevel: 2 as const,
    dmFriendsOfFriends: false,
    autoAcceptFriendCodes: false,
    searchableByUsername: false,
    searchableByEmail: false,
    searchableByPhone: false,
    allowEmbeddedContent: false,
    loadGifsAutomatically: true,
    sharePersonalizationWithGifProvider: true,
    allowFriendRequestsFrom: 'everyone' as const,
  },
  loaded: true,
};

/**
 * Earlier blocks in this file install permanent `mockImplementation` overrides
 * on `usePrivacyStore`, none of which carry `loaded`. Every block below
 * therefore reinstalls a complete shape rather than inheriting whichever one
 * ran last — without `loaded: true` the friend-request control renders
 * disabled and its clicks are no-ops, which would make these tests pass for
 * the wrong reason.
 */
function installPrivacyStoreMock(loaded: boolean) {
  vi.mocked(usePrivacyStore).mockImplementation((s: (st: unknown) => unknown) =>
    s({
      ...LOADED_PRIVACY_STORE,
      loaded,
      fetchPrivacy: mockFetchPrivacy,
      updatePrivacy: mockUpdatePrivacy,
    })
  );
}

function friendRequestGroup() {
  return screen.getByRole('group', { name: /who can send you friend requests/i });
}

describe('PrivacySecuritySection — friend-request write path (#1241)', () => {
  beforeEach(() => {
    drainOnceQueues();
    vi.clearAllMocks();
    mockUpdatePrivacy.mockReset();
    mockUpdatePrivacy.mockResolvedValue(undefined);
    installPrivacyStoreMock(true);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('issues exactly one PATCH carrying the picked friend-request mode', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Who Can Send You Friend Requests')).toBeInTheDocument()
    );

    const noOne = within(friendRequestGroup()).getByRole('button', { name: 'No One' });
    // Guard the harness: a disabled control makes the click a no-op.
    expect(noOne).not.toBeDisabled();
    fireEvent.click(noOne);

    // Debounced — nothing may go out before the window closes.
    expect(mockUpdatePrivacy).not.toHaveBeenCalled();

    vi.advanceTimersByTime(350);

    expect(mockUpdatePrivacy).toHaveBeenCalledTimes(1);
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ allowFriendRequestsFrom: 'nobody' });
  });

  it('coalesces rapid picks into ONE PATCH carrying the LAST mode', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Who Can Send You Friend Requests')).toBeInTheDocument()
    );

    // Re-query between clicks: each pick re-renders the control.
    fireEvent.click(within(friendRequestGroup()).getByRole('button', { name: 'No One' }));
    fireEvent.click(within(friendRequestGroup()).getByRole('button', { name: 'Everyone' }));
    fireEvent.click(within(friendRequestGroup()).getByRole('button', { name: 'Mutual Servers' }));

    vi.advanceTimersByTime(350);

    expect(mockUpdatePrivacy).toHaveBeenCalledTimes(1);
    expect(mockUpdatePrivacy).toHaveBeenCalledWith({ allowFriendRequestsFrom: 'mutual_servers' });
  });
});

// ── #1241 / AC-19: a rejected PATCH must REVERT the control ──────────────────
//
// The positive control only asserted that the error text renders. Deleting the
// `revert(...)` call left the suite green while the slider kept advertising a
// protection level the server had refused — which on a privacy control is the
// misrepresentation AC-19 exists to prevent.
describe('PrivacySecuritySection — rejected tier PATCH reverts the control (#1241, AC-19)', () => {
  beforeEach(() => {
    drainOnceQueues();
    vi.clearAllMocks();
    mockUpdatePrivacy.mockReset();
    installPrivacyStoreMock(true);
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('returns the DM slider to the last server-confirmed level', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let rejectDm!: (e: Error) => void;
    mockUpdatePrivacy.mockReturnValueOnce(
      new Promise((_res, rej) => {
        rejectDm = rej as (e: Error) => void;
      })
    );

    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());

    const dmGroup = screen.getByRole('group', { name: /who can dm you/i });
    fireEvent.click(within(dmGroup).getByRole('button', { name: 'No One' }));
    // Optimistic: the control shows the level-0 copy before the server answers.
    expect(within(dmGroup).getByText(/Hermit mode/)).toBeInTheDocument();

    vi.advanceTimersByTime(350);
    await act(async () => {
      rejectDm(new Error('dm save rejected'));
    });

    expect(await screen.findByText('dm save rejected')).toBeInTheDocument();
    // The store's confirmed value is level 2, so the control must go back to it.
    expect(within(dmGroup).queryByText(/Hermit mode/)).not.toBeInTheDocument();
    expect(within(dmGroup).getByText(/The social butterfly/)).toBeInTheDocument();
    expect(within(dmGroup).getByRole('button', { name: 'Friends + Server' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });

  it('returns the friend-request control to the last server-confirmed mode', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    let rejectFr!: (e: Error) => void;
    mockUpdatePrivacy.mockReturnValueOnce(
      new Promise((_res, rej) => {
        rejectFr = rej as (e: Error) => void;
      })
    );

    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Who Can Send You Friend Requests')).toBeInTheDocument()
    );

    fireEvent.click(within(friendRequestGroup()).getByRole('button', { name: 'No One' }));
    expect(
      within(friendRequestGroup()).getByText('No one can send you a friend request.')
    ).toBeInTheDocument();

    vi.advanceTimersByTime(350);
    await act(async () => {
      rejectFr(new Error('friend request save rejected'));
    });

    expect(await screen.findByText('friend request save rejected')).toBeInTheDocument();
    // getState() reports a confirmed 'everyone', so the control must go back.
    expect(
      within(friendRequestGroup()).queryByText('No one can send you a friend request.')
    ).not.toBeInTheDocument();
    expect(
      within(friendRequestGroup()).getByText('Anyone can send you a friend request.')
    ).toBeInTheDocument();
    expect(within(friendRequestGroup()).getByRole('button', { name: 'Everyone' })).toHaveAttribute(
      'aria-pressed',
      'true'
    );
  });
});

// ── #1241: the SECTION must forward `loaded`, not a literal ──────────────────
//
// privacyStore's own `loaded` transitions are covered in
// tests/unit/stores/ui/privacyStore.test.ts. What survived mutation was the WIRING:
// hardcoding `isLoaded={true}` at the FriendRequestPrivacyControls call site
// left the suite green, so nothing proved the section forwards the store flag.
describe('PrivacySecuritySection — friend-request control awaits `loaded` (#1241)', () => {
  beforeEach(() => {
    drainOnceQueues();
    vi.clearAllMocks();
    mockUpdatePrivacy.mockReset();
    mockUpdatePrivacy.mockResolvedValue(undefined);
    installPrivacyStoreMock(false);
  });

  it('renders the control inert and asserts NO mode until the store reports loaded', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() =>
      expect(screen.getByText('Who Can Send You Friend Requests')).toBeInTheDocument()
    );

    const group = friendRequestGroup();
    expect(group).toHaveAttribute('aria-busy', 'true');

    for (const name of ['No One', 'Mutual Servers', 'Everyone']) {
      const option = within(group).getByRole('button', { name });
      expect(option).toBeDisabled();
      // The store default is 'everyone'; presenting it as the user's own choice
      // would advertise a more permissive setting than they may actually hold.
      expect(option).toHaveAttribute('aria-pressed', 'false');
    }

    expect(within(group).getByRole('slider')).toBeDisabled();
    expect(within(group).getByText('Loading your setting…')).toBeInTheDocument();
    expect(
      within(group).queryByText('Anyone can send you a friend request.')
    ).not.toBeInTheDocument();
  });
});

describe('PrivacySecuritySection — screen-capture protection (#2468)', () => {
  const electron = () =>
    window.electron as typeof window.electron & {
      getPlatform: () => Promise<string>;
      getContentProtection: () => Promise<boolean>;
    };

  beforeEach(() => {
    drainOnceQueues();
    vi.clearAllMocks();
    electron().getContentProtection = vi.fn().mockResolvedValue(false);
    useDraftSettingsStore.getState().teardown();
    useDraftSettingsStore.getState().initialize();
  });

  it.each(['darwin', 'win32'])(
    'renders the accessible protection toggle on %s',
    async (platform) => {
      electron().getPlatform = vi.fn().mockResolvedValue(platform);
      render(<PrivacySecuritySection />);

      const toggle = await screen.findByRole('switch', {
        name: 'Protect Concord windows from screen capture',
      });
      expect(toggle).not.toBeChecked();
      expect(
        screen.getByText(
          'When applied, Concord asks macOS or Windows to prevent its main and picture-in-picture call windows from being captured. Your operating system may not block every capture method.'
        )
      ).toBeInTheDocument();
      const hint = screen.getByText(
        'When applied, Concord asks macOS or Windows to prevent its main and picture-in-picture call windows from being captured. Your operating system may not block every capture method.'
      );
      expect(toggle).toHaveAttribute('aria-describedby', hint.id);
    }
  );

  it.each(['darwin', 'win32'])(
    'keeps protection hidden on %s when the preload methods are unavailable',
    async (platform) => {
      electron().getPlatform = vi.fn().mockResolvedValue(platform);
      const bridge = electron() as Record<string, unknown>;
      const getContentProtection = bridge.getContentProtection;
      const setContentProtection = bridge.setContentProtection;
      delete bridge.getContentProtection;
      delete bridge.setContentProtection;

      try {
        useDraftSettingsStore.getState().teardown();
        useDraftSettingsStore.getState().initialize();
        render(<PrivacySecuritySection />);
        await act(async () => {
          await Promise.resolve();
        });

        expect(
          screen.queryByRole('switch', {
            name: 'Protect Concord windows from screen capture',
          })
        ).not.toBeInTheDocument();
      } finally {
        bridge.getContentProtection = getContentProtection;
        bridge.setContentProtection = setContentProtection;
      }
    }
  );

  it('does not render the protection toggle on Linux', async () => {
    electron().getPlatform = vi.fn().mockResolvedValue('linux');
    render(<PrivacySecuritySection />);
    await screen.findByText('Privacy');
    expect(
      screen.queryByRole('switch', {
        name: 'Protect Concord windows from screen capture',
      })
    ).not.toBeInTheDocument();
  });
});

// ── A sent MFA code is never offered again ──────────────────────────────────
//
// The server accepts a TOTP code at most once, and on some routes spends it
// even when the action then fails. A prompt that kept its code behind an
// enabled Confirm would re-send a spent code and draw a confusing refusal.
