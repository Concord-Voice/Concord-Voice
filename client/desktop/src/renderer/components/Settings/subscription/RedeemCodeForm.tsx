import React, { useState } from 'react';
import { apiFetch, safeJson } from '../../../services/apiClient';
import { useSubscriptionStore } from '../../../stores/subscriptionStore';
import { easterEggMessage, type RedeemResult } from './redeemEasterEgg';

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
  const [result, setResult] = useState<RedeemResult | null>(null);
  const hydrateEntitlements = useSubscriptionStore((s) => s.hydrate);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    // The server is authoritative on format (checksum, prefix); we never validate
    // the code shape here. There is deliberately no empty-input branch: the submit
    // button is disabled while `code.trim()` is empty, and HTML implicit submission
    // does not fire when the form's default button is disabled, so one would be
    // unreachable.
    const trimmed = code.trim();

    // Two ciphertexts from the intro-video blackboard (#2859) get a neutral local
    // reply and no request at all. The /redeem limiters (10/min per user, 20/min
    // per IP) are fail-OPEN, so this is an availability and UX benefit rather than
    // a security control: a curious viewer gets a friendly reply instead of a red
    // failure, and a shared-NAT audience does not push each other toward a 429.
    //
    // This MUST sit before setSubmitting(true): the early return skips the
    // finally block below, so a short-circuit placed after it would leave the
    // form permanently disabled with the button stuck on "Redeeming…".
    const egg = easterEggMessage(trimmed);
    if (egg) {
      setResult({ kind: 'notice', message: egg });
      return;
    }

    setSubmitting(true);
    setResult(null);

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
        setResult({ kind: 'success', message: description });
        setCode('');
        // Re-hydrate the entitlement store immediately (the WS push also fires,
        // but this makes the FeatureGrid/PlanCard update without a race), then
        // let the parent re-read the richer subscription status.
        await hydrateEntitlements();
        onRedeemed();
        return;
      }

      setResult({ kind: 'error', message: messageForStatus(res.status) });
    } catch {
      setResult({
        kind: 'error',
        message: 'Could not reach the server. Check your connection and try again.',
      });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form className="subscription-redeem-form" onSubmit={handleSubmit}>
      <label htmlFor="redeem-code" className="subscription-redeem-label">
        Code
      </label>
      <p id="redeem-code-help" className="settings-section-description">
        Enter a code you received from Concord Voice.
      </p>
      <div className="subscription-redeem-row">
        <input
          id="redeem-code"
          type="text"
          className="subscription-redeem-input"
          placeholder="Enter a code"
          aria-describedby="redeem-code-help"
          autoComplete="off"
          spellCheck={false}
          value={code}
          onChange={(ev) => {
            setCode(ev.target.value);
            if (result?.kind === 'error') setResult(null);
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

      {result?.kind === 'error' && (
        <div className="subscription-redeem-alert" role="alert">
          {result.message}
        </div>
      )}
      {result && result.kind !== 'error' && (
        // Native <output> — implicit role="status" with better AT support (Sonar
        // S6819). getByRole('status') still matches it. `success` and `notice`
        // both land here: a neutral reply must never interrupt a screen reader
        // the way role="alert" does.
        <output className="subscription-redeem-status">{result.message}</output>
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
