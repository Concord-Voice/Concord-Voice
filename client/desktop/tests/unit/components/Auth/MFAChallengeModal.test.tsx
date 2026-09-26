import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { vi } from 'vitest';
import {
  useMFAChallengeStore,
  type MFAChallengeResult,
} from '@/renderer/stores/auth/mfaChallengeStore';
import { completeSSOMFA, abandonSSOReservation } from '@/renderer/services/system/ssoService';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// #2424: mock only completeSSOMFA (the sso_login verify path); keep the real
// SSOServiceError class so the modal's error-branch instanceof check works.
// #2394 adds abandonSSOReservation to the same partial mock so the Cancel
// purpose-gate can be observed without an Electron bridge.
vi.mock('@/renderer/services/system/ssoService', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/ssoService')>();
  return { ...actual, completeSSOMFA: vi.fn(), abandonSSOReservation: vi.fn() };
});

// safeJson is mocked as a transparent passthrough so existing fetch mocks
// (which only set ok / json / status) continue to work without supplying
// Content-Type headers. The real safeJson is exercised by apiClient tests.
vi.mock('@/renderer/services/system/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
  ensureMachineId: vi.fn().mockResolvedValue('mock-machine-id'),
  safeJson: async <T,>(res: { json: () => Promise<T> }): Promise<T> => res.json(),
}));

// Mock child components to isolate MFAChallengeModal behavior
// Counts mounts, so a test can see the modal remount the input to clear it.
const totpMounts = vi.hoisted(() => ({ count: 0 }));
vi.mock('@/renderer/components/Auth/TOTPInput', async () => {
  const React = await import('react');
  return {
    default: function MockTOTPInput({
      onSubmit,
      disabled,
      error,
    }: {
      onSubmit: (code: string) => void;
      disabled?: boolean;
      error?: string;
    }) {
      const [mount] = React.useState(() => ++totpMounts.count);
      return (
        <div data-testid="totp-input" data-mount={mount}>
          <button data-testid="totp-submit" disabled={disabled} onClick={() => onSubmit('123456')}>
            Submit TOTP
          </button>
          {error && <span data-testid="totp-error">{error}</span>}
        </div>
      );
    },
  };
});

vi.mock('@/renderer/components/Auth/BackupCodeInput', () => ({
  default: ({
    onSubmit,
    disabled,
    error,
  }: {
    onSubmit: (code: string) => void;
    disabled?: boolean;
    error?: string;
  }) => (
    <div data-testid="backup-input">
      <button
        data-testid="backup-submit"
        disabled={disabled}
        onClick={() => onSubmit('BACKUP1234')}
      >
        Submit Backup
      </button>
      {error && <span data-testid="backup-error">{error}</span>}
    </div>
  ),
}));

// Build a minimal-but-valid PublicKeyCredential mock for the WebAuthn parity
// tests. The component's handler runs btoa(String.fromCodePoint(...))
// across each ArrayBuffer field, so any well-formed buffer works.
function makeMockCredential(): Credential {
  const buf = (bytes: number[]) => new Uint8Array(bytes).buffer;
  return {
    id: 'mock-cred-id',
    rawId: buf([1, 2, 3, 4]),
    type: 'public-key',
    response: {
      authenticatorData: buf([5, 6, 7, 8]),
      clientDataJSON: buf([9, 10, 11, 12]),
      signature: buf([13, 14, 15, 16]),
      userHandle: null,
    },
  } as unknown as Credential;
}

vi.mock('@/renderer/components/Auth/WebAuthnPrompt', () => ({
  default: ({
    onSuccess,
    onError,
    onCancel,
  }: {
    requestOptions: PublicKeyCredentialRequestOptions;
    onSuccess: (credential: Credential) => void;
    onError: (msg: string) => void;
    onCancel: () => void;
  }) => (
    <div data-testid="webauthn-prompt">
      <button data-testid="webauthn-success" onClick={() => onSuccess(makeMockCredential())}>
        WebAuthn Success
      </button>
      <button data-testid="webauthn-error" onClick={() => onError('webauthn failed')}>
        WebAuthn Error
      </button>
      <button data-testid="webauthn-cancel" onClick={onCancel}>
        WebAuthn Cancel
      </button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/MFAMethodPicker', () => {
  const React = require('react');
  const component = ({
    onSelect,
    onCancel,
  }: {
    methods: string[];
    currentMethod: string;
    onSelect: (method: string) => void;
    onCancel?: () => void;
    excludeMethods?: string[];
  }) => (
    <div data-testid="method-picker">
      <button data-testid="pick-totp" onClick={() => onSelect('totp')}>
        Pick TOTP
      </button>
      <button data-testid="pick-backup" onClick={() => onSelect('backup')}>
        Pick Backup
      </button>
      {onCancel && (
        <button data-testid="picker-cancel" onClick={onCancel}>
          Cancel Picker
        </button>
      )}
    </div>
  );

  return {
    default: component,
    getDefaultMethod: (methods: string[]) => {
      if (methods.includes('webauthn')) return 'webauthn';
      if (methods.includes('totp')) return 'totp';
      if (methods.includes('email') || methods.includes('sms')) return 'email-sms';
      return 'totp';
    },
    getAvailableCategories: (methods: string[]) => {
      const cats: string[] = [];
      if (methods.includes('webauthn')) cats.push('webauthn');
      if (methods.includes('totp')) cats.push('totp');
      if (methods.includes('email') || methods.includes('sms')) cats.push('email-sms');
      if (cats.length > 0) cats.push('backup');
      return cats;
    },
  };
});

import MFAChallengeModal from '@/renderer/components/Auth/MFAChallengeModal';

// Chromium fires `close` only after the dialog has closed; the modal ignores a
// close event that arrives while its dialog is open again.
function fireNativeClose(dialog: HTMLDialogElement) {
  dialog.removeAttribute('open');
  fireEvent(dialog, new Event('close'));
}

// Stub PublicKeyCredentialRequestOptions for tests — values don't matter
// because the WebAuthnPrompt mock ignores them; the modal only checks
// whether webauthnOptions is non-null to decide whether to mount the prompt.
const mockWebAuthnOptions = {
  challenge: new Uint8Array([1, 2, 3, 4]).buffer,
  timeout: 60000,
  rpId: 'test',
} as unknown as PublicKeyCredentialRequestOptions;

describe('MFAChallengeModal', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
  });

  it('renders nothing when no challenge token is present', () => {
    // The dialog portals out of the render container, so check the document.
    render(<MFAChallengeModal />);
    expect(document.querySelector('dialog.mfa-challenge-dialog')).toBeNull();
    expect(document.querySelector('[inert]')).toBeNull();
  });

  it('describes the dialog with its subtitle', () => {
    render(<MFAChallengeModal />);
    act(() => {
      void useMFAChallengeStore
        .getState()
        .showChallenge('desc-token', ['totp'], 'suspicious_refresh');
    });
    const dialog = document.querySelector('dialog.mfa-challenge-dialog') as HTMLDialogElement;
    const described = document.getElementById(dialog.getAttribute('aria-describedby') ?? '');
    expect(described).toHaveClass('mfa-modal-desc');
    expect(described?.textContent).not.toBe('');
  });

  it('routes an SSO MFA proof through completeSSOMFA (main), not the renderer POST (#2424)', async () => {
    const completion = { accessToken: 'sso-a', sessionId: 'sso-s', credentialOwner: 5 };
    vi.mocked(completeSSOMFA).mockResolvedValueOnce(completion);
    let resolved: MFAChallengeResult | undefined;
    useMFAChallengeStore.setState({
      challengeToken: 'sso-chal',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      purpose: 'sso_login',
      ssoContext: { provider: 'google', credentialOwner: 5 },
      resolve: (r) => {
        resolved = r;
      },
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => expect(completeSSOMFA).toHaveBeenCalledTimes(1));
    expect(completeSSOMFA).toHaveBeenCalledWith(
      {
        provider: 'google',
        mfaChallengeToken: 'sso-chal',
        credentialOwner: 5,
        method: 'totp',
        code: '123456',
      },
      expect.anything()
    );
    // The SSO branch NEVER hits the renderer /auth/mfa/verify POST.
    expect(mockFetch).not.toHaveBeenCalled();
    await waitFor(() => expect(resolved).toEqual({ verified: true, ssoCompletion: completion }));
  });

  // The modal stays open for a retry, and never shows a bare error code.
  it.each([
    ['the server message', { error: 'Invalid MFA code' }, 'Invalid MFA code'],
    [
      'readable copy for a code main raises',
      { error_code: 'sso_mfa_verify_failed' },
      "Couldn't check your code. Check your connection and try again.",
    ],
    [
      'the generic message for an unknown code',
      { error_code: 'mfa_code_invalid' },
      'Verification failed. Please try again.',
    ],
  ])('keeps the SSO modal open on a failed proof and shows %s (#2424)', async (_, body, shown) => {
    const { SSOServiceError } = await vi.importActual<
      typeof import('@/renderer/services/system/ssoService')
    >('@/renderer/services/system/ssoService');
    vi.mocked(completeSSOMFA).mockRejectedValueOnce(
      new SSOServiceError(401, 'sso_complete_mfa_failed_401', body)
    );
    let resolved: MFAChallengeResult | undefined;
    useMFAChallengeStore.setState({
      challengeToken: 'sso-chal',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      purpose: 'sso_login',
      ssoContext: { provider: 'apple', credentialOwner: 8 },
      resolve: (r) => {
        resolved = r;
      },
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => expect(screen.getByTestId('totp-error')).toHaveTextContent(shown));
    expect(screen.getByTestId('totp-error').textContent).not.toMatch(/sso_|mfa_code/);
    // The challenge is NOT resolved on error — the modal stays open for retry.
    expect(resolved).toBeUndefined();
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it('renders modal when challenge token is set', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    expect(screen.getByText('Verify Your Identity')).toBeInTheDocument();
  });

  it('shows TOTP subtitle when in totp mode', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    expect(
      screen.getByText('Enter the 6-digit code from your authenticator app')
    ).toBeInTheDocument();
  });

  it('shows TOTP input when totp is the default method', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    expect(screen.getByTestId('totp-input')).toBeInTheDocument();
  });

  it('shows "Choose another form" when multiple methods are available', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp', 'webauthn'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    expect(screen.getByText('Choose another form of verification')).toBeInTheDocument();
  });

  it('switches to method-select when "Choose another form" is clicked', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp', 'webauthn'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByText('Choose another form of verification'));
    expect(screen.getByText('Select a verification method')).toBeInTheDocument();
    expect(screen.getByTestId('method-picker')).toBeInTheDocument();
  });

  it('switches from method-select to totp when a method is picked', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp', 'webauthn'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByText('Choose another form of verification'));
    fireEvent.click(screen.getByTestId('pick-totp'));
    expect(screen.getByTestId('totp-input')).toBeInTheDocument();
  });

  it('switches from method-select to backup when backup is picked', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp', 'webauthn'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByText('Choose another form of verification'));
    fireEvent.click(screen.getByTestId('pick-backup'));
    expect(screen.getByTestId('backup-input')).toBeInTheDocument();
  });

  it('sends TOTP verify request to server', async () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      resolve: mockResolve,
    });

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        'http://localhost:8080/api/v1/auth/mfa/verify',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('"method":"totp"'),
        })
      );
    });
  });

  it('calls completeChallenge with payload on successful verification', async () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      resolve: mockResolve,
    });

    const verifyResponseBody = {
      access_token: 'jwt-after-mfa',
      session_id: 'sess-after-mfa',
      refresh_token: 'ref-after-mfa',
    };
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => verifyResponseBody,
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(mockResolve).toHaveBeenCalledWith({
        verified: true,
        payload: verifyResponseBody,
      });
    });
  });

  it('does NOT call completeChallenge on failed verification (modal stays open)', async () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      resolve: mockResolve,
    });

    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid code' }),
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(screen.getByTestId('totp-error')).toHaveTextContent('Invalid code');
    });
    expect(mockResolve).not.toHaveBeenCalled();
    expect(useMFAChallengeStore.getState().challengeToken).toBe('test-token');
  });

  // Each code is accepted once, so a refused code is either wrong or already
  // spent: the input remounts empty rather than offering it again.
  it.each([
    [
      'a refusal',
      () =>
        mockFetch.mockResolvedValueOnce({
          ok: false,
          json: async () => ({ error: 'Invalid MFA code' }),
        }),
    ],
    ['a network error', () => mockFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'))],
  ])('remounts the code input after %s', async (_label, arrange) => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      resolve: vi.fn(),
    });
    arrange();

    render(<MFAChallengeModal />);
    const before = screen.getByTestId('totp-input').dataset.mount;
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => expect(screen.getByTestId('totp-error')).toBeInTheDocument());
    expect(screen.getByTestId('totp-input').dataset.mount).not.toBe(before);
  });

  it('treats res.json() parse failure on a 2xx response as a verification failure (modal stays open)', async () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      resolve: mockResolve,
    });

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => {
        throw new SyntaxError('Unexpected token < in JSON at position 0');
      },
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(screen.getByTestId('totp-error')).toHaveTextContent(
        'Verification failed. Please try again.'
      );
    });
    expect(mockResolve).not.toHaveBeenCalled();
    expect(useMFAChallengeStore.getState().challengeToken).toBe('test-token');
  });

  it('shows error on failed verification with server-supplied message', async () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid TOTP code' }),
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(screen.getByTestId('totp-error')).toHaveTextContent('Invalid TOTP code');
    });
  });

  it('falls back to "Verification failed" when error response body has no error field', async () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({}),
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(screen.getByTestId('totp-error')).toHaveTextContent('Verification failed');
    });
  });

  it('shows generic error on network failure', async () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    mockFetch.mockRejectedValueOnce(new Error('network'));

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(screen.getByTestId('totp-error')).toHaveTextContent(
        'Verification failed. Please try again.'
      );
    });
  });

  it('sends backup_code verify request when backup input is submitted', async () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({}),
    });

    render(<MFAChallengeModal />);
    // Switch to method picker, then pick backup
    fireEvent.click(screen.getByText('Choose another form of verification'));
    fireEvent.click(screen.getByTestId('pick-backup'));
    fireEvent.click(screen.getByTestId('backup-submit'));

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        expect.stringContaining('/mfa/verify'),
        expect.objectContaining({
          body: expect.stringContaining('"method":"backup_code"'),
        })
      );
    });
  });

  it('calls clearChallenge when cancel button is clicked', () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      resolve: mockResolve,
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByText('Cancel'));

    // clearChallenge resolves with { verified: false }
    expect(mockResolve).toHaveBeenCalledWith({ verified: false });
    // challengeToken should be cleared
    expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
  });

  it('shows WebAuthn fallback when webauthn mode but no options', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['webauthn'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    expect(
      screen.getByText('WebAuthn verification will be triggered by the server challenge.')
    ).toBeInTheDocument();
  });

  it('shows email-sms subtitle and input when email method is available', () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['email'],
      recoveryOnlyMethods: [],
    });

    render(<MFAChallengeModal />);
    expect(screen.getByText('Enter the verification code sent to you')).toBeInTheDocument();
    expect(screen.getByTestId('totp-input')).toBeInTheDocument();
  });

  it('disables TOTP submit AND Cancel button while loading', async () => {
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['totp'],
      recoveryOnlyMethods: [],
    });

    // Never resolve to keep loading state
    mockFetch.mockReturnValueOnce(new Promise(() => {}));

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('totp-submit'));

    await waitFor(() => {
      expect(screen.getByTestId('totp-submit')).toBeDisabled();
    });
    // Cancel must also be disabled to prevent the verify-while-loading race
    // condition: cancel mid-fetch would resolve the promise as { verified:
    // false } while completeChallenge later tries to fire on the same
    // (now-null) resolver.
    expect(screen.getByText('Cancel')).toBeDisabled();
  });

  // ── WebAuthn parity tests (spec §7.1 item 4) ─────────────────────────────
  // These exercise handleWebAuthnSuccess via the WebAuthnPrompt mock's
  // onSuccess button. The mock mounts only when webauthnOptions is non-null
  // in the store; production callers that pass options through (e.g., when
  // the SSO MFA bridge is extended to forward webauthn_options from the
  // server) will benefit from the same code path tested here.

  it('WebAuthn: calls completeChallenge with payload on successful verification', async () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['webauthn'],
      recoveryOnlyMethods: [],
      webauthnOptions: mockWebAuthnOptions,
      resolve: mockResolve,
    });

    const verifyResponseBody = {
      access_token: 'jwt-webauthn',
      session_id: 'sess-webauthn',
    };
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => verifyResponseBody,
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('webauthn-success'));

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        'http://localhost:8080/api/v1/auth/mfa/verify',
        expect.objectContaining({
          method: 'POST',
          body: expect.stringContaining('"method":"webauthn"'),
        })
      );
    });
    await waitFor(() => {
      expect(mockResolve).toHaveBeenCalledWith({
        verified: true,
        payload: verifyResponseBody,
      });
    });
  });

  it('WebAuthn: does NOT call completeChallenge on failed verification (modal stays open)', async () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['webauthn'],
      recoveryOnlyMethods: [],
      webauthnOptions: mockWebAuthnOptions,
      resolve: mockResolve,
    });

    mockFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Invalid assertion' }),
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByTestId('webauthn-success'));

    // The modal stays open: there is no totp-error testid in webauthn mode
    // (the fallback only renders when options are null), so we assert via
    // mockResolve and the persisting challengeToken.
    await waitFor(() => {
      // Wait for the fetch to complete by polling on the absence of resolve
      // call — the modal handles error in setError state, no completeChallenge
      // fired.
      expect(mockFetch).toHaveBeenCalled();
    });
    expect(mockResolve).not.toHaveBeenCalled();
    expect(useMFAChallengeStore.getState().challengeToken).toBe('test-token');
  });

  it('WebAuthn: cancel resolves with { verified: false }', () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'test-token',
      methods: ['webauthn'],
      recoveryOnlyMethods: [],
      webauthnOptions: mockWebAuthnOptions,
      resolve: mockResolve,
    });

    render(<MFAChallengeModal />);
    // Click the modal's Cancel button (clearChallenge), not the WebAuthn
    // prompt's internal cancel which triggers onCancel for the prompt.
    fireEvent.click(screen.getByText('Cancel'));

    expect(mockResolve).toHaveBeenCalledWith({ verified: false });
    expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
  });

  // ── Cancel purpose gate (#2394) ──────────────────────────────────────────
  // Only the 'sso_login' purpose holds a main-process SSO credential
  // reservation. The mid-session purposes run behind a published credential,
  // where the release is already a structural no-op; the gate makes that
  // explicit instead of relying on it. Both paths must still clear the
  // challenge — the release is hygiene, never a precondition.

  it('Cancel on an sso_login challenge abandons the SSO reservation (#2394)', () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'sso-chal',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      purpose: 'sso_login',
      ssoContext: { provider: 'google', credentialOwner: 5 },
      resolve: mockResolve,
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(abandonSSOReservation).toHaveBeenCalledTimes(1);
    expect(mockResolve).toHaveBeenCalledWith({ verified: false });
    expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
  });

  it('Cancel on a suspicious_refresh challenge does NOT abandon (#2394)', () => {
    const mockResolve = vi.fn();
    useMFAChallengeStore.setState({
      challengeToken: 'refresh-chal',
      methods: ['totp'],
      recoveryOnlyMethods: [],
      purpose: 'suspicious_refresh',
      ssoContext: null,
      resolve: mockResolve,
    });

    render(<MFAChallengeModal />);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));

    expect(abandonSSOReservation).not.toHaveBeenCalled();
    expect(mockResolve).toHaveBeenCalledWith({ verified: false });
    expect(useMFAChallengeStore.getState().challengeToken).toBeNull();
  });

  // ── Native dialog dismissal (#3423) ──────────────────────────────────────
  // The challenge dialog is a native <dialog> opened with showModal() (see
  // [internal]rules/frontend.md § "A global overlay must be reachable over the
  // Settings dialog", rule 4). Chromium delivers an UNCANCELABLE `cancel` on
  // the first Escape for a script-opened dialog with no user activation, then
  // closes the dialog regardless of preventDefault() — so `onClose` is the
  // deadlock guard: a dialog closed natively while the store still holds a
  // challenge token would otherwise leave the pending request waiting forever
  // on an invisible challenge.
  describe('native dialog dismissal (#3423)', () => {
    function queryDialog(): HTMLDialogElement | null {
      return document.querySelector('dialog.mfa-challenge-dialog');
    }

    it('Escape (native cancel) clears an idle challenge, unmounts the dialog, and resolves not-verified', async () => {
      render(<MFAChallengeModal />);

      let challengePromise!: Promise<MFAChallengeResult>;
      act(() => {
        challengePromise = useMFAChallengeStore
          .getState()
          .showChallenge('idle-token', ['totp'], 'suspicious_refresh', []);
      });

      const dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be mounted before Escape is tested'
      ).not.toBeNull();

      const cancelEvent = new Event('cancel', { cancelable: true });
      fireEvent(dialog as HTMLDialogElement, cancelEvent);

      expect(cancelEvent.defaultPrevented, 'onCancel must call e.preventDefault() on Escape').toBe(
        true
      );
      expect(
        useMFAChallengeStore.getState().challengeToken,
        'challengeToken must clear when Escape is pressed on an idle challenge'
      ).toBeNull();
      expect(
        queryDialog(),
        'the dialog must unmount once the challenge clears'
      ).not.toBeInTheDocument();
      await expect(
        challengePromise,
        'the challenge promise must resolve not-verified after an Escape-cancel'
      ).resolves.toEqual({ verified: false });
    });

    it('Escape (native cancel) is ignored while a verification proof is in flight', async () => {
      useMFAChallengeStore.setState({
        challengeToken: 'inflight-token',
        methods: ['totp'],
        recoveryOnlyMethods: [],
        purpose: 'suspicious_refresh',
      });
      mockFetch.mockReturnValueOnce(new Promise(() => {}));

      render(<MFAChallengeModal />);
      fireEvent.click(screen.getByTestId('totp-submit'));

      await waitFor(() =>
        expect(
          screen.getByTestId('totp-submit'),
          'submit must be disabled once a verification proof is in flight'
        ).toBeDisabled()
      );

      const dialog = queryDialog();
      expect(dialog, 'the dialog must be mounted while loading').not.toBeNull();

      const cancelEvent = new Event('cancel', { cancelable: true });
      fireEvent(dialog as HTMLDialogElement, cancelEvent);

      expect(
        cancelEvent.defaultPrevented,
        'onCancel must still call e.preventDefault() while loading, to keep the dialog open'
      ).toBe(true);
      expect(
        useMFAChallengeStore.getState().challengeToken,
        'challengeToken must NOT clear while a verification proof is in flight'
      ).toBe('inflight-token');
      expect(
        queryDialog(),
        'the dialog must stay mounted while a verification proof is in flight'
      ).toBeInTheDocument();
    });

    it('a forced close with no preceding cancel clears an idle challenge and resolves not-verified (the deadlock guard)', async () => {
      render(<MFAChallengeModal />);

      let challengePromise!: Promise<MFAChallengeResult>;
      act(() => {
        challengePromise = useMFAChallengeStore
          .getState()
          .showChallenge('idle-token-2', ['totp'], 'suspicious_refresh', []);
      });

      const dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be mounted before the forced close is tested'
      ).not.toBeNull();

      fireNativeClose(dialog as HTMLDialogElement);

      expect(
        useMFAChallengeStore.getState().challengeToken,
        'challengeToken must clear on a forced native close with no preceding cancel'
      ).toBeNull();
      await expect(
        challengePromise,
        'the challenge promise must resolve not-verified after a forced close'
      ).resolves.toEqual({ verified: false });
    });

    // Without user activation Escape's cancel is uncancelable, so the dialog
    // closes despite preventDefault (Codex review, PR #3423).
    it('an uncancelable Escape while a verification proof is in flight re-shows the dialog and lets the proof settle the challenge', async () => {
      render(<MFAChallengeModal />);
      let challengePromise!: Promise<MFAChallengeResult>;
      act(() => {
        challengePromise = useMFAChallengeStore
          .getState()
          .showChallenge('inflight-token-2', ['totp'], 'suspicious_refresh', []);
      });
      let settleProof!: (response: unknown) => void;
      mockFetch.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            settleProof = resolve;
          })
      );
      fireEvent.click(screen.getByTestId('totp-submit'));
      // Gate on the request, not the DOM: the button disables before the fetch.
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

      const dialog = queryDialog() as HTMLDialogElement;
      fireEvent(dialog, new Event('cancel', { cancelable: false }));
      fireNativeClose(dialog);

      expect(dialog.open, 'the dialog must be shown again for the proof in flight').toBe(true);
      expect(
        useMFAChallengeStore.getState().challengeToken,
        'a close must not cancel a challenge whose proof is in flight'
      ).toBe('inflight-token-2');

      settleProof({ ok: true, json: async () => ({}) });
      await expect(
        challengePromise,
        'the proof, not the close, settles the waiting request'
      ).resolves.toEqual({ verified: true, payload: {} });
    });

    it('Escape (native cancel) abandons the SSO reservation for an sso_login challenge', () => {
      render(<MFAChallengeModal />);
      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('sso-cancel-token', ['totp'], 'sso_login', [], {
            provider: 'google',
            credentialOwner: 5,
          });
      });

      const dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be mounted for the sso_login challenge'
      ).not.toBeNull();

      fireEvent(dialog as HTMLDialogElement, new Event('cancel', { cancelable: true }));

      expect(
        abandonSSOReservation,
        'abandonSSOReservation must be called exactly once for an sso_login Escape-cancel'
      ).toHaveBeenCalledTimes(1);
    });

    it('a forced close abandons the SSO reservation for an sso_login challenge', () => {
      render(<MFAChallengeModal />);
      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('sso-close-token', ['totp'], 'sso_login', [], {
            provider: 'apple',
            credentialOwner: 8,
          });
      });

      const dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be mounted for the sso_login challenge'
      ).not.toBeNull();

      fireNativeClose(dialog as HTMLDialogElement);

      expect(
        abandonSSOReservation,
        'abandonSSOReservation must be called exactly once for an sso_login forced close'
      ).toHaveBeenCalledTimes(1);
    });

    it('does NOT abandon the SSO reservation for a suspicious_refresh challenge on cancel or forced close', () => {
      render(<MFAChallengeModal />);

      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('refresh-cancel-token', ['totp'], 'suspicious_refresh', []);
      });
      let dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be mounted for the first challenge'
      ).not.toBeNull();
      fireEvent(dialog as HTMLDialogElement, new Event('cancel', { cancelable: true }));
      expect(
        abandonSSOReservation,
        'abandonSSOReservation must not be called for a suspicious_refresh Escape-cancel'
      ).not.toHaveBeenCalled();

      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('refresh-close-token', ['totp'], 'suspicious_refresh', []);
      });
      dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be re-mounted for the second challenge'
      ).not.toBeNull();
      fireNativeClose(dialog as HTMLDialogElement);
      expect(
        abandonSSOReservation,
        'abandonSSOReservation must not be called for a suspicious_refresh forced close'
      ).not.toHaveBeenCalled();
    });

    it('re-shows the dialog for a replacement challenge and ignores the close queued for its predecessor', async () => {
      render(<MFAChallengeModal />);
      act(() => {
        void useMFAChallengeStore.getState().showChallenge('tok-A', ['totp'], 'suspicious_refresh');
      });
      const dialog = queryDialog() as HTMLDialogElement;
      // Chromium closed the dialog (uncancelable Escape while A's proof was in
      // flight, so A stayed live) and queued its close event.
      dialog.removeAttribute('open');

      let bResult: unknown;
      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('tok-B', ['totp'], 'suspicious_refresh')
          .then((result) => {
            bResult = result;
          });
      });
      expect(queryDialog()).toBe(dialog);
      expect(dialog.open, 'the replacement challenge must be on screen').toBe(true);

      fireEvent(dialog, new Event('close'));
      await act(async () => {
        await Promise.resolve();
      });
      expect(useMFAChallengeStore.getState().challengeToken).toBe('tok-B');
      expect(bResult, 'a close that belonged to A must not settle B').toBeUndefined();
    });

    it('a trailing close event after an Escape-cancel does not abandon the SSO reservation a second time', () => {
      render(<MFAChallengeModal />);
      act(() => {
        void useMFAChallengeStore
          .getState()
          .showChallenge('sso-double-cancel', ['totp'], 'sso_login', [], {
            provider: 'google',
            credentialOwner: 5,
          });
      });

      const dialog = queryDialog();
      expect(
        dialog,
        'the mfa-challenge-dialog must be mounted before the race is tested'
      ).not.toBeNull();

      // With no user activation, Escape's `cancel` is uncancelable, so the
      // dialog closes and a `close` event follows. Both run inside ONE act():
      // fireNativeClose's own fireEvent act() nests in it, so React does not
      // flush the unmount between them, and `onClose` sees the store update
      // `onCancel` already made.
      act(() => {
        (dialog as HTMLDialogElement).dispatchEvent(new Event('cancel', { cancelable: false }));
        fireNativeClose(dialog as HTMLDialogElement);
      });

      expect(
        useMFAChallengeStore.getState().challengeToken,
        'challengeToken must be cleared after the cancel+close pair'
      ).toBeNull();
      expect(
        abandonSSOReservation,
        'abandonSSOReservation must be called exactly once, not once per native event'
      ).toHaveBeenCalledTimes(1);
    });
  });

  // ── A new challenge starts clean (#3423) ────────────────────────────────
  // The modal stays mounted between challenges, so anything scoped to one
  // challenge must be reset or fenced when the next one arrives.
  describe('a new challenge starts clean (#3423)', () => {
    function show(token: string) {
      act(() => {
        void useMFAChallengeStore.getState().showChallenge(token, ['totp'], 'suspicious_refresh');
      });
    }

    async function flush() {
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 0));
      });
    }

    // B supersedes A (showChallenge settles A not-verified). A close cannot
    // stand in: it keeps a challenge whose proof is in flight. 'before' is
    // the ordering the state reset alone handles; 'after' is the one only the
    // fence handles, because A's result arrives while B is live.
    it.each(['before', 'after'] as const)(
      "does not inherit an earlier proof's spinner or error when that proof settles %s the next challenge opens",
      async (when) => {
        render(<MFAChallengeModal />);
        show('tok-A');
        let settleA!: (response: unknown) => void;
        mockFetch.mockImplementationOnce(
          () =>
            new Promise((resolve) => {
              settleA = resolve;
            })
        );
        fireEvent.click(screen.getByTestId('totp-submit'));
        await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));
        const lateResponse = { ok: false, json: async () => ({ error: 'error-from-A' }) };

        if (when === 'before') {
          settleA(lateResponse);
          await flush();
        }
        show('tok-B');
        if (when === 'after') {
          // While A is still in flight, B must already be answerable.
          expect(screen.getByRole('button', { name: 'Cancel' })).not.toBeDisabled();
          settleA(lateResponse);
        }
        await flush();

        expect(screen.getByTestId('totp-submit'), 'B has no proof in flight').not.toBeDisabled();
        expect(screen.getByRole('button', { name: 'Cancel' })).not.toBeDisabled();
        expect(screen.queryByTestId('totp-error'), "A's error must not appear on B").toBeNull();
      }
    );

    it("does not clear the next challenge's own in-flight spinner when a superseded proof settles late", async () => {
      render(<MFAChallengeModal />);
      show('tok-A');
      let settleA!: (response: unknown) => void;
      mockFetch.mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            settleA = resolve;
          })
      );
      fireEvent.click(screen.getByTestId('totp-submit'));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(1));

      show('tok-B');
      mockFetch.mockImplementationOnce(() => new Promise(() => {}));
      fireEvent.click(screen.getByTestId('totp-submit'));
      await waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));

      settleA({ ok: false, json: async () => ({ error: 'error-from-A' }) });
      await flush();
      expect(screen.getByTestId('totp-submit'), "B's own proof is still in flight").toBeDisabled();
      expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
    });

    // The catch branch is fenced too: a proof that REJECTS after B replaced A
    // must not put A's error or a cleared spinner on B.
    it.each(['verify fetch', 'SSO completion'] as const)(
      'ignores a superseded proof whose %s rejects',
      async (path) => {
        render(<MFAChallengeModal />);
        let rejectA!: (reason: unknown) => void;
        const pendingA = new Promise<never>((_, reject) => {
          rejectA = reject;
        });
        if (path === 'verify fetch') {
          show('tok-A');
          mockFetch.mockImplementationOnce(() => pendingA);
        } else {
          act(() => {
            void useMFAChallengeStore.getState().showChallenge('tok-A', ['totp'], 'sso_login', [], {
              provider: 'google',
              credentialOwner: 5,
            });
          });
          vi.mocked(completeSSOMFA).mockImplementationOnce(() => pendingA);
        }
        fireEvent.click(screen.getByTestId('totp-submit'));
        await waitFor(() =>
          expect(path === 'verify fetch' ? mockFetch : completeSSOMFA).toHaveBeenCalledTimes(1)
        );

        show('tok-B');
        mockFetch.mockImplementationOnce(() => new Promise(() => {}));
        fireEvent.click(screen.getByTestId('totp-submit'));
        await waitFor(() =>
          expect(mockFetch).toHaveBeenCalledTimes(path === 'verify fetch' ? 2 : 1)
        );

        rejectA(new Error('error-from-A'));
        await flush();
        expect(screen.queryByTestId('totp-error'), "A's error must not appear on B").toBeNull();
        expect(
          screen.getByTestId('totp-submit'),
          "B's own proof is still in flight"
        ).toBeDisabled();
      }
    );

    // A replacement with no methods keeps the old mode, but must still drop
    // the old error and spinner.
    it.each(['error', 'spinner'] as const)(
      "drops the earlier challenge's %s when the next one has no methods",
      async (leftover) => {
        render(<MFAChallengeModal />);
        show('tok-A');
        if (leftover === 'error') {
          mockFetch.mockResolvedValueOnce({
            ok: false,
            json: async () => ({ error: 'error-from-A' }),
          });
        } else {
          mockFetch.mockImplementationOnce(() => new Promise(() => {}));
        }
        fireEvent.click(screen.getByTestId('totp-submit'));
        if (leftover === 'error') {
          await screen.findByTestId('totp-error');
        } else {
          await waitFor(() => expect(screen.getByTestId('totp-submit')).toBeDisabled());
        }

        act(() => {
          void useMFAChallengeStore.getState().showChallenge('tok-B', [], 'suspicious_refresh');
        });

        expect(screen.queryByTestId('totp-error')).toBeNull();
        expect(screen.getByTestId('totp-submit')).not.toBeDisabled();
      }
    );

    it('a verify response that stalls after its headers still reports the timeout', async () => {
      // The abort fires mid-body, so the error is the JSON read's, not the fetch's.
      vi.spyOn(AbortSignal, 'timeout').mockReturnValueOnce(
        AbortSignal.abort(new DOMException('The operation timed out.', 'TimeoutError'))
      );
      render(<MFAChallengeModal />);
      show('tok-S');
      mockFetch.mockResolvedValueOnce({
        ok: true,
        json: async () => {
          throw new DOMException('The operation was aborted.', 'AbortError');
        },
      });
      fireEvent.click(screen.getByTestId('totp-submit'));

      expect(await screen.findByTestId('totp-error')).toHaveTextContent(
        'Verification timed out. Please try again.'
      );
      vi.restoreAllMocks();
    });

    it('a verify request that times out says so and gives Cancel back', async () => {
      const timeout = vi.spyOn(AbortSignal, 'timeout');
      render(<MFAChallengeModal />);
      show('tok-T');
      mockFetch.mockRejectedValueOnce(new DOMException('The operation timed out.', 'TimeoutError'));
      fireEvent.click(screen.getByTestId('totp-submit'));

      expect(await screen.findByTestId('totp-error')).toHaveTextContent(
        'Verification timed out. Please try again.'
      );
      expect(screen.getByRole('button', { name: 'Cancel' })).not.toBeDisabled();
      const init = mockFetch.mock.calls[0][1] as RequestInit;
      expect(timeout).toHaveBeenCalledWith(30_000);
      expect(init.signal, 'the request carries that 30 s signal').toBe(
        timeout.mock.results[0].value
      );
      timeout.mockRestore();
    });
  });
});
