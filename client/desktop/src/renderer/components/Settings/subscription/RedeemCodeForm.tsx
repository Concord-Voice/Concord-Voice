import React, { useEffect, useRef, useState } from 'react';
import { apiFetch, safeJson } from '../../../services/system/apiClient';
import { useSubscriptionStore } from '../../../stores/auth/subscriptionStore';
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
//   409 → the user already redeemed THIS code (idempotent, not an oracle), or,
//         with error `credit_confirmation_required`, a code for another plan
//         would become face-value credit on the user's subscription (#2033 D1):
//         the form shows the quote and re-sends with `confirm_credit` on Add;
//         with error `seat_member`, the user's access comes from a friend's
//         Wing and the code stays unused (#2033 PR-5, D5),
//   503 `billing_unavailable` → billing is not set up for credit; nothing redeemed
//         (any other 503 gets the generic message),
//   otherwise → an infra error (the grant may still have landed post-commit).
// There is deliberately no 410 branch — the engine never returns one (expired /
// revoked are folded into the 400 no-oracle bucket).

interface RedeemSuccess {
  success?: boolean;
  description?: string;
}

// The quote a 409 credit_confirmation_required carries. Amounts are cents, USD.
interface CreditQuote {
  amount: number;
  currency: string;
  code_plan: string;
  subscribed_plan: string;
}

interface CreditConfirmationBody {
  error?: string;
  credit?: Partial<CreditQuote>;
}

function creditQuote(body: CreditConfirmationBody | null): CreditQuote | null {
  const c = body?.credit;
  if (body?.error !== 'credit_confirmation_required' || !c) return null;
  // Money is confirmed against this quote: only whole, positive USD cents pass;
  // anything else falls back to the plain 409 message.
  if (!Number.isSafeInteger(c.amount) || (c.amount as number) <= 0 || c.currency !== 'usd')
    return null;
  if (typeof c.code_plan !== 'string' || typeof c.subscribed_plan !== 'string') return null;
  return {
    amount: c.amount as number,
    currency: c.currency,
    code_plan: c.code_plan,
    subscribed_plan: c.subscribed_plan,
  };
}

const PLAN_NAMES: Record<string, string> = { supersonic: 'Supersonic', wing: 'Wing' };
const planName = (plan: string) => PLAN_NAMES[plan] ?? plan;
const dollars = (cents: number) => `$${(cents / 100).toFixed(2)}`;

/** Parse a JSON body, or null when it is missing or unparseable. */
async function readJson<T>(res: Response): Promise<T | null> {
  try {
    return await safeJson<T>(res);
  } catch {
    return null;
  }
}

/** What a refused /redeem means: a credit offer to confirm, or a message. */
async function redeemFailure(res: Response): Promise<{ quote: CreditQuote } | { message: string }> {
  if (res.status === 409) {
    // An unparseable 409 is treated as the already-redeemed case.
    const body = await readJson<CreditConfirmationBody>(res);
    const quote = creditQuote(body);
    if (quote) return { quote };
    if (body?.error === 'seat_member') return { message: SEAT_MEMBER_MESSAGE };
  }
  if (res.status === 503) {
    // Only billing_unavailable is known to leave the code unused.
    const body = await readJson<{ error?: string }>(res);
    if (body?.error !== 'billing_unavailable') return { message: messageForStatus(0) };
  }
  return { message: messageForStatus(res.status) };
}

interface RedeemCodeFormProps {
  // Called after a successful redeem so the parent re-reads /subscriptions/me.
  onRedeemed: () => void;
}

const RedeemCodeForm: React.FC<RedeemCodeFormProps> = ({ onRedeemed }) => {
  const [code, setCode] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [result, setResult] = useState<RedeemResult | null>(null);
  const [quote, setQuote] = useState<CreditQuote | null>(null);
  const hydrateEntitlements = useSubscriptionStore((s) => s.hydrate);
  const inputRef = useRef<HTMLInputElement>(null);
  const confirmRef = useRef<HTMLFieldSetElement>(null);

  // Move focus to the quote when it appears, so a screen reader reads it.
  useEffect(() => {
    if (quote) confirmRef.current?.focus();
  }, [quote]);

  const cancelQuote = () => {
    setQuote(null);
    inputRef.current?.focus();
  };

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

    await redeem(trimmed, false);
  };

  const redeem = async (trimmed: string, confirmCredit: boolean) => {
    setSubmitting(true);
    setResult(null);
    setQuote(null);

    try {
      const res = await apiFetch('/api/v1/redeem', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(
          confirmCredit ? { code: trimmed, confirm_credit: true } : { code: trimmed }
        ),
      });

      if (res.ok) {
        // The redemption already succeeded server-side. Parse the body for the
        // richer "what was granted" description, but a parse failure on a 200
        // must NOT fall into the network-error catch below — the code IS
        // redeemed, so we still confirm success (with a generic message).
        const data = await readJson<RedeemSuccess>(res);
        setResult({ kind: 'success', message: data?.description || 'Your code was redeemed.' });
        setCode('');
        // Re-hydrate the entitlement store immediately (the WS push also fires,
        // but this makes the FeatureGrid/PlanCard update without a race), then
        // let the parent re-read the richer subscription status.
        await hydrateEntitlements();
        onRedeemed();
        return;
      }

      const failure = await redeemFailure(res);
      if ('quote' in failure) setQuote(failure.quote);
      else setResult({ kind: 'error', message: failure.message });
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
          ref={inputRef}
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
            setQuote(null);
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

      {quote && (
        <fieldset
          ref={confirmRef}
          className="subscription-redeem-confirm"
          aria-label="Confirm credit"
          tabIndex={-1}
        >
          <p>
            This code is for {planName(quote.code_plan)}, and you subscribe to{' '}
            {planName(quote.subscribed_plan)}. It becomes {dollars(quote.amount)} of credit that
            your next renewals use before your card is charged.
          </p>
          <div className="subscription-redeem-row">
            <button
              type="button"
              className="subscription-redeem-submit"
              disabled={submitting}
              onClick={() => void redeem(code.trim(), true)}
            >
              Add {dollars(quote.amount)} credit
            </button>
            <button
              type="button"
              className="subscription-redeem-submit subscription-redeem-cancel"
              disabled={submitting}
              onClick={cancelQuote}
            >
              Cancel
            </button>
          </div>
        </fieldset>
      )}
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

const SEAT_MEMBER_MESSAGE =
  "You have Supersonic through a friend's Wing. Your code was not used. Leave Wing first to use it.";

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
    case 503:
      return 'Could not add this code to your subscription right now. Your code was not used. Please try again.';
    default:
      return 'Could not redeem the code right now. Please try again.';
  }
}

export default RedeemCodeForm;
