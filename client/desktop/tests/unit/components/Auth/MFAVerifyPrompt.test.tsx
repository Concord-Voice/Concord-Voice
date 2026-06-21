import { render, screen, fireEvent } from '../../../test-utils';
import { vi } from 'vitest';

// Mock apiFetch for WebAuthn flows
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  API_BASE: 'http://localhost:8080',
}));

import MFAVerifyPrompt from '@/renderer/components/Auth/MFAVerifyPrompt';

describe('MFAVerifyPrompt', () => {
  const onVerify = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  // ── Rendering ──────────────────────────────────────────────────────────

  it('renders MFA Verification label', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} />);
    expect(screen.getByText('MFA Verification')).toBeInTheDocument();
  });

  it('shows TOTP input when totp is the default method', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} />);
    // Should show 6 digit inputs for TOTP
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
  });

  it('shows WebAuthn button when webauthn is the default method', () => {
    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    expect(screen.getByText('Verify with security key')).toBeInTheDocument();
  });

  it('shows switch links for other available methods', () => {
    render(<MFAVerifyPrompt methods={['totp', 'webauthn']} onVerify={onVerify} />);
    // Default should be webauthn (highest priority), so we should see "Use authenticator app"
    expect(screen.getByText('Use authenticator app instead')).toBeInTheDocument();
  });

  // ── Mode Switching ─────────────────────────────────────────────────────

  it('switches from webauthn to totp mode', () => {
    render(<MFAVerifyPrompt methods={['totp', 'webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Use authenticator app instead'));
    // Now should show TOTP digit inputs
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
  });

  it('switches from totp to backup code mode', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Use a backup code instead'));
    expect(screen.getByPlaceholderText('XXXXXXXX')).toBeInTheDocument();
  });

  it('switches from backup back to totp mode', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Use a backup code instead'));
    expect(screen.getByPlaceholderText('XXXXXXXX')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Use authenticator app instead'));
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
  });

  it('shows webauthn switch from totp mode', () => {
    render(<MFAVerifyPrompt methods={['totp', 'webauthn']} onVerify={onVerify} />);
    // Default is webauthn; switch to totp first
    fireEvent.click(screen.getByText('Use authenticator app instead'));
    // Now should see option to switch back to webauthn
    expect(screen.getByText('Use a security key instead')).toBeInTheDocument();
  });

  it('switches from backup to webauthn when available', () => {
    render(<MFAVerifyPrompt methods={['totp', 'webauthn']} onVerify={onVerify} />);
    // Start on webauthn, go to totp, then backup
    fireEvent.click(screen.getByText('Use authenticator app instead'));
    fireEvent.click(screen.getByText('Use a backup code instead'));
    // Should see webauthn link from backup mode
    expect(screen.getByText('Use a security key instead')).toBeInTheDocument();
  });

  // ── Exclude Options ────────────────────────────────────────────────────

  it('hides backup code option when excludeBackupCodes is true', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} excludeBackupCodes />);
    expect(screen.queryByText('Use a backup code instead')).not.toBeInTheDocument();
  });

  it('excludes recovery-only methods', () => {
    render(
      <MFAVerifyPrompt
        methods={['totp', 'email']}
        recoveryOnlyMethods={['email']}
        onVerify={onVerify}
      />
    );
    // Should not show email/sms switch
    expect(screen.queryByText(/email/i)).not.toBeInTheDocument();
  });

  it('does not show backup switch when excludeBackupCodes and in webauthn mode', () => {
    render(
      <MFAVerifyPrompt methods={['webauthn', 'totp']} onVerify={onVerify} excludeBackupCodes />
    );
    expect(screen.queryByText('Use a backup code instead')).not.toBeInTheDocument();
  });

  // ── Code Submission ────────────────────────────────────────────────────

  it('calls onVerify when TOTP code is entered', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} />);
    for (let i = 1; i <= 6; i++) {
      fireEvent.change(screen.getByLabelText(`Digit ${i}`), {
        target: { value: String(i) },
      });
    }
    expect(onVerify).toHaveBeenCalledWith('123456');
  });

  it('calls onVerify when backup code is submitted', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Use a backup code instead'));
    const input = screen.getByPlaceholderText('XXXXXXXX');
    fireEvent.change(input, {
      target: { value: 'ABCD1234' },
    });
    fireEvent.submit(input.closest('form')!);
    expect(onVerify).toHaveBeenCalledWith('ABCD1234');
  });

  // ── Error & Disabled States ────────────────────────────────────────────

  it('shows error message when provided', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} error="Invalid code" />);
    expect(screen.getByText('Invalid code')).toBeInTheDocument();
  });

  it('disables inputs when disabled prop is true', () => {
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} disabled />);
    const inputs = screen.getAllByRole('textbox');
    for (const input of inputs) {
      expect(input).toBeDisabled();
    }
  });

  it('disables webauthn button when disabled', () => {
    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} disabled />);
    expect(screen.getByText('Verify with security key')).toBeDisabled();
  });

  it('disables switch links when disabled', () => {
    render(<MFAVerifyPrompt methods={['totp', 'webauthn']} onVerify={onVerify} disabled />);
    expect(screen.getByText('Use authenticator app instead')).toBeDisabled();
  });

  // ── WebAuthn Inline Flow ───────────────────────────────────────────────

  it('shows "Verify with security key" button in idle state', () => {
    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    expect(screen.getByText('Verify with security key')).toBeInTheDocument();
  });

  it('shows waiting state during WebAuthn ceremony', async () => {
    // Mock the begin endpoint to return valid options
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdC1jaGFsbGVuZ2U',
          rpId: 'localhost',
          allowCredentials: [],
        },
      }),
    });

    // Mock navigator.credentials.get to never resolve (simulates waiting for key)
    Object.defineProperty(navigator, 'credentials', {
      value: {
        get: vi.fn().mockReturnValue(new Promise(() => {})),
        create: vi.fn(),
      },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    // Should transition to waiting state
    await vi.waitFor(() => {
      expect(screen.getByText(/Touch your security key/)).toBeInTheDocument();
    });
  });

  it('shows error and retry when WebAuthn begin fails', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'No credentials registered' }),
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(screen.getByText('No credentials registered')).toBeInTheDocument();
      expect(screen.getByText('Try again')).toBeInTheDocument();
    });
  });

  // ── WebAuthn Full Success Flow ──────────────────────────────────────────

  it('completes WebAuthn verification and calls onVerify with mfa_token', async () => {
    // Step 1: begin endpoint returns challenge options
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdC1jaGFsbGVuZ2U',
          rpId: 'localhost',
          allowCredentials: [],
        },
        challengeToken: 'test-challenge-token',
      }),
    });

    // Step 2: finish endpoint returns MFA token
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ mfa_token: 'webauthn-mfa-token-123' }),
    });

    // Mock navigator.credentials.get to return a credential
    const mockCredential = {
      id: 'credential-id',
      rawId: new Uint8Array([1, 2, 3]).buffer,
      type: 'public-key',
      response: {
        authenticatorData: new Uint8Array([10, 20]).buffer,
        clientDataJSON: new Uint8Array([30, 40]).buffer,
        signature: new Uint8Array([50, 60]).buffer,
        userHandle: new Uint8Array([70, 80]).buffer,
      },
    };

    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockResolvedValue(mockCredential), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(onVerify).toHaveBeenCalledWith('webauthn-mfa-token-123');
    });
  });

  it('shows error when navigator.credentials.get returns null', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdC1jaGFsbGVuZ2U',
          rpId: 'localhost',
          allowCredentials: [],
        },
      }),
    });

    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockResolvedValue(null), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(screen.getByText('No credential returned')).toBeInTheDocument();
      expect(screen.getByText('Try again')).toBeInTheDocument();
    });
  });

  it('shows "Cancelled or timed out" on NotAllowedError', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdC1jaGFsbGVuZ2U',
          rpId: 'localhost',
          allowCredentials: [],
        },
      }),
    });

    const notAllowed = new DOMException('User denied', 'NotAllowedError');
    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockRejectedValue(notAllowed), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(screen.getByText('Cancelled or timed out. Try again.')).toBeInTheDocument();
    });
  });

  it('silently resets to idle on AbortError', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdC1jaGFsbGVuZ2U',
          rpId: 'localhost',
          allowCredentials: [],
        },
      }),
    });

    const abortError = new DOMException('Aborted', 'AbortError');
    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockRejectedValue(abortError), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      // Should go back to idle state (show the verify button again)
      expect(screen.getByText('Verify with security key')).toBeInTheDocument();
    });
    expect(onVerify).not.toHaveBeenCalled();
  });

  it('shows finish endpoint error message', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdC1jaGFsbGVuZ2U',
          rpId: 'localhost',
          allowCredentials: [],
        },
        challengeToken: 'tok',
      }),
    });
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Credential not recognized' }),
    });

    const mockCredential = {
      id: 'cred-id',
      rawId: new Uint8Array([1]).buffer,
      type: 'public-key',
      response: {
        authenticatorData: new Uint8Array([2]).buffer,
        clientDataJSON: new Uint8Array([3]).buffer,
        signature: new Uint8Array([4]).buffer,
        userHandle: null,
      },
    };
    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockResolvedValue(mockCredential), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(screen.getByText('Credential not recognized')).toBeInTheDocument();
    });
  });

  it('handles allowCredentials with base64url ids', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdA',
          rpId: 'localhost',
          allowCredentials: [{ id: 'AQID', type: 'public-key' }],
        },
        challengeToken: 'tok',
      }),
    });
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ mfa_token: 'token-with-creds' }),
    });

    const mockCredential = {
      id: 'cred',
      rawId: new Uint8Array([1]).buffer,
      type: 'public-key',
      response: {
        authenticatorData: new Uint8Array([2]).buffer,
        clientDataJSON: new Uint8Array([3]).buffer,
        signature: new Uint8Array([4]).buffer,
        userHandle: null,
      },
    };
    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockResolvedValue(mockCredential), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(onVerify).toHaveBeenCalledWith('token-with-creds');
    });
  });

  it('shows fallback error for non-Error thrown values', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdA',
          rpId: 'localhost',
          allowCredentials: [],
        },
      }),
    });

    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockRejectedValue('string-error'), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(screen.getByText('Verification failed')).toBeInTheDocument();
    });
  });

  it('can retry after WebAuthn error', async () => {
    // First attempt fails
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      json: async () => ({ error: 'Server error' }),
    });

    render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(screen.getByText('Try again')).toBeInTheDocument();
    });

    // Second attempt succeeds
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: { challenge: 'dGVzdA', rpId: 'localhost', allowCredentials: [] },
        challengeToken: 'tok2',
      }),
    });
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({ mfa_token: 'retry-token' }),
    });

    const mockCred = {
      id: 'c',
      rawId: new Uint8Array([1]).buffer,
      type: 'public-key',
      response: {
        authenticatorData: new Uint8Array([2]).buffer,
        clientDataJSON: new Uint8Array([3]).buffer,
        signature: new Uint8Array([4]).buffer,
        userHandle: null,
      },
    };
    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockResolvedValue(mockCred), create: vi.fn() },
      writable: true,
      configurable: true,
    });

    fireEvent.click(screen.getByText('Try again'));

    await vi.waitFor(() => {
      expect(onVerify).toHaveBeenCalledWith('retry-token');
    });
  });

  // ── Email/SMS Mode ─────────────────────────────────────────────────────

  it('shows TOTP-style input for email-sms mode', () => {
    render(<MFAVerifyPrompt methods={['email']} onVerify={onVerify} />);
    // email maps to email-sms category, which shows TOTP input
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
  });

  it('shows email-sms switch link from totp mode when email is available', () => {
    render(<MFAVerifyPrompt methods={['totp', 'email']} onVerify={onVerify} />);
    // Default is totp (higher priority than email-sms)
    // Should see email/sms switch link
    expect(screen.getByText('Use email/SMS code instead')).toBeInTheDocument();
  });

  it('switches from totp to email-sms mode', () => {
    render(<MFAVerifyPrompt methods={['totp', 'email']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Use email/SMS code instead'));
    // email-sms mode also shows TOTP-style 6-digit input
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
    // Should now see link to switch back to authenticator app
    expect(screen.getByText('Use authenticator app instead')).toBeInTheDocument();
  });

  // ── Edge Cases ─────────────────────────────────────────────────────────

  it('handles empty methods array gracefully', () => {
    render(<MFAVerifyPrompt methods={[]} onVerify={onVerify} />);
    // Default method falls back to 'totp', should render TOTP inputs
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
  });

  it('defaults to first available when excludeBackupCodes removes the default', () => {
    // If only backup would be available (which shouldn't normally happen),
    // the component handles it by falling through
    render(<MFAVerifyPrompt methods={['totp']} onVerify={onVerify} excludeBackupCodes />);
    // Should still show TOTP
    const inputs = screen.getAllByRole('textbox');
    expect(inputs.length).toBe(6);
  });

  it('shows all three method links when all methods available', () => {
    render(<MFAVerifyPrompt methods={['totp', 'webauthn', 'email']} onVerify={onVerify} />);
    // Default is webauthn (highest priority)
    expect(screen.getByText('Use authenticator app instead')).toBeInTheDocument();
    expect(screen.getByText('Use a backup code instead')).toBeInTheDocument();
    expect(screen.getByText('Use email/SMS code instead')).toBeInTheDocument();
  });

  it('unmount aborts pending WebAuthn ceremony', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        publicKey: {
          challenge: 'dGVzdA',
          rpId: 'localhost',
          allowCredentials: [],
        },
      }),
    });

    const mockGet = vi.fn().mockReturnValue(new Promise(() => {}));
    Object.defineProperty(navigator, 'credentials', {
      value: { get: mockGet, create: vi.fn() },
      writable: true,
      configurable: true,
    });

    // Spy on AbortController.prototype.abort
    const abortSpy = vi.spyOn(AbortController.prototype, 'abort');

    const { unmount } = render(<MFAVerifyPrompt methods={['webauthn']} onVerify={onVerify} />);
    fireEvent.click(screen.getByText('Verify with security key'));

    await vi.waitFor(() => {
      expect(mockGet).toHaveBeenCalled();
    });

    unmount();
    expect(abortSpy).toHaveBeenCalled();

    abortSpy.mockRestore();
  });
});
