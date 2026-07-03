import React, { useState } from 'react';
import { apiFetch, safeJson } from '../../../services/apiClient';
import { useSubscriptionStore } from '../../../stores/subscriptionStore';

// The universal redeem form (#1304 / #1303 engine). Labelled input → POST
// /api/v1/redeem {code}. On 200 it shows the server-returned `description` (what
// was granted) in a native <output> (implicit role="status"), then refreshes the
// status view + re-hydrates
// the entitlement store (the server ALSO pushes entitlements_changed, so the
// store updates live — this is belt-and-suspenders for an immediate refresh).
//
// On error it shows a status-specific message in a role="alert". The engine's
// status contract (services/control-plane/internal/redemption/handler.go) is:
//   400 → generic "not valid" (no-oracle: bad checksum / not found / expired /
//         revoked / exhausted all collapse here — the client CANNOT distinguish),
//   409 → the user already redeemed THIS code (idempotent, not an oracle),
//   otherwise → an infra error (the grant may still have landed post-commit).
// There is deliberately no 410 branch — the engine never returns one (expired /
// revoked are folded into the 400 no-oracle bucket).

interface RedeemSuccess {
  success?: boolean;
  description?: string;
}

interface RedeemCodeFormProps {
  // Called after a successful redeem so the parent re-reads /subscriptions/me.
  onRedeemed: () => void;
}

const RedeemCodeForm: React.FC<RedeemCodeFormProps> = ({ onRedeemed }) => {
  const [code, setCode] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [success, setSuccess] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const hydrateEntitlements = useSubscriptionStore((s) => s.hydrate);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    const trimmed = code.trim();
    // Client-side non-empty guard only — the server is authoritative on format
    // (checksum, prefix). We never try to validate the code shape here.
    if (!trimmed) {
      setError('Enter a code to redeem.');
      setSuccess(null);
      return;
    }

    setSubmitting(true);
    setError(null);
    setSuccess(null);

    try {
      const res = await apiFetch('/api/v1/redeem', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code: trimmed }),
      });

      if (res.ok) {
        // The redemption already succeeded server-side. Parse the body for the
        // richer "what was granted" description, but a parse failure on a 200
        // must NOT fall into the network-error catch below — the code IS
        // redeemed, so we still confirm success (with a generic message).
        let description = 'Your code was redeemed.';
        try {
          const data = await safeJson<RedeemSuccess>(res);
          if (data?.description) description = data.description;
        } catch {
          // Unparseable success body — keep the generic confirmation.
        }
        setSuccess(description);
        setCode('');
        // Re-hydrate the entitlement store immediately (the WS push also fires,
        // but this makes the FeatureGrid/PlanCard update without a race), then
        // let the parent re-read the richer subscription status.
        await hydrateEntitlements();
        onRedeemed();
        return;
      }

      setError(messageForStatus(res.status));
    } catch {
      setError('Could not reach the server. Check your connection and try again.');
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form className="subscription-redeem-form" onSubmit={handleSubmit}>
      <label htmlFor="redeem-code" className="subscription-redeem-label">
        Redeem a code
      </label>
      <div className="subscription-redeem-row">
        <input
          id="redeem-code"
          type="text"
          className="subscription-redeem-input"
          placeholder="e.g. KS-XXXX-XXXX-XXXX"
          autoComplete="off"
          spellCheck={false}
          value={code}
          onChange={(ev) => {
            setCode(ev.target.value);
            if (error) setError(null);
          }}
          disabled={submitting}
        />
        <button
          type="submit"
          className="subscription-redeem-submit"
          disabled={submitting || code.trim().length === 0}
        >
          {submitting ? 'Redeeming…' : 'Redeem'}
        </button>
      </div>

      {error && (
        <div className="subscription-redeem-alert" role="alert">
          {error}
        </div>
      )}
      {success && (
        // Native <output> — implicit role="status" with better AT support (Sonar
        // S6819). getByRole('status') still matches it.
        <output className="subscription-redeem-status">{success}</output>
      )}
    </form>
  );
};

// messageForStatus maps the engine's HTTP status to a distinct user message. The
// 400 message stays generic (no-oracle contract) — it must NOT claim to know
// WHY the code failed.
function messageForStatus(status: number): string {
  switch (status) {
    case 400:
      return 'That code is not valid. Double-check it and try again.';
    case 409:
      return 'You have already redeemed this code.';
    case 429:
      return 'Too many attempts. Please wait a moment and try again.';
    default:
      return 'Could not redeem the code right now. Please try again.';
  }
}

export default RedeemCodeForm;
