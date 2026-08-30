import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import EmailVerification from '@/renderer/components/Auth/EmailVerification';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { usePendingRegistrationStore } from '@/renderer/stores/auth/pendingRegistrationStore';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock fetch
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

// Keep aborted-session cleanup observable without issuing a second network request.
vi.mock('@/renderer/services/system/apiClient', () => ({
  revokeAbortedSession: vi.fn().mockResolvedValue(undefined),
}));

const MOCK_PENDING_ID = 'test-pending-id';
const MOCK_EMAIL = 'test@example.com';
const MOCK_CONFIRM_SUCCESS = {
  access_token: 'origin-a-access',
  refresh_token: 'origin-a-refresh',
  session_id: 'origin-a-session',
};

function mockSuccessfulConfirmation() {
  mockFetch.mockResolvedValueOnce({
    ok: true,
    json: async () => MOCK_CONFIRM_SUCCESS,
  });
}

function seedPendingStore() {
  usePendingRegistrationStore.getState().setPending({
    pending_id: MOCK_PENDING_ID,
    email: MOCK_EMAIL,
    expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
    code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
  });
}

describe('EmailVerification', () => {
  const mockOnSuccess = vi.fn();
  const mockOnChangeEmail = vi.fn();
  const mockOnCancel = vi.fn();
  const originalElectron = globalThis.electron;

  beforeEach(() => {
    vi.clearAllMocks();
    resetAllStores();
    resetRuntimeServerBase();
    seedPendingStore();
  });

  afterEach(() => {
    resetRuntimeServerBase();
    Object.defineProperty(globalThis, 'electron', {
      value: originalElectron,
      writable: true,
    });
  });

  function submitVerificationCode() {
    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: String(i + 1) } });
    }
  }

  // ── Rendering ──────────────────────────────────────────────────────────

  it('renders with masked email by default', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Verify your email')).toBeInTheDocument();
    expect(screen.getByText(/t\*+t@example\.com/)).toBeInTheDocument();
  });

  it('renders with unmasked email when maskEmail=false', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} maskEmail={false} />);
    expect(screen.getByText(MOCK_EMAIL)).toBeInTheDocument();
  });

  it('renders 6 digit input fields', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    expect(inputs).toHaveLength(6);
  });

  it('shows change-email button when onChangeEmail prop provided', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} onChangeEmail={mockOnChangeEmail} />);
    expect(screen.getByText('Change email')).toBeInTheDocument();
  });

  it('does not show change-email button when onChangeEmail not provided', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} />);
    expect(screen.queryByText('Change email')).not.toBeInTheDocument();
  });

  it('calls onChangeEmail when change email button clicked', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} onChangeEmail={mockOnChangeEmail} />);
    fireEvent.click(screen.getByText('Change email'));
    expect(mockOnChangeEmail).toHaveBeenCalledOnce();
  });

  // ── Code Verification ──────────────────────────────────────────────────

  it('submits code to confirm endpoint and calls onSuccess', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          access_token: 'new-access-token',
          refresh_token: 'new-refresh-token',
          session_id: 'sess-1',
        }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);

    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: String(i + 1) } });
    }

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        'http://localhost:8080/api/v1/auth/register/confirm',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ pending_id: MOCK_PENDING_ID, code: '123456' }),
        })
      );
    });

    await waitFor(() => expect(mockOnSuccess).toHaveBeenCalledOnce());
  });

  it('sets accessToken and emailVerified in auth store on success', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          access_token: 'verified-token',
          refresh_token: 'refresh',
          session_id: 'sess-1',
        }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);

    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: String(i + 1) } });
    }

    await waitFor(() => {
      expect(useAuthStore.getState().accessToken).toBe('verified-token');
      expect(useAuthStore.getState().emailVerified).toBe(true);
    });
  });

  it('keeps origin B credentials when B persists before A token storage resolves', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/system/apiClient');
    const requestApiBase = 'https://origin-a.example';
    const originAOwner = 101;
    const originBOwner = 202;
    setRuntimeServerBase(requestApiBase);

    let finishStore: (owner: number) => void = () => {
      throw new Error('Token-store resolver was not initialized.');
    };
    const storeRefreshToken = vi.fn().mockReturnValue(
      new Promise<number>((resolve) => {
        finishStore = resolve;
      })
    );
    let currentMainOwner = originAOwner;
    const clearTokensIfOwner = vi.fn().mockImplementation(async (owner: number) => {
      if (owner !== currentMainOwner) return false;
      currentMainOwner = 0;
      return true;
    });
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken, clearTokensIfOwner },
      writable: true,
    });
    mockSuccessfulConfirmation();

    submitVerificationCode();

    await waitFor(() => {
      expect(storeRefreshToken).toHaveBeenCalledWith({
        refreshToken: 'origin-a-refresh',
        rememberMe: true,
        apiBase: requestApiBase,
        accessToken: 'origin-a-access',
      });
    });
    expect(mockFetch).toHaveBeenCalledWith(
      `${requestApiBase}/api/v1/auth/register/confirm`,
      expect.any(Object)
    );

    // B wins main-process ownership but has not resumed from its IPC await to
    // publish renderer auth yet. A may attempt only an owner-scoped clear.
    setRuntimeServerBase('https://origin-b.example');
    currentMainOwner = originBOwner;
    finishStore(originAOwner);

    await waitFor(() => {
      expect(revokeAbortedSession).toHaveBeenCalledWith({
        accessToken: 'origin-a-access',
        refreshToken: 'origin-a-refresh',
        sessionId: 'origin-a-session',
        apiBase: requestApiBase,
      });
    });
    expect(clearTokensIfOwner).toHaveBeenCalledWith(originAOwner);
    expect(currentMainOwner).toBe(originBOwner);
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(mockOnSuccess).not.toHaveBeenCalled();
  });

  it('clears persisted origin A tokens by owner when the server switches', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/system/apiClient');
    const requestApiBase = 'https://origin-a.example';
    const originAOwner = 101;
    setRuntimeServerBase(requestApiBase);

    const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      setRuntimeServerBase('https://origin-b.example');
      return originAOwner;
    });
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken, clearTokensIfOwner },
      writable: true,
    });
    mockSuccessfulConfirmation();

    submitVerificationCode();

    await waitFor(() => {
      expect(revokeAbortedSession).toHaveBeenCalledWith({
        accessToken: 'origin-a-access',
        refreshToken: 'origin-a-refresh',
        sessionId: 'origin-a-session',
        apiBase: requestApiBase,
      });
    });
    expect(storeRefreshToken).toHaveBeenCalledWith(
      expect.objectContaining({ apiBase: requestApiBase })
    );
    expect(clearTokensIfOwner).toHaveBeenCalledWith(originAOwner);
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(mockOnSuccess).not.toHaveBeenCalled();
  });

  it('does not persist A after switching to B while the confirmation response is pending', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/system/apiClient');
    const requestApiBase = 'https://origin-a.example';
    setRuntimeServerBase(requestApiBase);

    let finishJson: (data: typeof MOCK_CONFIRM_SUCCESS) => void = () => {
      throw new Error('Response resolver was not initialized.');
    };
    const responseJson = vi.fn().mockReturnValue(
      new Promise<typeof MOCK_CONFIRM_SUCCESS>((resolve) => {
        finishJson = resolve;
      })
    );
    const storeRefreshToken = vi.fn().mockResolvedValue(101);
    const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken, clearTokensIfOwner },
      writable: true,
    });
    mockFetch.mockResolvedValueOnce({ ok: true, json: responseJson });

    submitVerificationCode();
    await waitFor(() => expect(responseJson).toHaveBeenCalled());

    setRuntimeServerBase('https://origin-b.example');
    useAuthStore.getState().beginAuthLifecycle('origin-b-access', 'origin-b-session');
    finishJson(MOCK_CONFIRM_SUCCESS);

    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    expect(mockFetch).toHaveBeenCalledWith(
      `${requestApiBase}/api/v1/auth/register/confirm`,
      expect.any(Object)
    );
    expect(storeRefreshToken).not.toHaveBeenCalled();
    expect(clearTokensIfOwner).not.toHaveBeenCalled();
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'origin-b-access',
      sessionId: 'origin-b-session',
    });
    expect(mockOnSuccess).not.toHaveBeenCalled();
  });

  it('rejects an A to B to A selection cycle while token persistence is pending', async () => {
    const { revokeAbortedSession } = await import('@/renderer/services/system/apiClient');
    const requestApiBase = 'https://origin-a.example';
    const originAOwner = 101;
    setRuntimeServerBase(requestApiBase);

    const clearTokensIfOwner = vi.fn().mockResolvedValue(true);
    const storeRefreshToken = vi.fn().mockImplementation(async () => {
      setRuntimeServerBase('https://origin-b.example');
      setRuntimeServerBase(requestApiBase);
      return originAOwner;
    });
    Object.defineProperty(globalThis, 'electron', {
      value: { ...globalThis.electron, storeRefreshToken, clearTokensIfOwner },
      writable: true,
    });
    mockSuccessfulConfirmation();

    submitVerificationCode();

    await waitFor(() => expect(revokeAbortedSession).toHaveBeenCalled());
    expect(clearTokensIfOwner).toHaveBeenCalledWith(originAOwner);
    expect(useAuthStore.getState().accessToken).toBeNull();
    expect(mockOnSuccess).not.toHaveBeenCalled();
  });

  it('clears pending store on success', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({ access_token: 'tok', refresh_token: 'ref', session_id: 'sess' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: String(i + 1) } });
    }

    await waitFor(() => {
      expect(usePendingRegistrationStore.getState().pendingId).toBeNull();
    });
  });

  it('shows error on invalid_code with attempts remaining', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 401,
      json: () => Promise.resolve({ code: 'invalid_code', attempts_remaining: 4 }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: '0' } });
    }

    await waitFor(() => {
      // Error text appears in both TOTPInput and the component's own error paragraph
      expect(screen.getAllByText('Incorrect code. 4 attempts remaining.')).toHaveLength(2);
    });
    expect(screen.getByText('4 attempts remaining')).toBeInTheDocument();
  });

  it('uses singular "attempt" for 1 remaining', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 401,
      json: () => Promise.resolve({ code: 'invalid_code', attempts_remaining: 1 }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: '0' } });
    }

    await waitFor(() => {
      expect(screen.getAllByText('Incorrect code. 1 attempt remaining.')).toHaveLength(2);
    });
  });

  it('shows code_expired error', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 410,
      json: () => Promise.resolve({ code: 'code_expired' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: '0' } });
    }

    await waitFor(() => {
      // Error text appears in TOTPInput and the component's own error paragraph
      expect(screen.getAllByText('Code expired — tap resend.')).toHaveLength(2);
    });
  });

  it('clears pending and calls onCancel on pending_expired', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 410,
      json: () => Promise.resolve({ code: 'pending_expired' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} onCancel={mockOnCancel} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: '0' } });
    }

    await waitFor(() => {
      expect(usePendingRegistrationStore.getState().pendingId).toBeNull();
      expect(mockOnCancel).toHaveBeenCalledOnce();
    });
  });

  // ── Resend ─────────────────────────────────────────────────────────────

  it('shows resend button enabled on mount', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const btn = screen.getByText('Send new code');
    expect(btn).not.toBeDisabled();
  });

  it('sends resend request with pending_id', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
          resends_remaining: 3,
        }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Send new code'));

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        'http://localhost:8080/api/v1/auth/register/resend',
        expect.objectContaining({
          method: 'POST',
          body: JSON.stringify({ pending_id: MOCK_PENDING_ID }),
        })
      );
    });
  });

  it('disables resend button and shows cooldown after resend', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: true,
      json: () =>
        Promise.resolve({
          code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
          resends_remaining: 3,
        }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Send new code'));

    await waitFor(() => {
      expect(screen.getByText(/Send new code \(\d+s\)/)).toBeInTheDocument();
    });
  });

  // ── Cancel ─────────────────────────────────────────────────────────────

  it('shows cancel button when onCancel provided', () => {
    render(<EmailVerification onSuccess={mockOnSuccess} onCancel={mockOnCancel} />);
    expect(screen.getByText('← Cancel registration')).toBeInTheDocument();
  });

  it('DELETEs pending registration and calls onCancel', async () => {
    // handleCancel only calls clearPending + onCancel on 204 or 404
    mockFetch.mockResolvedValueOnce({ status: 204 });

    render(<EmailVerification onSuccess={mockOnSuccess} onCancel={mockOnCancel} />);
    fireEvent.click(screen.getByText('← Cancel registration'));

    await waitFor(() => {
      expect(mockFetch).toHaveBeenCalledWith(
        `http://localhost:8080/api/v1/auth/register/${MOCK_PENDING_ID}`,
        expect.objectContaining({ method: 'DELETE' })
      );
      expect(mockOnCancel).toHaveBeenCalledOnce();
    });
  });

  it('clears pending store after cancel', async () => {
    // handleCancel only calls clearPending on 204 or 404
    mockFetch.mockResolvedValueOnce({ status: 204 });

    render(<EmailVerification onSuccess={mockOnSuccess} onCancel={mockOnCancel} />);
    fireEvent.click(screen.getByText('← Cancel registration'));

    await waitFor(() => {
      expect(usePendingRegistrationStore.getState().pendingId).toBeNull();
    });
  });

  it('shows error and keeps pending state when cancel returns unexpected status', async () => {
    mockFetch.mockResolvedValueOnce({ status: 500 });

    render(<EmailVerification onSuccess={mockOnSuccess} onCancel={mockOnCancel} />);
    fireEvent.click(screen.getByText('← Cancel registration'));

    await waitFor(() => {
      // queryAllByText because the error may appear in both the TOTP error
      // slot and the email-verification-error paragraph.
      expect(screen.queryAllByText(/Failed to cancel registration/).length).toBeGreaterThan(0);
    });
    expect(usePendingRegistrationStore.getState().pendingId).toBe(MOCK_PENDING_ID);
    expect(mockOnCancel).not.toHaveBeenCalled();
  });

  it('accepts 404 as terminal cancel state and clears pending', async () => {
    mockFetch.mockResolvedValueOnce({ status: 404 });

    render(<EmailVerification onSuccess={mockOnSuccess} onCancel={mockOnCancel} />);
    fireEvent.click(screen.getByText('← Cancel registration'));

    await waitFor(() => {
      expect(usePendingRegistrationStore.getState().pendingId).toBeNull();
    });
    expect(mockOnCancel).toHaveBeenCalledOnce();
  });

  // ── Resend error branches ───────────────────────────────────────────────

  it('shows cooldown and hides error on cooldown_active resend response', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 429,
      json: () => Promise.resolve({ code: 'cooldown_active', retry_after_seconds: 45 }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Send new code'));

    await waitFor(() => {
      // Button should show cooldown (45s) and no error message
      expect(screen.getByText(/Send new code \(45s\)/)).toBeInTheDocument();
    });
    expect(screen.queryByText(/failed/i)).not.toBeInTheDocument();
  });

  it('shows resends_exhausted error', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 429,
      json: () => Promise.resolve({ code: 'resends_exhausted' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Send new code'));

    await waitFor(() => {
      expect(
        screen.getAllByText("You've used all 4 resend attempts. Please start over.").length
      ).toBeGreaterThan(0);
    });
  });

  it('shows generic error on unknown resend failure', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: () => Promise.resolve({ code: 'server_error' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Send new code'));

    await waitFor(() => {
      expect(
        screen.getAllByText('Failed to resend code. Please try again.').length
      ).toBeGreaterThan(0);
    });
  });

  it('shows too_many_attempts error on code submit', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 429,
      json: () => Promise.resolve({ code: 'too_many_attempts' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: '0' } });
    }

    await waitFor(() => {
      expect(screen.getAllByText('Too many attempts — request a new code.').length).toBeGreaterThan(
        0
      );
    });
  });

  it('shows fallback error for unrecognized status', async () => {
    mockFetch.mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: () => Promise.resolve({ code: 'some_unknown_code' }),
    });

    render(<EmailVerification onSuccess={mockOnSuccess} />);
    const inputs = screen.getAllByRole('textbox');
    for (let i = 0; i < 6; i++) {
      fireEvent.change(inputs[i], { target: { value: '0' } });
    }

    await waitFor(() => {
      expect(screen.getAllByText('Verification failed. Please try again.').length).toBeGreaterThan(
        0
      );
    });
  });
});
