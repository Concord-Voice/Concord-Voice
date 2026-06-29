import { useState } from 'react';
import { usePendingRegistrationStore } from '../../stores/pendingRegistrationStore';
import { apiUrl } from '../../services/runtimeServerBase';
import './ChangeEmail.css';

interface Props {
  readonly onDone: () => void;
  readonly onCancel: () => void;
}

function errorMessage(status: number, code?: string): string {
  if (code === 'email_already_registered') return 'An account with this email already exists.';
  if (code === 'registration_pending')
    return 'This email has an active registration — wait 15 minutes or use the correct password.';
  if (code === 'pending_expired') return 'Your registration session expired. Please start over.';
  if (status === 429) return 'Too many email changes — wait 15 minutes.';
  return 'Failed to change email. Try again.';
}

interface ChangeEmailResult {
  error?: string;
}

async function submitChangeEmail(pendingId: string, newEmail: string): Promise<ChangeEmailResult> {
  const res = await fetch(apiUrl('/api/v1/auth/register/change-email'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ pending_id: pendingId, new_email: newEmail }),
  });
  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as { code?: string };
    return { error: errorMessage(res.status, body.code) };
  }
  const { email, code_expires_at } = (await res.json()) as {
    email: string;
    code_expires_at: string;
  };
  usePendingRegistrationStore.getState().updateEmail(email, code_expires_at);
  return {};
}

export function ChangeEmail({ onDone, onCancel }: Props) {
  const pendingId = usePendingRegistrationStore((s) => s.pendingId);
  const [newEmail, setNewEmail] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const runSubmit = async () => {
    if (!pendingId) return;
    setSubmitting(true);
    setError(null);
    try {
      const result = await submitChangeEmail(pendingId, newEmail);
      if (result.error) {
        setError(result.error);
        return;
      }
      onDone();
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form
      className="change-email-form"
      onSubmit={(e) => {
        e.preventDefault();
        void runSubmit();
      }}
    >
      <h2>Change email address</h2>
      <p>Enter the corrected email. A new verification code will be sent.</p>
      <label>
        <span>{'New email'}</span>
        <input
          type="email"
          value={newEmail}
          onChange={(e) => setNewEmail(e.target.value)}
          required
          autoComplete="email"
          aria-label="New email"
          aria-describedby={error ? 'change-email-error' : undefined}
        />
      </label>
      {error && (
        <p id="change-email-error" className="error">
          {error}
        </p>
      )}
      <div className="buttons">
        <button type="submit" disabled={submitting}>
          Send code to new email
        </button>
        <button type="button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </form>
  );
}
