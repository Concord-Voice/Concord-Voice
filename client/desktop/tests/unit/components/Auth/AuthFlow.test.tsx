import { render, screen, fireEvent } from '../../../test-utils';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { usePendingRegistrationStore } from '@/renderer/stores/pendingRegistrationStore';
import { useSSOStore } from '@/renderer/stores/ssoStore';
import { resetAllStores } from '../../../helpers/store-helpers';

// Mock child components to isolate AuthFlow logic
vi.mock('@/renderer/components/Auth/ConnectionSelector', () => ({
  default: ({ onSelect }: { onSelect: (mode: string) => void }) => (
    <div data-testid="connection-selector">
      <button onClick={() => onSelect('hosted')}>Select Hosted</button>
      <button onClick={() => onSelect('hosted-login')}>Select Login</button>
      <button onClick={() => onSelect('self-hosted')}>Select Self-Hosted</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/ServerInput', () => ({
  default: ({ onConnect, onBack }: { onConnect: (url: string) => void; onBack: () => void }) => (
    <div data-testid="server-input">
      <button onClick={() => onConnect('https://my.server.com')}>Connect</button>
      <button onClick={onBack}>Back</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/Register', () => ({
  default: ({ onBack, onSuccess, onSwitchToLogin }: any) => (
    <div data-testid="register">
      <button onClick={onBack}>Back</button>
      <button
        onClick={() =>
          onSuccess({
            pendingId: 'mock-pending-id',
            email: 'newuser@example.com',
          })
        }
      >
        Register Success
      </button>
      <button onClick={onSwitchToLogin}>Switch to Login</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/Login', () => ({
  default: ({ onBack, onSuccess, onSwitchToRegister, onForgotPassword }: any) => (
    <div data-testid="login">
      <button onClick={onBack}>Back</button>
      <button
        onClick={() =>
          onSuccess({
            accessToken: 'login-token',
            user: { id: 'u1', username: 'existing' },
            rememberMe: false,
          })
        }
      >
        Login Success
      </button>
      <button
        onClick={() =>
          onSuccess({
            accessToken: 'unverified-token',
            user: {
              id: 'u2',
              username: 'unverified',
              email_verified: false,
            },
            rememberMe: false,
          })
        }
      >
        Login Unverified
      </button>
      <button onClick={onSwitchToRegister}>Switch to Register</button>
      <button onClick={onForgotPassword}>Forgot Password</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/AccountRecovery', () => ({
  default: ({ onBack, onComplete }: any) => (
    <div data-testid="account-recovery">
      <button onClick={onBack}>Recovery Back</button>
      <button onClick={onComplete}>Recovery Complete</button>
    </div>
  ),
}));

const mockNavigate = vi.fn();
vi.mock('react-router-dom', async () => {
  const actual = await vi.importActual('react-router-dom');
  return { ...actual, useNavigate: () => mockNavigate };
});

vi.mock('@/renderer/components/Auth/EmailVerification', () => ({
  default: ({ onSuccess, onChangeEmail, onCancel }: any) => (
    <div data-testid="email-verification">
      <button onClick={onSuccess}>Verify Success</button>
      {onChangeEmail && <button onClick={onChangeEmail}>Change Email</button>}
      {onCancel && <button onClick={onCancel}>Cancel Verification</button>}
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/ChangeEmail', () => ({
  ChangeEmail: ({ onDone, onCancel }: any) => (
    <div data-testid="change-email">
      <button onClick={onDone}>Change Done</button>
      <button onClick={onCancel}>Change Cancel</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Auth/SSOPassphraseSetup', () => ({
  default: () => <div data-testid="sso-passphrase-setup">Welcome to Concord</div>,
}));

vi.mock('@/renderer/components/Auth/SSOAccountLinkConfirm', () => ({
  default: () => <div data-testid="sso-link-confirm">Link your Google account</div>,
}));

import AuthFlow from '@/renderer/components/Auth/AuthFlow';

describe('AuthFlow', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
  });

  it('renders connection selector initially', () => {
    render(<AuthFlow />);
    expect(screen.getByTestId('connection-selector')).toBeInTheDocument();
  });

  it('shows register form when hosted mode selected', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    expect(screen.getByTestId('register')).toBeInTheDocument();
  });

  it('shows login form when hosted-login mode selected', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    expect(screen.getByTestId('login')).toBeInTheDocument();
  });

  it('shows server input when self-hosted mode selected', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Self-Hosted'));
    expect(screen.getByTestId('server-input')).toBeInTheDocument();
  });

  it('navigates back to connection selector', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    expect(screen.getByTestId('register')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Back'));
    expect(screen.getByTestId('connection-selector')).toBeInTheDocument();
  });

  it('switches from register to login', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    expect(screen.getByTestId('register')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Switch to Login'));
    expect(screen.getByTestId('login')).toBeInTheDocument();
  });

  it('switches from login to register', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    expect(screen.getByTestId('login')).toBeInTheDocument();
    fireEvent.click(screen.getByText('Switch to Register'));
    expect(screen.getByTestId('register')).toBeInTheDocument();
  });

  it('shows self-hosted auth after server connect', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Self-Hosted'));
    fireEvent.click(screen.getByText('Connect'));
    expect(screen.getByText('Connecting to: https://my.server.com')).toBeInTheDocument();
  });

  it('handles registration success — routes to email-verification', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    fireEvent.click(screen.getByText('Register Success'));

    expect(screen.getByTestId('email-verification')).toBeInTheDocument();
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it('handles login success', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    fireEvent.click(screen.getByText('Login Success'));

    expect(useAuthStore.getState().accessToken).toBe('login-token');
    expect(useUserStore.getState().user?.username).toBe('existing');
    expect(mockNavigate).toHaveBeenCalledWith('/app/dms');
  });

  it('navigates to /app/dms after email verification success', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    fireEvent.click(screen.getByText('Register Success'));
    fireEvent.click(screen.getByText('Verify Success'));

    expect(mockNavigate).toHaveBeenCalledWith('/app/dms');
  });

  it('routes to change-email step when onChangeEmail triggered', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    fireEvent.click(screen.getByText('Register Success'));
    fireEvent.click(screen.getByText('Change Email'));

    expect(screen.getByTestId('change-email')).toBeInTheDocument();
  });

  it('returns to email-verification from change-email on done', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    fireEvent.click(screen.getByText('Register Success'));
    fireEvent.click(screen.getByText('Change Email'));
    fireEvent.click(screen.getByText('Change Done'));

    expect(screen.getByTestId('email-verification')).toBeInTheDocument();
  });

  it('returns to email-verification from change-email on cancel', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Hosted'));
    fireEvent.click(screen.getByText('Register Success'));
    fireEvent.click(screen.getByText('Change Email'));
    fireEvent.click(screen.getByText('Change Cancel'));

    expect(screen.getByTestId('email-verification')).toBeInTheDocument();
  });

  it('shows forgot-password step when onForgotPassword triggered', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    fireEvent.click(screen.getByText('Forgot Password'));

    expect(screen.getByTestId('account-recovery')).toBeInTheDocument();
  });

  it('returns to login from forgot-password on back', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    fireEvent.click(screen.getByText('Forgot Password'));
    fireEvent.click(screen.getByText('Recovery Back'));

    expect(screen.getByTestId('login')).toBeInTheDocument();
  });

  it('returns to login from forgot-password on complete', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    fireEvent.click(screen.getByText('Forgot Password'));
    fireEvent.click(screen.getByText('Recovery Complete'));

    expect(screen.getByTestId('login')).toBeInTheDocument();
  });

  it('routes to email-verification when login returns unverified user', () => {
    render(<AuthFlow />);
    fireEvent.click(screen.getByText('Select Login'));
    fireEvent.click(screen.getByText('Login Unverified'));

    expect(screen.getByTestId('email-verification')).toBeInTheDocument();
  });

  it('starts at email-verification when pending registration exists', () => {
    usePendingRegistrationStore.getState().setPending({
      pending_id: 'existing-pending-id',
      email: 'existing@example.com',
      expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
      code_expires_at: new Date(Date.now() + 2 * 60_000).toISOString(),
    });

    render(<AuthFlow />);
    expect(screen.getByTestId('email-verification')).toBeInTheDocument();
  });

  // SSO phase routing (#270, Task 21) ──────────────────────────────────────
  // Verifies AuthFlow re-renders into the right SSO wizard when an OAuth
  // callback drives ssoStore into a non-idle phase, and falls through to
  // the standard connection-select flow when phase=idle.

  it('renders SSOPassphraseSetup when ssoStore phase=register_required', () => {
    useSSOStore.getState().setState({
      phase: 'register_required',
      provider: 'google',
      ssoToken: 'tok-fake',
      email: 'new@example.test',
      name: 'New User',
    });

    render(<AuthFlow />);
    expect(screen.getByTestId('sso-passphrase-setup')).toBeInTheDocument();
    expect(screen.getByText(/welcome to concord/i)).toBeInTheDocument();
  });

  it('renders SSOAccountLinkConfirm when ssoStore phase=link_required', () => {
    useSSOStore.getState().setState({
      phase: 'link_required',
      provider: 'google',
      ssoToken: 'tok-link',
      maskedEmail: 'a***@example.test',
    });

    render(<AuthFlow />);
    expect(screen.getByTestId('sso-link-confirm')).toBeInTheDocument();
    expect(screen.getByText(/link your google account/i)).toBeInTheDocument();
  });

  it('falls through to connection-selector when ssoStore phase=idle', () => {
    // Default state after resetAllStores() — verifies the SSO branches do
    // not swallow the standard non-SSO flow.
    render(<AuthFlow />);
    expect(screen.getByTestId('connection-selector')).toBeInTheDocument();
    expect(screen.queryByTestId('sso-passphrase-setup')).not.toBeInTheDocument();
    expect(screen.queryByTestId('sso-link-confirm')).not.toBeInTheDocument();
  });

  it('renders an SSO error screen with retry when ssoStore phase=error', () => {
    // Without this branch, useSSOFlow would set phase='error' and AuthFlow
    // would silently render the connection selector — the user would never
    // see the failure or have a way to retry.
    useSSOStore.getState().setState({
      phase: 'error',
      message: 'oauth_state_mismatch',
    });

    render(<AuthFlow />);
    // Mapped friendly title for the known stable code.
    expect(screen.getByText(/sign-in interrupted/i)).toBeInTheDocument();
    const retryBtn = screen.getByRole('button', { name: /try again/i });
    expect(retryBtn).toBeInTheDocument();

    // Clicking Try Again resets the store back to idle so the user lands at
    // the connection selector.
    fireEvent.click(retryBtn);
    expect(useSSOStore.getState().state.phase).toBe('idle');
  });

  it('SSO error screen falls back to a generic message for unknown error codes', () => {
    useSSOStore.getState().setState({
      phase: 'error',
      message: 'something_unrecognized',
    });

    render(<AuthFlow />);
    expect(screen.getByText(/sign-in failed/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
  });
});
