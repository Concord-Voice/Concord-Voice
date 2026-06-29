import React, { useState, useEffect, useCallback } from 'react';
import TOTPInput from './TOTPInput';
import LoadingSpinner from './LoadingSpinner';
import { useAuthStore } from '../../stores/authStore';
import { usePendingRegistrationStore } from '../../stores/pendingRegistrationStore';
import { apiUrl, getApiBase } from '../../services/runtimeServerBase';
import './EmailVerification.css';
import './TOTPInput.css';

function pluralAttempts(count: number): string {
  return count === 1 ? 'attempt' : 'attempts';
}

interface ConfirmErrorResult {
  message: string;
  attemptsRemaining?: number;
  shouldCancel?: boolean;
}

function mapConfirmError(
  status: number,
  errCode: string | undefined,
  attemptsRemaining: number | undefined
): ConfirmErrorResult {
  if (status === 401 && errCode === 'invalid_code') {
    if (attemptsRemaining !== undefined) {
      return {
        message: `Incorrect code. ${attemptsRemaining} ${pluralAttempts(attemptsRemaining)} remaining.`,
        attemptsRemaining,
      };
    }
    return { message: 'Incorrect code.' };
  }
  if (status === 410 && errCode === 'code_expired') {
    return { message: 'Code expired — tap resend.' };
  }
  if (status === 410 && errCode === 'pending_expired') {
    return {
      message: 'Registration session expired. Please register again.',
      shouldCancel: true,
    };
  }
  if (status === 429 && errCode === 'too_many_attempts') {
    return { message: 'Too many attempts — request a new code.' };
  }
  return { message: 'Verification failed. Please try again.' };
}

interface EmailVerificationProps {
  maskEmail?: boolean;
  onSuccess: () => void;
  onChangeEmail?: () => void;
  onCancel?: () => void;
}

function maskEmailAddress(email: string): string {
  const [local, domain] = email.split('@');
  if (!domain) return email;
  if (local.length <= 2) return `${local[0]}***@${domain}`;
  return `${local[0]}${'*'.repeat(Math.min(local.length - 2, 6))}${local.at(-1)}@${domain}`;
}

interface ConfirmResponse {
  access_token: string;
  refresh_token: string;
  session_id?: string;
  user?: { email_verified?: boolean };
}

interface ResendResponse {
  code_expires_at: string;
  resends_remaining: number;
}

const EmailVerification: React.FC<EmailVerificationProps> = ({
  maskEmail = true,
  onSuccess,
  onChangeEmail,
  onCancel,
}) => {
  const pendingId = usePendingRegistrationStore((s) => s.pendingId);
  const email = usePendingRegistrationStore((s) => s.email);
  const resendsRemaining = usePendingRegistrationStore((s) => s.resendsRemaining);
  const updateAfterResend = usePendingRegistrationStore((s) => s.updateAfterResend);
  const clearPending = usePendingRegistrationStore((s) => s.clearPending);

  const [error, setError] = useState<string>('');
  const [isVerifying, setIsVerifying] = useState(false);
  const [isResending, setIsResending] = useState(false);
  const [isCancelling, setIsCancelling] = useState(false);
  const [resendCooldown, setResendCooldown] = useState(0);
  const [attemptsRemaining, setAttemptsRemaining] = useState<number | null>(null);

  useEffect(() => {
    if (resendCooldown <= 0) return;
    const timer = setInterval(() => {
      setResendCooldown((prev) => Math.max(0, prev - 1));
    }, 1000);
    return () => clearInterval(timer);
  }, [resendCooldown]);

  const displayEmail = email ?? '';
  const shownEmail = maskEmail ? maskEmailAddress(displayEmail) : displayEmail;

  const handleSubmitCode = useCallback(
    async (code: string) => {
      if (isVerifying || !pendingId) return;
      setIsVerifying(true);
      setError('');

      try {
        const response = await fetch(apiUrl('/api/v1/auth/register/confirm'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          credentials: 'include',
          body: JSON.stringify({ pending_id: pendingId, code }),
        });

        const data = (await response.json()) as ConfirmResponse & {
          code?: string;
          attempts_remaining?: number;
          retry_after_seconds?: number;
        };

        if (response.ok) {
          if (globalThis.electron?.storeRefreshToken) {
            await globalThis.electron.storeRefreshToken({
              refreshToken: data.refresh_token,
              rememberMe: true,
              apiBase: getApiBase(),
              accessToken: data.access_token,
            });
          }
          useAuthStore.getState().setAccessToken(data.access_token);
          if (data.session_id) useAuthStore.getState().setSessionId(data.session_id);
          useAuthStore.getState().setEmailVerified(true);
          clearPending();
          onSuccess();
          return;
        }

        const result = mapConfirmError(response.status, data.code, data.attempts_remaining);
        setError(result.message);
        if (result.attemptsRemaining !== undefined) {
          setAttemptsRemaining(result.attemptsRemaining);
        }
        if (result.shouldCancel) {
          clearPending();
          onCancel?.();
        }
      } finally {
        setIsVerifying(false);
      }
    },
    [pendingId, isVerifying, clearPending, onSuccess, onCancel]
  );

  const handleResend = async () => {
    if (resendCooldown > 0 || isResending || !pendingId) return;
    setIsResending(true);
    setError('');

    try {
      const response = await fetch(apiUrl('/api/v1/auth/register/resend'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ pending_id: pendingId }),
      });

      const data = (await response.json()) as ResendResponse & {
        code?: string;
        retry_after_seconds?: number;
      };

      if (!response.ok) {
        const errCode = data.code;
        if (response.status === 429 && errCode === 'cooldown_active') {
          const after = data.retry_after_seconds ?? 120;
          setResendCooldown(after);
          setError('');
        } else if (response.status === 429 && errCode === 'resends_exhausted') {
          setError("You've used all 4 resend attempts. Please start over.");
        } else {
          setError('Failed to resend code. Please try again.');
        }
        return;
      }

      updateAfterResend({
        code_expires_at: data.code_expires_at,
        resends_remaining: data.resends_remaining,
      });
      setResendCooldown(120);
      setAttemptsRemaining(null);
    } finally {
      setIsResending(false);
    }
  };

  const handleCancel = async () => {
    if (!pendingId || isCancelling) return;
    setIsCancelling(true);
    try {
      const res = await fetch(apiUrl(`/api/v1/auth/register/${pendingId}`), {
        method: 'DELETE',
      });
      if (res.status === 204 || res.status === 404) {
        // 204 = deleted, 404 = already gone — both acceptable terminal states
        clearPending();
        onCancel?.();
      } else {
        // Surface the error; keep local pending state so retry is possible
        setError('Failed to cancel registration. Please try again.');
      }
    } catch {
      setError('Network error canceling registration. Please try again.');
    } finally {
      setIsCancelling(false);
    }
  };

  const resendDisabled = resendCooldown > 0 || isResending || resendsRemaining <= 0;

  return (
    <div className="email-verification">
      <div className="email-verification-header">
        <h2>Verify your email</h2>
        <p className="email-verification-subtitle">
          Enter the 6-digit code sent to <strong>{shownEmail}</strong>
        </p>
      </div>

      <div className="email-verification-input">
        <TOTPInput onSubmit={handleSubmitCode} disabled={isVerifying} error={error} autoFocus />
        {isVerifying && (
          <div className="email-verification-loading">
            <LoadingSpinner />
          </div>
        )}
      </div>

      {attemptsRemaining !== null && attemptsRemaining > 0 && (
        <p className="email-verification-attempts">
          {attemptsRemaining} {pluralAttempts(attemptsRemaining)} remaining
        </p>
      )}

      <div className="email-verification-actions">
        <button
          type="button"
          className="email-verification-resend"
          onClick={handleResend}
          disabled={resendDisabled}
        >
          {(() => {
            if (isResending) return 'Sending...';
            if (resendCooldown > 0) return `Send new code (${resendCooldown}s)`;
            return 'Send new code';
          })()}
        </button>

        {onChangeEmail && (
          <button type="button" className="email-verification-change" onClick={onChangeEmail}>
            Change email
          </button>
        )}
      </div>

      {error && <p className="email-verification-error">{error}</p>}

      {onCancel && (
        <button
          type="button"
          className="email-verification-back"
          onClick={handleCancel}
          disabled={isCancelling}
        >
          {isCancelling ? 'Cancelling...' : '← Cancel registration'}
        </button>
      )}
    </div>
  );
};

export default EmailVerification;
