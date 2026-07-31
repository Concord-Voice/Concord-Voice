import { useState, useEffect, useRef } from 'react';
import { submitSignedAgeClaim } from '../../services/ageClaim/ageClaimService';
import { type AgeSignal } from '../../services/ageClaim/evaluateAge';
import { useAgeStatus } from '../../hooks/useAgeStatus';
import { useSettingsStore } from '../../stores/settingsStore';
import ToggleSwitch from './ToggleSwitch';
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

/**
 * The gate's copy is driven by two independent inputs — the local `phase` and the
 * server-backed `status` — which can each resolve to the same user-facing outcome.
 * Resolving an outcome token first and mapping it to copy once keeps every string
 * defined in exactly one place (SonarQube S1871).
 */
type GateOutcome = 'underage' | 'eligible' | 'ineligible' | 'submitting' | 'loading' | 'unverified';

const GATE_COPY: Record<GateOutcome, { status: string; reason?: string }> = {
  underage: { status: "Age verification did not meet Concord's minimum age requirement." },
  eligible: { status: 'Age verified · Eligible for NSFW content' },
  ineligible: {
    status: 'Age verified · Not eligible for NSFW content',
    reason: 'You must be 18 or older to enable this preference.',
  },
  submitting: { status: 'Submitting age verification…' },
  loading: { status: 'Checking your verification status…' },
  unverified: { status: 'Verify your age before you can change this preference.' },
};

const NsfwContentGate = () => {
  const status = useAgeStatus();
  const allowNsfwContent = useSettingsStore((state) => state.allowNsfwContent);
  const setAllowNsfwContent = useSettingsStore((state) => state.setAllowNsfwContent);
  const [year, setYear] = useState('');
  const [month, setMonth] = useState('');
  const [day, setDay] = useState('');
  const [phase, setPhase] = useState<Phase>({ kind: 'form' });
  // Captured once at mount (state initializer — keeps render pure). Bounds the
  // future-date check + the year input's max; a settings form is short-lived so a
  // mount-time "now" is fine.
  const [now] = useState(() => new Date());
  const confirmRef = useRef<HTMLFieldSetElement>(null);
  const statusRef = useRef<HTMLOutputElement>(null);

  // Move focus to the confirm group EXACTLY ONCE when entering the confirm phase —
  // announces the step to screen readers and keeps keyboard focus off document body when
  // the prior step unmounts. Keyed on phase.kind (not an inline ref callback) so a
  // re-render within the confirm phase does NOT yank focus back from a button the user
  // tabbed to (Gitar/Copilot/@code-reviewer review). Targets the non-actionable container,
  // never Submit, so a stray Enter cannot fire the irreversible submit.
  useEffect(() => {
    if (phase.kind === 'confirm') confirmRef.current?.focus();
  }, [phase.kind]);

  useEffect(() => {
    if (phase.kind === 'submitting') statusRef.current?.focus();
  }, [phase.kind]);

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

  const locallyAdult = phase.kind === 'unlocked';
  const durablyAdult =
    phase.kind === 'form' &&
    status.state === 'verified' &&
    status.validAge === true &&
    status.nsfwAuth === true;
  const nsfwEligible = locallyAdult || durablyAdult;
  const checked = nsfwEligible && allowNsfwContent;

  // The store setter is unconditional and its value is persisted to disk, so the
  // eligibility conjunction must be enforced somewhere that actually writes. Guarding
  // the handler (rather than relying on the removed `disabled` attribute) keeps an
  // ineligible user from ever setting it.
  const handleToggle = (next: boolean) => {
    if (!nsfwEligible) return;
    setAllowNsfwContent(next);
  };

  // NOTE: deliberately NOT clearing a stored `true` when eligibility is lost. Ineligible
  // includes the transient `status.state === 'loading'` window on every launch, so
  // clearing would silently destroy the user's opt-in each start; and the stored value is
  // intent, which `masks stored intent while ineligible and restores it when eligibility
  // returns` locks as a behaviour. The fail-open risk the reviewers raised is closed at
  // the two write paths instead (the guard above) plus the store-field contract comment.

  // Precedence is unchanged from the original chain: every `phase` kind is resolved
  // before any `status` state is consulted.
  let outcome: GateOutcome;
  if (phase.kind === 'disabled') {
    outcome = 'underage';
  } else if (phase.kind === 'unlocked') {
    outcome = 'eligible';
  } else if (phase.kind === 'verifiedLocked') {
    outcome = 'ineligible';
  } else if (phase.kind === 'submitting') {
    outcome = 'submitting';
  } else if (status.state === 'loading') {
    outcome = 'loading';
  } else if (status.state === 'verified' && status.validAge && status.nsfwAuth) {
    outcome = 'eligible';
  } else if (status.state === 'verified' && status.validAge) {
    outcome = 'ineligible';
  } else if (status.state === 'verified') {
    outcome = 'underage';
  } else {
    outcome = 'unverified';
  }

  const statusCopy = GATE_COPY[outcome].status;
  const reasonCopy = GATE_COPY[outcome].reason ?? '';

  const showDobForm =
    status.state === 'unverified' && (phase.kind === 'form' || phase.kind === 'error');

  return (
    <div className="nsfw-gate">
      <div className="settings-row">
        <div className="settings-row-info">
          <span id="allow-nsfw-content-label" className="settings-row-label">
            Allow NSFW content
          </span>
          <output
            id="allow-nsfw-content-status"
            className="nsfw-gate__status"
            tabIndex={-1}
            ref={statusRef}
          >
            {statusCopy}
          </output>
          <span id="allow-nsfw-content-reason" className="settings-row-hint">
            {reasonCopy}
          </span>
        </div>
        <ToggleSwitch
          id="allow-nsfw-content"
          ariaLabelledBy="allow-nsfw-content-label"
          aria-describedby="allow-nsfw-content-status allow-nsfw-content-reason allow-nsfw-content-future"
          inputRole="switch"
          checked={checked}
          // `aria-disabled`, never the HTML `disabled` attribute — see ToggleSwitch's
          // own contract. `disabled` removes the control from the tab order, so a
          // keyboard/AT user could never reach it to hear the `aria-describedby`
          // explanation of WHY it is unavailable. Enforcement moves to the handler
          // guard below, which is stronger than a DOM attribute anyway.
          aria-disabled={!nsfwEligible}
          onChange={handleToggle}
        />
      </div>
      <p id="allow-nsfw-content-future" className="settings-section-description">
        This saves your preference for future NSFW-marked channels. NSFW-marked channels are not
        available yet.
      </p>

      {showDobForm && (
        <>
          <p className="settings-section-description">
            To access NSFW content, verify your age by entering your date of birth. Your date of
            birth is used only to compute your age on this device — it is never saved or sent
            anywhere. Only the verified result is submitted.
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
        </>
      )}

      {phase.kind === 'confirm' && (
        // Native <fieldset> groups the confirm step (preferred over role="group" per Sonar
        // S6819). Focus is moved here once on entering the confirm phase by the effect above.
        <fieldset
          className="nsfw-gate__confirm"
          aria-label="Confirm your date of birth"
          tabIndex={-1}
          ref={confirmRef}
        >
          <p>
            You entered{' '}
            <strong>
              {phase.signal.year}-{String(phase.signal.month).padStart(2, '0')}-
              {String(phase.signal.day).padStart(2, '0')}
            </strong>
            {'. Submit this date of birth for age verification?'}
          </p>
          <div className="nsfw-gate__actions">
            <button
              type="button"
              className="btn-primary"
              onClick={() => handleConfirm(phase.signal)}
            >
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
      )}

      {phase.kind === 'disabled' && (
        // The actionable appeal link wires up when #1646 (re-enablement/appeal) lands; until
        // then this is text guidance (no invented route).
        <div className="nsfw-gate__disabled" role="alert">
          <p>
            Your account has been disabled because the date of birth you provided is below our
            minimum age requirement.
          </p>
          <p>If you believe this is a mistake, please contact support to request a review.</p>
        </div>
      )}
    </div>
  );
};

export default NsfwContentGate;
