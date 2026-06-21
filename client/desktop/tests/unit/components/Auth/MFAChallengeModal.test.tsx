import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { vi } from 'vitest';
import { useMFAChallengeStore } from '@/renderer/stores/mfaChallengeStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock global fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// safeJson is mocked as a transparent passthrough so existing fetch mocks
// (which only set ok / json / status) continue to work without supplying
// Content-Type headers. The real safeJson is exercised by apiClient tests.
vi.mock('@/renderer/services/apiClient', () => ({
  API_BASE: 'http://localhost:8080',
  ensureMachineId: vi.fn().mockResolvedValue('mock-machine-id'),
  safeJson: async <T,>(res: { json: () => Promise<T> }): Promise<T> => res.json(),
}));

// Mock child components to isolate MFAChallengeModal behavior
vi.mock('@/renderer/components/Auth/TOTPInput', () => ({
  default: ({
    onSubmit,
    disabled,
    error,
  }: {
    onSubmit: (code: string) => void;
    disabled?: boolean;
    error?: string;
  }) => (
    <div data-testid="totp-input">
      <button data-testid="totp-submit" disabled={disabled} onClick={() => onSubmit('123456')}>
        Submit TOTP
      </button>
      {error && <span data-testid="totp-error">{error}</span>}
    </div>
  ),
}));

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
    const { container } = render(<MFAChallengeModal />);
    expect(container.innerHTML).toBe('');
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
});
