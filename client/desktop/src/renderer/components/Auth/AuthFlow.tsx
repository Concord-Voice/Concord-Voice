import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { ConnectionMode } from '../../types/auth';
import { useAuthStore } from '../../stores/authStore';
import { useUserStore, UserProfile } from '../../stores/userStore';
import { usePendingRegistrationStore } from '../../stores/pendingRegistrationStore';
import { useSSOStore } from '../../stores/ssoStore';
import { useClientConfigStore } from '../../stores/clientConfigStore';
import { resetRuntimeServerBase, setRuntimeServerBase } from '../../services/runtimeServerBase';
import ConnectionSelector from './ConnectionSelector';
import ServerInput from './ServerInput';
import Register from './Register';
import Login from './Login';
import AccountRecovery from './AccountRecovery';
import EmailVerification from './EmailVerification';
import { ChangeEmail } from './ChangeEmail';
import SSOPassphraseSetup from './SSOPassphraseSetup';
import SSOAccountLinkConfirm from './SSOAccountLinkConfirm';
import './AuthFlow.css';

/**
 * SSOErrorScreen — terminal-state UI for the SSO `error` phase. Maps known
 * stable error codes from the server / loopback layer to actionable copy and
 * gives the user a Try Again button that resets the SSO store back to idle.
 *
 * The renderer wired this branch in #808 review: previously, useSSOFlow set
 * `phase: 'error'` and AuthFlow had no matching branch — the error string was
 * silently held in the store with no UI surface.
 */
const SSOErrorScreen: React.FC = () => {
  const message = useSSOStore((s) => (s.state.phase === 'error' ? s.state.message : ''));
  const reset = useSSOStore((s) => s.reset);

  // Map known stable codes to actionable copy. Anything else falls through
  // to a generic message — we don't want to dump raw error strings into the UI.
  let title = 'Sign-in failed';
  let body = "We couldn't complete sign-in. Please try again.";
  if (message === 'oauth_state_mismatch') {
    title = 'Sign-in interrupted';
    body =
      "The sign-in attempt didn't match our records (possibly multiple browser windows)." +
      ' Please try again from this app.';
  } else if (message === 'oauth_timeout') {
    title = 'Sign-in timed out';
    body = 'The sign-in window was closed or took too long. Please try again.';
  } else if (message === 'redis_unavailable' || message === 'sso_unavailable') {
    title = 'Service temporarily unavailable';
    body = "We're having trouble reaching the sign-in service. Please try again in a moment.";
  } else if (message === 'oauth_cancelled') {
    title = 'Sign-in cancelled';
    body = "You cancelled the sign-in. Please try again when you're ready.";
  } else if (message === 'sso_mfa_failed') {
    title = 'Verification failed';
    body = "We couldn't verify your second factor. Please try again.";
  }

  return (
    <div className="auth-placeholder" role="alert">
      <h2>{title}</h2>
      <p>{body}</p>
      <button onClick={reset}>Try Again</button>
    </div>
  );
};

type AuthStep =
  | 'connection-select'
  | 'server-input'
  | 'hosted-register'
  | 'hosted-login'
  | 'email-verification'
  | 'change-email'
  | 'forgot-password';

const AuthFlow: React.FC = () => {
  const navigate = useNavigate();
  const accessToken = useAuthStore((state) => state.accessToken);
  const emailVerified = useAuthStore((state) => state.emailVerified);

  const hasPending = usePendingRegistrationStore((s) => s.pendingId !== null && !s.isExpired());

  // SSO state takes priority over the standard step flow: when an OAuth callback
  // routes the user into a follow-up flow (passphrase setup for new SSO accounts,
  // password confirmation for account linking), render that wizard instead of the
  // connection-selector chain. Phase 'idle' falls through to the existing flow.
  const ssoPhase = useSSOStore((s) => s.state.phase);

  // Derive initial step: pending registration resumes at email-verification,
  // otherwise a token without verified email goes to verification (login gate),
  // otherwise start at connection selection.
  const getInitialStep = (): AuthStep => {
    if (hasPending) return 'email-verification';
    if (accessToken && !emailVerified) return 'email-verification';
    return 'connection-select';
  };

  const [step, setStep] = useState<AuthStep>(getInitialStep);

  // Watch for async verification state changes (e.g. session restore → /users/me fetch
  // flips emailVerified to false after mount). Transition to verification when needed.
  useEffect(() => {
    if (accessToken && !emailVerified) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: transitions to email-verification step when store flips emailVerified after session restore; not a render loop
      setStep('email-verification');
    }
  }, [accessToken, emailVerified]);

  const handleConnectionSelect = (mode: ConnectionMode) => {
    if (mode === 'hosted') {
      setStep('hosted-register');
    } else if (mode === 'hosted-login') {
      setStep('hosted-login');
    } else {
      setStep('server-input');
    }
  };

  const handleServerConnect = (url: string) => {
    setRuntimeServerBase(url);
    useClientConfigStore.getState().setServerCapabilities(null);
    setStep('hosted-login');
  };

  const handleBack = () => {
    resetRuntimeServerBase();
    setStep('connection-select');
  };

  const handleRegistrationSuccess = (_data: { pendingId: string; email: string }) => {
    console.debug('Registration successful, pending email verification');
    setStep('email-verification');
  };

  const handleVerificationSuccess = () => {
    console.debug('Email verified, navigating to app');
    navigate('/app/dms');
  };

  const handleLoginSuccess = (data: {
    accessToken: string;
    user?: UserProfile;
    rememberMe: boolean;
  }) => {
    console.debug('Login successful, access token received');
    useAuthStore.getState().setAccessToken(data.accessToken);
    useAuthStore.getState().setRememberMe(data.rememberMe);
    if (data.user) {
      useUserStore.getState().setUser(data.user);

      // Gate unverified users behind email verification (same as registration)
      if (data.user.email_verified === false) {
        useAuthStore.getState().setEmailVerified(false);
        setStep('email-verification');
        return;
      }
    }
    useAuthStore.getState().setEmailVerified(true);
    navigate('/app/dms');
  };

  const handleSwitchToLogin = () => {
    setStep('hosted-login');
  };

  const handleSwitchToRegister = () => {
    setStep('hosted-register');
  };

  const handleForgotPassword = () => {
    setStep('forgot-password');
  };

  // SSO branches must render before the standard step flow so an in-flight
  // SSO callback (register_required / link_required) cannot be hidden by a
  // stale `step` value left over from a prior connection-select interaction.
  if (ssoPhase === 'register_required') {
    return (
      <div className="auth-flow">
        <div className="auth-screen">
          <SSOPassphraseSetup />
        </div>
      </div>
    );
  }

  if (ssoPhase === 'link_required') {
    return (
      <div className="auth-flow">
        <div className="auth-screen">
          <SSOAccountLinkConfirm />
        </div>
      </div>
    );
  }

  // mfa_required is handled by the existing MFAChallengeModal mounted at App
  // root via useSSOFlow's bridge into useMFAChallengeStore.
  if (ssoPhase === 'error') {
    return (
      <div className="auth-flow">
        <div className="auth-screen">
          <SSOErrorScreen />
        </div>
      </div>
    );
  }

  return (
    <div className="auth-flow">
      {step === 'connection-select' && (
        <div key="connection-select" className="auth-screen">
          <ConnectionSelector onSelect={handleConnectionSelect} />
        </div>
      )}

      {step === 'server-input' && (
        <div key="server-input" className="auth-screen">
          <ServerInput onConnect={handleServerConnect} onBack={handleBack} />
        </div>
      )}

      {step === 'hosted-register' && (
        <div key="hosted-register" className="auth-screen">
          <Register
            onBack={handleBack}
            onSuccess={handleRegistrationSuccess}
            onSwitchToLogin={handleSwitchToLogin}
          />
        </div>
      )}

      {step === 'hosted-login' && (
        <div key="hosted-login" className="auth-screen">
          <Login
            onBack={handleBack}
            onSuccess={handleLoginSuccess}
            onSwitchToRegister={handleSwitchToRegister}
            onForgotPassword={handleForgotPassword}
          />
        </div>
      )}

      {step === 'email-verification' && (
        <div key="email-verification" className="auth-screen">
          <EmailVerification
            maskEmail={false}
            onSuccess={handleVerificationSuccess}
            onChangeEmail={() => setStep('change-email')}
            onCancel={handleBack}
          />
        </div>
      )}

      {step === 'change-email' && (
        <div key="change-email" className="auth-screen">
          <ChangeEmail
            onDone={() => setStep('email-verification')}
            onCancel={() => setStep('email-verification')}
          />
        </div>
      )}

      {step === 'forgot-password' && (
        <div key="forgot-password" className="auth-screen">
          <AccountRecovery
            onBack={() => setStep('hosted-login')}
            onComplete={() => setStep('hosted-login')}
          />
        </div>
      )}
    </div>
  );
};

export default AuthFlow;
