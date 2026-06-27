import { useState, useEffect, useRef } from 'react';
import { submitSignedAgeClaim } from '../../services/ageClaim/ageClaimService';
import { type AgeSignal } from '../../services/ageClaim/evaluateAge';
import { useAgeStatus } from '../../hooks/useAgeStatus';
import './NsfwContentGate.css';

// parseDob only ever produces the birthdate variant; narrow so the confirm screen can
// read year/month/day without re-narrowing the AgeSignal union.
type BirthdateSignal = Extract<AgeSignal, { kind: 'birthdate' }>;

type Phase =
  | { kind: 'form' }
  | { kind: 'confirm'; signal: BirthdateSignal }
  | { kind: 'submitting' }
  | { kind: 'unlocked' } // >=18
  | { kind: 'verifiedLocked' } // 16–17
  | { kind: 'disabled' } // <16, or re-submit after a prior disable
  | { kind: 'error'; message: string };

const ERROR_COPY: Record<string, string> = {
  unavailable: "Couldn't reach the server. Check your connection and try again.",
  invalid_signature: 'Your device key may have changed. Please sign in again and retry.',
  stale_key_version: 'Your device key just rotated. Please try again.',
};

function errorCopyFor(code: string): string {
  return (
    ERROR_COPY[code] ?? 'Something went wrong submitting your age verification. Please try again.'
  );
}

/**
 * Parse the three numeric fields into an AgeSignal, or null when the date is incomplete,
 * impossible (e.g. Feb 31 — caught by the UTC round-trip), or in the future. Pure: `now`
 * is injected so the future-date check is deterministic. Exported for direct unit testing.
 */
export function parseDob(
  yearStr: string,
  monthStr: string,
  dayStr: string,
  now: Date
): BirthdateSignal | null {
  if (yearStr === '' || monthStr === '' || dayStr === '') return null;
  const year = Number(yearStr);
  const month = Number(monthStr);
  const day = Number(dayStr);
  if (!Number.isInteger(year) || !Number.isInteger(month) || !Number.isInteger(day)) return null;
  if (month < 1 || month > 12 || day < 1 || day > 31) return null;
  if (year < 1900 || year > now.getUTCFullYear()) return null;
  const d = new Date(Date.UTC(year, month - 1, day));
  if (d.getUTCFullYear() !== year || d.getUTCMonth() !== month - 1 || d.getUTCDate() !== day) {
    return null; // impossible date rolled over
  }
  if (d.getTime() > now.getTime()) return null; // future date
  return { kind: 'birthdate', year, month, day };
}

const NsfwContentGate = () => {
  const status = useAgeStatus();
  const [year, setYear] = useState('');
  const [month, setMonth] = useState('');
  const [day, setDay] = useState('');
  const [phase, setPhase] = useState<Phase>({ kind: 'form' });
  // Captured once at mount (state initializer — keeps render pure). Bounds the
  // future-date check + the year input's max; a settings form is short-lived so a
  // mount-time "now" is fine.
  const [now] = useState(() => new Date());
  const confirmRef = useRef<HTMLFieldSetElement>(null);

  // Move focus to the confirm group EXACTLY ONCE when entering the confirm phase —
  // announces the step to screen readers and keeps keyboard focus off document body when
  // the prior step unmounts. Keyed on phase.kind (not an inline ref callback) so a
  // re-render within the confirm phase does NOT yank focus back from a button the user
  // tabbed to (Gitar/Copilot/@code-reviewer review). Targets the non-actionable container,
  // never Submit, so a stray Enter cannot fire the irreversible submit.
  useEffect(() => {
    if (phase.kind === 'confirm') confirmRef.current?.focus();
  }, [phase.kind]);

  // Mount-time rehydration of the durable verified outcome (#1763). Only short-circuits
  // the first-run form: once the user has entered the local submit flow (confirm /
  // submitting / a terminal result), those phases own the render below. A fail-closed
  // 'unverified' status falls through to the DOB form, so a degraded read re-prompts
  // rather than under-gates.
  if (phase.kind === 'form') {
    if (status.state === 'loading') {
      return <output className="nsfw-gate__status">Checking your verification status…</output>;
    }
    if (status.state === 'verified') {
      return status.nsfwAuth ? (
        <p className="nsfw-gate__satisfied">
          Your age is already verified — NSFW content access is enabled.
        </p>
      ) : (
        <output className="nsfw-gate__status">
          Your age is verified. NSFW content access requires you to be 18 or older, so it remains
          locked.
        </output>
      );
    }
  }

  const clearDob = () => {
    setYear('');
    setMonth('');
    setDay('');
  };

  // Recomputed each render (pure, in-memory) to gate the submit button.
  const signal = parseDob(year, month, day, now);

  const handleReview = () => {
    if (signal) setPhase({ kind: 'confirm', signal });
  };

  const handleConfirm = async (confirmed: BirthdateSignal) => {
    setPhase({ kind: 'submitting' });
    const result = await submitSignedAgeClaim({ signal: confirmed });
    clearDob(); // raw DOB no longer needed — discard from component state
    if (result.ok) {
      // Render the verdict the service SIGNED + submitted — exactly the value the server
      // enforces the disable on — never a second client recompute that could disagree at a
      // birthday boundary across the round-trip (#1625).
      if (result.validAge) {
        setPhase({ kind: result.nsfwAuth ? 'unlocked' : 'verifiedLocked' });
      } else {
        setPhase({ kind: 'disabled' }); // valid_age=false → server disabled the account
      }
      return;
    }
    if (result.code === 'account_disabled') {
      setPhase({ kind: 'disabled' });
      return;
    }
    setPhase({ kind: 'error', message: errorCopyFor(result.code) });
  };

  // <output> is the native status live-region (implicit role=status) — preferred over
  // role="status" on a <p> per Sonar S6819, and semantically apt: each is the RESULT of
  // the user's verification action.
  if (phase.kind === 'submitting') {
    return <output className="nsfw-gate__status">Submitting age verification…</output>;
  }

  if (phase.kind === 'unlocked') {
    return (
      <output className="nsfw-gate__status nsfw-gate__status--ok">
        Age verified. NSFW content access is now enabled.
      </output>
    );
  }

  if (phase.kind === 'verifiedLocked') {
    return (
      <output className="nsfw-gate__status">
        Age verified. NSFW content access requires you to be 18 or older, so it remains locked.
      </output>
    );
  }

  if (phase.kind === 'disabled') {
    // The actionable appeal link wires up when #1646 (re-enablement/appeal) lands; until
    // then this is text guidance (no invented route).
    return (
      <div className="nsfw-gate__disabled" role="alert">
        <p>
          Your account has been disabled because the date of birth you provided is below our minimum
          age requirement.
        </p>
        <p>If you believe this is a mistake, please contact support to request a review.</p>
      </div>
    );
  }

  if (phase.kind === 'confirm') {
    const { year: cy, month: cm, day: cd } = phase.signal;
    const pretty = `${cy}-${String(cm).padStart(2, '0')}-${String(cd).padStart(2, '0')}`;
    return (
      // Native <fieldset> groups the confirm step (preferred over role="group" per Sonar
      // S6819). Focus is moved here once on entering the confirm phase by the effect above.
      <fieldset
        className="nsfw-gate__confirm"
        aria-label="Confirm your date of birth"
        tabIndex={-1}
        ref={confirmRef}
      >
        <p>
          You entered <strong>{pretty}</strong>. Submit this date of birth for age verification?
        </p>
        <div className="nsfw-gate__actions">
          <button type="button" className="btn-primary" onClick={() => handleConfirm(phase.signal)}>
            Submit
          </button>
          <button
            type="button"
            className="settings-btn-secondary"
            onClick={() => setPhase({ kind: 'form' })}
          >
            Cancel
          </button>
        </div>
      </fieldset>
    );
  }

  // phase.kind === 'form' | 'error'
  return (
    <div className="nsfw-gate">
      <p className="settings-section-description">
        To access NSFW content, verify your age by entering your date of birth. Your date of birth
        is used only to compute your age on this device — it is never saved or sent anywhere. Only
        the verified result is submitted.
      </p>

      {phase.kind === 'error' && (
        <p className="nsfw-gate__error" role="alert">
          {phase.message}
        </p>
      )}

      <div className="nsfw-gate__fields">
        <div className="form-group">
          <label className="form-label" htmlFor="nsfw-dob-year">
            Year
          </label>
          <input
            id="nsfw-dob-year"
            type="number"
            inputMode="numeric"
            className="nsfw-gate__input"
            value={year}
            min={1900}
            max={now.getUTCFullYear()}
            onChange={(e) => setYear(e.target.value)}
          />
        </div>
        <div className="form-group">
          <label className="form-label" htmlFor="nsfw-dob-month">
            Month
          </label>
          <input
            id="nsfw-dob-month"
            type="number"
            inputMode="numeric"
            className="nsfw-gate__input"
            value={month}
            min={1}
            max={12}
            onChange={(e) => setMonth(e.target.value)}
          />
        </div>
        <div className="form-group">
          <label className="form-label" htmlFor="nsfw-dob-day">
            Day
          </label>
          <input
            id="nsfw-dob-day"
            type="number"
            inputMode="numeric"
            className="nsfw-gate__input"
            value={day}
            min={1}
            max={31}
            onChange={(e) => setDay(e.target.value)}
          />
        </div>
      </div>

      <button type="button" className="btn-primary" onClick={handleReview} disabled={!signal}>
        Verify age
      </button>
    </div>
  );
};

export default NsfwContentGate;
