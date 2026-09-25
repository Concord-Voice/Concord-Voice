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
vi.mock('@/renderer/services/system/apiClient', () => ({
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
  useUserStore: vi.fn((s) => s({ logout: vi.fn() })),
}));
// One state object for every render. A fresh `vi.fn()` per render gave the
// section's mount effect a new `fetchPrivacy` dependency on every render, so
// the effect re-ran — and re-read the MFA status — on every render, which made
// any count of status reads meaningless. The real store action is stable.
const privacyState = vi.hoisted(() => ({ current: null as unknown }));
vi.mock('@/renderer/stores/ui/privacyStore', () => ({
  usePrivacyStore: vi.fn((s) => {
    privacyState.current ??= {
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
    };
    return s(privacyState.current);
  }),
  DMPrivacyLevel: {},
}));
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
vi.mock('@/renderer/stores/ui/clientConfigStore', () => ({
  useClientConfigStore: vi.fn((s) =>
    s({ activityHistoryCapability: { status: 'confirmed-unsupported' } })
  ),
}));

import PrivacySecuritySection from '@/renderer/components/Settings/PrivacySecuritySection';
import type { MfaStepUpResult } from '@/renderer/components/Settings/mfaStepUp';

interface MFASelectorProps {
  onResetTOTP: (password: string, code: string) => Promise<MfaStepUpResult>;
  onRevokeWebAuthnKey: (credentialId: string, password: string) => Promise<MfaStepUpResult>;
  onDisableEmailSms: (password: string, mfaCode: string) => Promise<MfaStepUpResult>;
  onSetBackupEmail: (email: string, password: string, mfaCode: string) => Promise<MfaStepUpResult>;
  onToggleRecoveryHardened: (
    enabled: boolean,
    password: string,
    mfaCode?: string
  ) => Promise<MfaStepUpResult>;
  onToggleRecoveryOnly: (
    method: string,
    recoveryOnly: boolean,
    password: string,
    mfaCode?: string
  ) => Promise<MfaStepUpResult>;
}

/** ok-response helper. */
const ok = (body: unknown) => ({ ok: true, json: async () => body });
/** non-ok-response helper. An explicit `status` is required only by the
 * step-up handlers' 403 discrimination (mapMfaStepUpResponse reads
 * `res.status`); the four legacy boolean handlers only ever check `res.ok`. */
const fail = (body: unknown = {}, status?: number) => ({
  ok: false,
  status,
  json: async () => body,
});

/** Parses the JSON body of the most recent call to `path` on mockApiFetch. */
function lastBodyFor(path: string): unknown {
  const call = [...mockApiFetch.mock.calls].reverse().find((c) => c[0] === path);
  if (!call) throw new Error(`no call recorded for ${path}`);
  return JSON.parse((call[1] as { body: string }).body);
}

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

  // All six handlers now resolve an MfaStepUpResult through submitMfaStepUp
  // and never throw: the four that used to throw or return a boolean encoded
  // the legacy contract, and are rewritten to assert the mapped kind.

  it('onResetTOTP posts {password, code} to totp/disable and resolves accepted', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({})); // the disable POST
    await expect(h.onResetTOTP('pw', '123456')).resolves.toEqual({ kind: 'accepted', data: {} });
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/totp/disable',
      expect.objectContaining({ method: 'POST' })
    );
    // TOTPDisable binds the code as `code`, not `mfa_code`.
    expect(lastBodyFor('/api/v1/mfa/totp/disable')).toEqual({
      password: 'pw', // pragma: allowlist secret
      code: '123456',
    });
  });

  it('onResetTOTP carries the server text on a refusal the seam does not classify', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'wrong code' }, 403));
    await expect(h.onResetTOTP('pw', '000000')).resolves.toEqual({
      kind: 'failed',
      message: 'wrong code',
    });
  });

  it('onResetTOTP maps the 409 inline-factor refusal', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(
      fail({ error: 'Turn off email first.', inline_factor_required: true }, 409)
    );
    await expect(h.onResetTOTP('pw', '000000')).resolves.toEqual({
      kind: 'inlineFactorRequired',
      message: 'Turn off email first.',
    });
  });

  // I5: the status is refetched after an accepted change and never after a
  // refusal — a refusal changed nothing, and a refetch would spend a request.
  it('refetches the MFA status after an accepted change only (I5)', async () => {
    const h = await renderAndCaptureHandlers();
    const statusReads = () =>
      mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/status').length;
    await vi.waitFor(() => expect(statusReads()).toBe(1));

    mockApiFetch.mockResolvedValueOnce(fail({ error: 'Invalid password' }, 403));
    await h.onResetTOTP('pw', '000000');
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'Invalid password' }, 403));
    await h.onDisableEmailSms('pw', '000000');
    expect(statusReads()).toBe(1);

    mockApiFetch.mockResolvedValueOnce(ok({}));
    await h.onResetTOTP('pw', '123456');
    await vi.waitFor(() => expect(statusReads()).toBe(2));
  });

  it('a request that never left (AbortError) resolves aborted, not networkError', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockRejectedValueOnce(
      new DOMException('Request lifecycle changed before dispatch', 'AbortError')
    );
    await expect(h.onDisableEmailSms('pw', '123456')).resolves.toEqual({ kind: 'aborted' });
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
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toMatchObject({
      kind: 'accepted',
    });
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/webauthn/credentials/cred-1',
      expect.objectContaining({ method: 'DELETE' })
    );
    // The route verifies the password alone.
    expect(lastBodyFor('/api/v1/mfa/webauthn/credentials/cred-1')).toEqual({ password: 'pw' }); // pragma: allowlist secret
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
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toMatchObject({
      kind: 'accepted',
    });
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
    // Signal rejects, but the revoke still resolves accepted — the signal is a hint.
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toMatchObject({
      kind: 'accepted',
    });
    vi.unstubAllGlobals();
  });

  it('onRevokeWebAuthnKey resolves failed with the server text when the delete fails', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'Credential not found' }, 404));
    await expect(h.onRevokeWebAuthnKey('cred-1', 'pw')).resolves.toEqual({
      kind: 'failed',
      message: 'Credential not found',
    });
  });

  // The handlers now resolve an MfaStepUpResult through submitMfaStepUp — a
  // step-up-gated route never throws, so the old "throws the server error" /
  // "returns false" shape encoded pre-gate behaviour. Both are rewritten to
  // assert the mapped kind instead, and the request-body assertions (§4.3,
  // "mfa_code omitted when empty") are new coverage this task adds.

  it('onDisableEmailSms posts to email-sms/disable and resolves accepted on success', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({}));
    await expect(h.onDisableEmailSms('pw', '123456')).resolves.toEqual({
      kind: 'accepted',
      data: {},
    });
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/email-sms/disable',
      expect.objectContaining({ method: 'POST' })
    );
    expect(lastBodyFor('/api/v1/mfa/email-sms/disable')).toEqual({
      password: 'pw', // pragma: allowlist secret
      mfa_code: '123456',
    });
  });

  it('onDisableEmailSms omits mfa_code from the body when it is empty', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({}));
    await h.onDisableEmailSms('pw', '');
    expect(lastBodyFor('/api/v1/mfa/email-sms/disable')).toEqual({ password: 'pw' }); // pragma: allowlist secret
  });

  it('onDisableEmailSms maps a step-up refusal instead of throwing', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'Invalid password' }, 403));
    await expect(h.onDisableEmailSms('pw', '')).resolves.toEqual({ kind: 'invalidPassword' });
  });

  it('onSetBackupEmail PUTs the email and resolves accepted on success', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({ backup_email: 'new@example.com' }));
    await expect(h.onSetBackupEmail('new@example.com', 'pw', '123456')).resolves.toEqual({
      kind: 'accepted',
      data: { backup_email: 'new@example.com' },
    });
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/mfa/backup-email',
      expect.objectContaining({ method: 'PUT' })
    );
    expect(lastBodyFor('/api/v1/mfa/backup-email')).toEqual({
      email: 'new@example.com',
      password: 'pw', // pragma: allowlist secret
      mfa_code: '123456',
    });
  });

  it('onSetBackupEmail omits mfa_code from the body when it is empty', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({ backup_email: 'new@example.com' }));
    await h.onSetBackupEmail('new@example.com', 'pw', '');
    expect(lastBodyFor('/api/v1/mfa/backup-email')).toEqual({
      email: 'new@example.com',
      password: 'pw', // pragma: allowlist secret
    });
  });

  it('onSetBackupEmail maps a step-up refusal instead of returning false', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ password_required: true }, 403));
    await expect(h.onSetBackupEmail('bad', '', '')).resolves.toEqual({
      kind: 'passwordRequired',
    });
  });

  it('onToggleRecoveryHardened PUTs {enabled, password, mfa_code} and resolves accepted', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(ok({ recovery_hardened: true }));
    await expect(h.onToggleRecoveryHardened(true, 'pw', '123456')).resolves.toEqual({
      kind: 'accepted',
      data: { recovery_hardened: true },
    });
    expect(lastBodyFor('/api/v1/mfa/recovery-hardened')).toEqual({
      enabled: true,
      password: 'pw', // pragma: allowlist secret
      mfa_code: '123456',
    });
  });

  it('onToggleRecoveryHardened resolves networkError on a rejected fetch', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockRejectedValueOnce(new Error('network down'));
    await expect(h.onToggleRecoveryHardened(false, 'pw')).resolves.toEqual({
      kind: 'networkError',
    });
  });

  it('onToggleRecoveryHardened maps an mfa_required refusal (now on the step-up seam)', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(
      fail({ error: 'MFA verification required', mfa_required: true, methods: ['totp'] }, 403)
    );
    await expect(h.onToggleRecoveryHardened(true, 'pw', '')).resolves.toEqual({
      kind: 'mfaRequired',
      methods: ['totp'],
    });
  });

  it('onToggleRecoveryHardened refetches the status when the echo lacks the field', async () => {
    const h = await renderAndCaptureHandlers();
    const statusReads = () =>
      mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/status').length;
    await vi.waitFor(() => expect(statusReads()).toBe(1));
    mockApiFetch.mockResolvedValueOnce(ok({}));
    await h.onToggleRecoveryHardened(true, 'pw', '123456');
    await vi.waitFor(() => expect(statusReads()).toBe(2));
  });

  it('onToggleRecoveryOnly adds a method and resolves accepted', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(
      ok({ recovery_only_methods: ['totp'], recovery_hardened: true })
    );
    await expect(h.onToggleRecoveryOnly('totp', true, 'pw', '123456')).resolves.toMatchObject({
      kind: 'accepted',
    });
    expect(lastBodyFor('/api/v1/mfa/recovery-only')).toEqual({
      methods: ['totp'],
      password: 'pw', // pragma: allowlist secret
      mfa_code: '123456',
    });
  });

  it('onToggleRecoveryOnly refetches the status when the echo lacks the method list', async () => {
    const h = await renderAndCaptureHandlers();
    const statusReads = () =>
      mockApiFetch.mock.calls.filter((c) => c[0] === '/api/v1/mfa/status').length;
    await vi.waitFor(() => expect(statusReads()).toBe(1));
    mockApiFetch.mockResolvedValueOnce(ok({}));
    await h.onToggleRecoveryOnly('totp', true, 'pw', '123456');
    await vi.waitFor(() => expect(statusReads()).toBe(2));
  });

  it('onToggleRecoveryOnly resolves networkError on a rejected fetch', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockRejectedValueOnce(new Error('network down'));
    await expect(h.onToggleRecoveryOnly('totp', false, 'pw')).resolves.toEqual({
      kind: 'networkError',
    });
  });

  it('onToggleRecoveryOnly resolves rateLimited on a 429', async () => {
    const h = await renderAndCaptureHandlers();
    mockApiFetch.mockResolvedValueOnce(fail({ error: 'Too many verification attempts' }, 429));
    await expect(h.onToggleRecoveryOnly('totp', true, 'pw', '123456')).resolves.toEqual({
      kind: 'rateLimited',
    });
  });
});

// ── F8: an unreadable MFA status is reported, not rendered as "MFA off" ──────

describe('PrivacySecuritySection — MFA status load (F8)', () => {
  const STATUS = {
    methods: ['totp'],
    recovery_only_methods: [],
    recovery_hardened: false,
    backup_codes_remaining: 5,
    backup_email: '',
  };
  // Routed by path, not queued: other sections' mount fetches (/friends,
  // presence settings) run first and would consume a queued fixture.
  let statusReply: () => Promise<unknown>;

  beforeEach(() => {
    vi.clearAllMocks();
    captured.props = null;
    statusReply = async () => ok(STATUS);
    mockApiFetch.mockReset().mockImplementation(async (path: string) => {
      if (path === '/api/v1/mfa/status') return statusReply();
      if (path === '/api/v1/mfa/webauthn/credentials') return ok({ credentials: [] });
      if (path === '/api/v1/sessions') {
        return ok({ sessions: [], past_sessions: [], revocation_mode: 'secure' });
      }
      return ok({});
    });
  });

  it('hides the tier controls and offers a retry when the status read fails', async () => {
    statusReply = async () => fail({ error: 'Failed to read MFA status' }, 500);
    render(<PrivacySecuritySection />);

    expect(
      await screen.findByText(
        "We couldn't load your MFA settings, so they're hidden until they load. Nothing has changed."
      )
    ).toBeInTheDocument();
    expect(screen.queryByTestId('mfa-tier-selector')).not.toBeInTheDocument();

    statusReply = async () => ok(STATUS);
    // Its own name: the page carries other "Try again" buttons.
    screen.getByRole('button', { name: 'Reload MFA settings' }).click();
    expect(await screen.findByTestId('mfa-tier-selector')).toBeInTheDocument();
    expect(screen.queryByText(/We couldn't load your MFA settings/)).not.toBeInTheDocument();
  });

  it('shows neither the controls nor the error while the first read is in flight', async () => {
    let release: (v: unknown) => void = () => {};
    statusReply = () => new Promise((r) => (release = r));
    render(<PrivacySecuritySection />);
    expect(await screen.findByText('Loading your MFA settings…')).toBeInTheDocument();
    expect(screen.queryByTestId('mfa-tier-selector')).not.toBeInTheDocument();
    release(ok(STATUS));
    expect(await screen.findByTestId('mfa-tier-selector')).toBeInTheDocument();
  });
});
