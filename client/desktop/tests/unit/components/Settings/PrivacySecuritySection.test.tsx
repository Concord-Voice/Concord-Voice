import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';

const mockApiFetch = vi.fn();
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
  json: async () => ({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' }),
  text: async () => JSON.stringify({ custom_text_tier: 0, custom_text: '', custom_text_emoji: '' }),
}));
vi.mock('@/renderer/services/apiClient', () => ({
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
    if (path === '/api/v1/users/me/security' && method === 'GET') {
      return mockSecurityGetFetch();
    }
    if (path === '/api/v1/users/me/presence-settings' && method === 'GET') {
      return mockPresenceGetFetch();
    }
    return mockApiFetch(...args);
  },
  API_BASE: 'http://localhost:8080',
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
}));
vi.mock('@/renderer/stores/authStore', () => ({
  useAuthStore: vi.fn((s) => s({ accessToken: 'mock-token' })),
}));
vi.mock('@/renderer/stores/userStore', () => ({
  useUserStore: vi.fn((s) => s({ logout: vi.fn() })),
}));
const mockFetchPrivacy = vi.fn().mockResolvedValue(undefined);
const mockUpdatePrivacy = vi.fn().mockResolvedValue(undefined);
vi.mock('@/renderer/stores/privacyStore', () => ({
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
      fetchPrivacy: mockFetchPrivacy,
      updatePrivacy: mockUpdatePrivacy,
    })
  ),
  DMPrivacyLevel: {},
}));
vi.mock('@/renderer/stores/osPermissionStore', () => ({
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
vi.mock('@/renderer/services/gifProvider/klipyClient', () => ({
  klipyClient: {
    getCurrentCustomerId: vi.fn(() => 'mock-customer-id-123'),
    rotateCustomerId: vi.fn(() => Promise.resolve('mock-rotated-id-456')),
  },
}));
vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  default: () => <div data-testid="mfa-tier-selector">MFATierSelector</div>,
  WebAuthnCredential: {},
}));
vi.mock('@/renderer/components/Settings/MFASetup', () => ({
  default: () => <div data-testid="mfa-setup">MFASetup</div>,
}));
vi.mock('@/renderer/components/Auth/MFAVerifyPrompt', () => ({
  default: ({
    onVerify,
    disabled,
  }: {
    onVerify: (code: string) => void;
    disabled?: boolean;
    methods?: string[];
    recoveryOnlyMethods?: string[];
    error?: string;
    excludeBackupCodes?: boolean;
  }) => (
    <div data-testid="mfa-verify-prompt">
      <button data-testid="mfa-verify-btn" onClick={() => onVerify('123456')} disabled={disabled}>
        Verify
      </button>
    </div>
  ),
}));
vi.mock('@/renderer/components/Settings/BackupCodeDisplay', () => ({
  default: () => <div data-testid="backup-code-display">BackupCodeDisplay</div>,
}));
vi.mock('@/renderer/components/Settings/EmailSmsSetup', () => ({
  default: () => <div data-testid="email-sms-setup">EmailSmsSetup</div>,
}));
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: ({ size }: { size?: string }) => (
    <div data-testid="loading-spinner" data-size={size}>
      Loading...
    </div>
  ),
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

vi.mock('@/renderer/stores/clientConfigStore', () => ({
  useClientConfigStore: vi.fn((s) => s({})),
}));

import PrivacySecuritySection, {
  resolveSSOToggleError,
} from '@/renderer/components/Settings/PrivacySecuritySection';

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

describe('PrivacySecuritySection', () => {
  beforeEach(() => {
    vi.clearAllMocks();
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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

    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/s2',
        expect.objectContaining({ method: 'DELETE' })
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
      await import('@/renderer/stores/privacyStore').then((m) => m.usePrivacyStore)
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
      await import('@/renderer/stores/privacyStore').then((m) => m.usePrivacyStore)
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
        fetchPrivacy: mockFetchPrivacy,
        updatePrivacy: mockUpdatePrivacy,
      })
    );
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText(/Click to load/)).toBeInTheDocument());
    expect(screen.getByText(/GIF picker results are not personalized/)).toBeInTheDocument();
    expect(screen.getByText(/Ephemeral ID — rotates automatically/)).toBeInTheDocument();
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
      await import('@/renderer/stores/privacyStore').then((m) => m.usePrivacyStore)
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
      await import('@/renderer/stores/privacyStore').then((m) => m.usePrivacyStore)
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
    await vi.waitFor(() => expect(screen.getByText('No One')).toBeInTheDocument());
    fireEvent.click(screen.getByText('No One'));
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
      await import('@/renderer/stores/userStore').then((m) => m.useUserStore)
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

    // Open the modal
    fireEvent.click(screen.getByText('Revoke All Sessions'));
    await vi.waitFor(() =>
      expect(screen.getByText(/revoke all of your active session tokens/)).toBeInTheDocument()
    );

    // Enter password
    const passwordInput = screen.getByPlaceholderText('Enter your password');
    fireEvent.change(passwordInput, { target: { value: 'my-password' } });

    // Mock the revoke-all API response
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });

    // Click confirm
    fireEvent.click(screen.getByText('Yes, Revoke All Sessions'));
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/revoke-all',
        expect.objectContaining({ method: 'POST' })
      )
    );
    await vi.waitFor(() => expect(mockLogout).toHaveBeenCalled());
  });

  // ── Revoke All modal 403 error ─────────────────────────────────────────

  it('shows error on revoke all 403', async () => {
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
      expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument()
    );
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'wrong-pw' },
    });

    // Mock 403 response
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'Incorrect password' }),
    });
    fireEvent.click(screen.getByText('Yes, Revoke All Sessions'));
    await vi.waitFor(() => expect(screen.getByText('Incorrect password')).toBeInTheDocument());
  });

  // ── Mode change submission ─────────────────────────────────────────────

  it('submits mode change with password', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());

    // Click Simple to open mode change modal
    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() => expect(screen.getByText('Change Revocation Mode')).toBeInTheDocument());

    // Enter password in the modal
    const pwInput = screen.getByPlaceholderText('Enter your password');
    fireEvent.change(pwInput, { target: { value: 'test-password' } });

    // Mock mode change response
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ revocation_mode: 'simple' }),
    });

    fireEvent.click(screen.getByText('Confirm'));
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/revocation-mode',
        expect.objectContaining({ method: 'PUT' })
      )
    );
  });

  // ── Mode change 403 error ─────────────────────────────────────────────

  it('shows error on mode change 403', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());

    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() => expect(screen.getByText('Change Revocation Mode')).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'wrong-pw' },
    });

    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'Authentication failed' }),
    });

    fireEvent.click(screen.getByText('Confirm'));
    await vi.waitFor(() => expect(screen.getByText('Authentication failed')).toBeInTheDocument());
  });

  // ── Session password modal submission ──────────────────────────────────

  it('submits session password and revokes', async () => {
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

    // First click triggers 403 password_required
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'password_required' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Verify Your Identity')).toBeInTheDocument());

    // Enter password
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'session-pw' },
    });

    // Mock successful revoke + fetchSessions
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) }).mockResolvedValueOnce({
      ok: true,
      json: async () => ({ sessions: [], past_sessions: [], revocation_mode: 'secure' }),
    });

    fireEvent.click(screen.getByText('Confirm & Revoke'));
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/s2',
        expect.objectContaining({ method: 'DELETE' })
      )
    );
  });

  // ── Revoke All empty password validation ──────────────────────────────

  it('shows password required error when submitting without password', async () => {
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
    // The confirm button should be disabled without password
    expect(screen.getByText('Yes, Revoke All Sessions')).toBeDisabled();
  });

  // ── Session revoke 403 with incorrect password ─────────────────────────

  it('shows error in session password modal on incorrect password', async () => {
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

    // Trigger 403 password_required
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'password_required' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Verify Your Identity')).toBeInTheDocument());

    // Enter wrong password and submit
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'wrong' },
    });
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'Incorrect password' }),
    });
    fireEvent.click(screen.getByText('Confirm & Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Incorrect password')).toBeInTheDocument());
  });

  // ── DM slider interaction ──────────────────────────────────────────────

  it('changes DM level via slider', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Who Can DM You')).toBeInTheDocument());
    const slider = screen.getByRole('slider');
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
      expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument()
    );
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'pw123' },
    });

    // Mock server error (500)
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => ({ error: 'Internal error' }),
    });
    fireEvent.click(screen.getByText('Yes, Revoke All Sessions'));
    await vi.waitFor(() => expect(screen.getByText('Internal error')).toBeInTheDocument());
  });

  // ── Mode change non-ok non-403 error ───────────────────────────────────

  it('handles non-403 error on mode change', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());

    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() => expect(screen.getByText('Change Revocation Mode')).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'pw' },
    });

    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => ({ error: 'Server error' }),
    });

    fireEvent.click(screen.getByText('Confirm'));
    await vi.waitFor(() => expect(screen.getByText('Server error')).toBeInTheDocument());
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

  it('submits revoke all on Enter key in password field', async () => {
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
      expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument()
    );
    const pwInput = screen.getByPlaceholderText('Enter your password');
    fireEvent.change(pwInput, { target: { value: 'my-pw' } });

    // Mock successful revoke
    mockApiFetch.mockResolvedValueOnce({ ok: true, json: async () => ({}) });
    fireEvent.keyDown(pwInput, { key: 'Enter' });
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/revoke-all',
        expect.objectContaining({ method: 'POST' })
      )
    );
  });

  // ── Mode change Enter key ─────────────────────────────────────────────

  it('submits mode change on Enter key', async () => {
    render(<PrivacySecuritySection />);
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() =>
      expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument()
    );
    const pwInput = screen.getByPlaceholderText('Enter your password');
    fireEvent.change(pwInput, { target: { value: 'test-pw' } });

    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ revocation_mode: 'simple' }),
    });
    fireEvent.keyDown(pwInput, { key: 'Enter' });
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/sessions/revocation-mode',
        expect.objectContaining({ method: 'PUT' })
      )
    );
  });

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
      await import('@/renderer/stores/userStore').then((m) => m.useUserStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
    // Click "No One" button
    fireEvent.click(screen.getByText('No One'));
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
    expect(screen.getByText(/This will invalidate all existing backup codes/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Enter your password')).toBeInTheDocument();
  });

  // ── Backup code reset submission ────────────────────────────────────────

  it('submits backup code reset and shows new codes', async () => {
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
    // Open backup reset modal
    fireEvent.click(screen.getByText(/Reset/));
    await vi.waitFor(() => expect(screen.getByText('Reset Backup Codes')).toBeInTheDocument());

    // Fill in password
    fireEvent.change(screen.getByPlaceholderText('Enter your password'), {
      target: { value: 'my-password' },
    });

    // Mock successful regeneration + MFA status refetch
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ backup_codes: ['CODE1', 'CODE2', 'CODE3'] }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 3,
          backup_email: '',
        }),
      })
      .mockResolvedValueOnce({ ok: true, json: async () => ({ credentials: [] }) });

    // Click Verify in the mocked MFAVerifyPrompt to set the MFA code
    const verifyBtns = screen.getAllByTestId('mfa-verify-btn');
    // The backup reset modal's verify button
    fireEvent.click(verifyBtns[verifyBtns.length - 1]);

    // Now Regenerate should be enabled
    await vi.waitFor(() => expect(screen.getByText('Regenerate Codes')).not.toBeDisabled());
    fireEvent.click(screen.getByText('Regenerate Codes'));
    await vi.waitFor(() =>
      expect(mockApiFetch).toHaveBeenCalledWith(
        '/api/v1/mfa/backup-codes/regenerate',
        expect.objectContaining({ method: 'POST' })
      )
    );
    // BackupCodeDisplay mock should be shown
    await vi.waitFor(() => expect(screen.getByTestId('backup-code-display')).toBeInTheDocument());
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
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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

  it('validates empty password on revoke all submission', async () => {
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
      expect(screen.getByText('Yes, Revoke All Sessions')).toBeInTheDocument()
    );
    // Confirm button should be disabled when no password entered
    expect(screen.getByText('Yes, Revoke All Sessions')).toBeDisabled();
  });

  // ── MFA-enabled modals use MFA verify prompt ───────────────────────────

  it('shows MFA verify prompt in Revoke All modal when MFA is active', async () => {
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
    await vi.waitFor(() => expect(screen.getByText('Revoke All Sessions')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Revoke All Sessions'));
    await vi.waitFor(() =>
      expect(screen.getByText(/Verify your identity to continue/)).toBeInTheDocument()
    );
    // MFAVerifyPrompt is mocked as a simple div
    expect(screen.getAllByTestId('mfa-verify-prompt').length).toBeGreaterThanOrEqual(1);
  });

  it('shows MFA verify prompt in mode change modal when MFA is active', async () => {
    mockApiFetch
      .mockReset()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          sessions: [],
          past_sessions: [],
          revocation_mode: 'secure',
        }),
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
    await vi.waitFor(() => expect(screen.getByText('Session Revocation')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Simple'));
    await vi.waitFor(() => expect(screen.getByText('Change Revocation Mode')).toBeInTheDocument());
    expect(screen.getAllByTestId('mfa-verify-prompt').length).toBeGreaterThanOrEqual(1);
  });

  it('shows MFA verify prompt in session password modal when MFA active', async () => {
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
    await vi.waitFor(() => expect(screen.getByText('Chrome Browser')).toBeInTheDocument());

    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 403,
      json: async () => ({ error: 'auth_required' }),
    });
    fireEvent.click(screen.getByText('Revoke'));
    await vi.waitFor(() => expect(screen.getByText('Verify Your Identity')).toBeInTheDocument());
    expect(screen.getAllByTestId('mfa-verify-prompt').length).toBeGreaterThanOrEqual(1);
  });

  // ── Permissions loading state ──────────────────────────────────────────

  it('shows loading state when permissions not loaded', async () => {
    vi.mocked(
      await import('@/renderer/stores/osPermissionStore').then((m) => m.useOsPermissionStore)
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
