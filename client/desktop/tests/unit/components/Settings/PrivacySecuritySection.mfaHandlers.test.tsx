import { render, screen } from '../../../test-utils';
import { vi } from 'vitest';

// ── MFA action-handler coverage (#1516) ──────────────────────────────────────
// PrivacySecuritySection passes six MFA management callbacks to <MFATierSelector>
// (onResetTOTP, onRevokeWebAuthnKey, onDisableEmailSms, onSetBackupEmail,
// onToggleRecoveryHardened, onToggleRecoveryOnly). The main suite stubs
// MFATierSelector as a static div, so those handlers are never invoked and the
// extracted bodies (PrivacySecuritySection.tsx §"MFA action handlers", plus the
// module-scope signalRemovedWebAuthnCredential + base64UrlToBuffer helpers) were
// uncovered new code after the #1516 cognitive-complexity refactor. This file
// mocks MFATierSelector to CAPTURE its props, then invokes each handler directly
// against a mocked apiFetch — driving every branch (happy + error + the WebAuthn
// Signal-API best-effort path) without steering the full management UI.
//
// A separate file is required because a single test file cannot register two
// different vi.mock factories for the same module (the main suite's static-div
// mock vs. this prop-capturing mock).

const mockApiFetch = vi.fn();

// Captures the props handed to <MFATierSelector> on each render so tests can
// invoke the callback props directly. `vi.hoisted` makes the box visible to the
// hoisted vi.mock factory below.
const captured = vi.hoisted(() => ({
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- test prop-capture box; the captured props are the component's own typed MFATierSelector props, re-typed at the call site
  props: null as any,
}));

const mockSsoIdentitiesFetch = vi.fn(async () => ({
  ok: true,
  status: 200,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  json: async () => ({ identities: [] }),
  text: async () => JSON.stringify({ identities: [] }),
}));
const mockSecurityGetFetch = vi.fn(async () => ({
  ok: true,
  status: 200,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  json: async () => ({ password_login_disabled: false, trust_sso_security: false }),
  text: async () => JSON.stringify({ password_login_disabled: false, trust_sso_security: false }),
}));
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => {
    const [path, init] = args;
    const method = (init as RequestInit | undefined)?.method ?? 'GET';
    if (path === '/api/v1/users/me/sso-identities') {
      return mockSsoIdentitiesFetch();
    }
    if (path === '/api/v1/users/me/security' && method === 'GET') {
      return mockSecurityGetFetch();
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
      fetchPrivacy: vi.fn().mockResolvedValue(undefined),
      updatePrivacy: vi.fn().mockResolvedValue(undefined),
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
// Prop-capturing MFATierSelector mock — the whole point of this file.
vi.mock('@/renderer/components/Settings/MFATierSelector', () => ({
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- captured props are re-typed per-test at the invocation site
  default: (props: any) => {
    captured.props = props;
    return <div data-testid="mfa-tier-selector">MFATierSelector</div>;
  },
  WebAuthnCredential: {},
}));
vi.mock('@/renderer/components/Settings/MFASetup', () => ({
  default: () => <div data-testid="mfa-setup">MFASetup</div>,
}));
vi.mock('@/renderer/components/Auth/MFAVerifyPrompt', () => ({
  default: () => <div data-testid="mfa-verify-prompt">MFAVerifyPrompt</div>,
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
  default: ({ isOpen, children }: { isOpen: boolean; children: React.ReactNode }) =>
    isOpen ? <div data-testid="modal">{children}</div> : null,
}));
vi.mock('@/renderer/stores/clientConfigStore', () => ({
  useClientConfigStore: vi.fn((s) => s({})),
}));

import PrivacySecuritySection from '@/renderer/components/Settings/PrivacySecuritySection';

interface MFASelectorProps {
  onResetTOTP: (password: string, code: string) => Promise<boolean>;
  onRevokeWebAuthnKey: (credentialId: string, password: string) => Promise<boolean>;
  onDisableEmailSms: (password: string) => Promise<boolean>;
  onSetBackupEmail: (email: string) => Promise<boolean>;
  onToggleRecoveryHardened: (
    enabled: boolean,
    password: string,
    mfaCode?: string
  ) => Promise<boolean>;
  onToggleRecoveryOnly: (
    method: string,
    recoveryOnly: boolean,
    password: string,
    mfaCode?: string
  ) => Promise<boolean>;
}

/** ok-response helper. */
const ok = (body: unknown) => ({ ok: true, json: async () => body });
/** non-ok-response helper. */
const fail = (body: unknown = {}) => ({ ok: false, json: async () => body });

/** Render and wait until MFATierSelector props are captured. Returns them typed. */
async function renderAndCaptureHandlers(): Promise<MFASelectorProps> {
  render(<PrivacySecuritySection />);
  await vi.waitFor(() => expect(screen.getByTestId('mfa-tier-selector')).toBeInTheDocument());
  return captured.props as MFASelectorProps;
}

describe('PrivacySecuritySection — MFA action handlers (#1516)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    captured.props = null;
    // Three mount fetches: sessions, MFA status, webauthn credentials.
    mockApiFetch
      .mockResolvedValueOnce(ok({ sessions: [], past_sessions: [], revocation_mode: 'secure' }))
      .mockResolvedValueOnce(
        ok({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 5,
          backup_email: '',
        })
      )
      .mockResolvedValueOnce(ok({ credentials: [] }))
      // Any post-mount call (handler bodies + their fetchMFAStatus refreshes)
      // gets a sane default unless a test queues a specific Once response first.
      .mockResolvedValue(
        ok({
          methods: ['totp'],
          recovery_only_methods: [],
          recovery_hardened: false,
          backup_codes_remaining: 5,
          backup_email: '',
        })
      );
  });

  it('onResetTOTP posts to totp/disable and returns true on success', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({})); // the disable POST
    await expect(h.onResetTOTP('pw', '123456')).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/totp/disable',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('onResetTOTP throws the server error message on failure', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'wrong code' }));
    await expect(h.onResetTOTP('pw', '000000')).rejects.toThrow('wrong code');
  });

  it('onRevokeWebAuthnKey deletes the credential and signals the authenticator', async () => {
    // Stub the WebAuthn Signal API so signalRemovedWebAuthnCredential +
    // base64UrlToBuffer execute their happy path.
    const signal = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('PublicKeyCredential', { signalAllAcceptedCredentialIds: signal });

    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(
      ok({ remaining_credential_ids: ['AAEC', 'BBED'], user_id: 'user-uuid-123' })
    );
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/webauthn/credentials/cred-1',
      expect.objectContaining({ method: 'DELETE' })
    );
    expect(signal).toHaveBeenCalledWith(
      expect.objectContaining({ rpId: 'localhost', userId: expect.anything() })
    );
    vi.unstubAllGlobals();
  });

  it('onRevokeWebAuthnKey skips the Signal API when no remaining ids are returned', async () => {
    const signal = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal('PublicKeyCredential', { signalAllAcceptedCredentialIds: signal });

    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({})); // no remaining_credential_ids / user_id
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toBe(true);
    expect(signal).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it('onRevokeWebAuthnKey swallows Signal-API failures (best-effort)', async () => {
    const signal = vi.fn().mockRejectedValue(new Error('authenticator offline'));
    vi.stubGlobal('PublicKeyCredential', { signalAllAcceptedCredentialIds: signal });

    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(
      ok({ remaining_credential_ids: ['AAEC'], user_id: 'user-uuid-123' })
    );
    // Signal rejects, but the revoke still resolves true — the signal is a hint.
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toBe(true);
    vi.unstubAllGlobals();
  });

  it('onRevokeWebAuthnKey throws when the delete fails', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'cannot revoke last factor' }));
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).rejects.toThrow(
      'cannot revoke last factor'
    );
  });

  it('onDisableEmailSms posts to email-sms/disable and returns true', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({}));
    await expect(h.onDisableEmailSms('pw')).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/email-sms/disable',
      expect.objectContaining({ method: 'POST' })
    );
  });

  it('onDisableEmailSms throws the server error on failure', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'no email/sms factor' }));
    await expect(h.onDisableEmailSms('pw')).rejects.toThrow('no email/sms factor');
  });

  it('onSetBackupEmail PUTs the email and returns true on success', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({ backup_email: 'new@example.com' }));
    await expect(h.onSetBackupEmail('new@example.com')).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/backup-email',
      expect.objectContaining({ method: 'PUT' })
    );
  });

  it('onSetBackupEmail returns false when the server rejects', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail());
    await expect(h.onSetBackupEmail('bad')).resolves.toBe(false);
  });

  it('onToggleRecoveryHardened PUTs and returns true on success', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({ recovery_hardened: true }));
    await expect(h.onToggleRecoveryHardened(true, 'pw', '123456')).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/recovery-hardened',
      expect.objectContaining({ method: 'PUT' })
    );
  });

  it('onToggleRecoveryHardened returns false on a thrown error', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockRejectedValueOnce(new Error('network down'));
    await expect(h.onToggleRecoveryHardened(false, 'pw')).resolves.toBe(false);
  });

  it('onToggleRecoveryHardened returns false when the server responds not-ok', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail()); // res.ok === false, no exception
    await expect(h.onToggleRecoveryHardened(true, 'pw', '123456')).resolves.toBe(false);
  });

  it('onToggleRecoveryOnly adds a method and returns true', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(
      ok({ recovery_only_methods: ['totp'], recovery_hardened: true })
    );
    await expect(h.onToggleRecoveryOnly('totp', true, 'pw', '123456')).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/recovery-only',
      expect.objectContaining({ method: 'PUT' })
    );
  });

  it('onToggleRecoveryOnly returns false on a thrown error', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockRejectedValueOnce(new Error('network down'));
    await expect(h.onToggleRecoveryOnly('totp', false, 'pw')).resolves.toBe(false);
  });

  it('onToggleRecoveryOnly returns false when the server responds not-ok', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail()); // res.ok === false, no exception
    await expect(h.onToggleRecoveryOnly('totp', true, 'pw', '123456')).resolves.toBe(false);
  });
});
