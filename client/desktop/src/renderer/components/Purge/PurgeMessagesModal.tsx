import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import PurgeRangePicker from './PurgeRangePicker';
import PurgeResult from './PurgeResult';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import StepUpFields, { stepUpFieldErrors, stepUpRefusedField } from './StepUpFields';
import { PURGE_RANGE_PHRASES, type PurgeRange } from '../../constants/purgeRanges';
import {
  isSoftLockChallengeResult,
  isStepUpPurgeResult,
  purgeMessages,
  type PurgeArgs,
  type PurgeContext,
  type PurgeResult as PurgeOutcome,
  type SoftLockChallengeView,
  type StepUpPurgeResult,
  type TerminalPurgeResult,
} from '../../services/messaging/purgeApi';
import { usePrivacyStore } from '../../stores/ui/privacyStore';
import { useSettingsNavStore } from '../../stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '../../stores/ui/settingsOverlayStore';
import './purgeMessages.css';

interface PurgeMessagesModalProps {
  context: PurgeContext;
  isOpen: boolean;
  onClose: () => void;
  scopeId: string;
  scopeName: string;
  /** Group DM only — the copy differs because the backend behaviour differs. */
  role?: 'admin' | 'member';
  /**
   * The actor holds ManageOwnMessages but not ManageAllMessages, so the server
   * self-scopes the purge to their own messages. Such an actor is never denied
   * the entry point — only the copy and the resulting scope narrow (copy deck
   * §1, spec §4.2).
   */
  selfScopeOnly?: boolean;
}

/**
 * Dialog titles. Constant across every stage: ui/Modal binds the title to
 * aria-labelledby, so mutating it renames the dialog mid-interaction
 * (WCAG 4.1.2 / 3.2.2). Copy deck §1.
 */
const TITLES: Record<PurgeContext, string> = {
  channel: 'Purge Messages',
  server: 'Purge Server Messages',
  dm: 'Purge Messages',
  group: 'Purge Messages',
};

const SERVER_RANGE_HELPER = 'Channels you cannot moderate will be skipped.';

/**
 * apiFetch rejects — rather than resolving with a status — when no response
 * arrives: offline, DNS, TLS, but equally a connection dropped after the server
 * received the request and committed batches. It keeps its own kind so the copy
 * can name the cause, but it may never claim that nothing was purged.
 */
const TRANSPORT_FAILURE: TerminalPurgeResult = { kind: 'networkError' };

/**
 * configure → result, or configure → stepup → result for DM/group. A channel or
 * server SELF-purge that trips the delete-rate soft-lock (#3455) goes
 * configure → softlock → result.
 */
type Stage = 'configure' | 'stepup' | 'softlock' | 'result';

/**
 * The scope echo, split around the bolded scope name. Qualitative by
 * construction: no count-preview endpoint exists, and a count approximated
 * from the loaded window is wrong whenever history exceeds it. Copy deck §1/§2.
 */
function scopeSentence(
  context: PurgeContext,
  scopeName: string,
  phrase: string,
  selfScopeOnly: boolean
): { lead: string; name: string; tail: string } {
  // A ManageOwn-only actor purges only their own messages, so the range phrase
  // reads "your messages" where a moderator's reads "all messages" (copy deck §1).
  const scopedPhrase = selfScopeOnly ? phrase.replace(/^all messages/, 'your messages') : phrase;
  const lead = `Are you sure you want to purge ${scopedPhrase} `;
  switch (context) {
    case 'channel':
      return {
        lead: `${lead}in `,
        name: `#${scopeName}`,
        tail: '? This action cannot be undone. The channel itself will stay.',
      };
    case 'server':
      return {
        // Server context names the moderated subset out loud: a ManageOwn-only
        // actor's purge reaches only channels they moderate (copy deck §1).
        lead: selfScopeOnly ? `${lead}in channels you moderate across ` : `${lead}across `,
        name: scopeName,
        tail: `? This action cannot be undone. ${SERVER_RANGE_HELPER}`,
      };
    // DM and group scope is stated by scopeNote, visible before a range is
    // chosen, so the confirmation sentence does not repeat it.
    case 'dm':
      return {
        lead: `${lead}in your conversation with `,
        name: scopeName,
        tail: '? This action cannot be undone.',
      };
    case 'group':
      return {
        lead: `${lead}in `,
        name: scopeName,
        tail: '? This action cannot be undone.',
      };
  }
}

interface SoftLockStageProps {
  softLock: SoftLockChallengeView;
  /** Remounts the MFA prompt empty after a spent code (#3466). */
  promptKey: number;
  purpose: StepUpPurpose;
  busy: boolean;
  canSubmit: boolean;
  password: string;
  onPasswordChange: (value: string) => void;
  onCodeChange: (value: string) => void;
  onSubmit: (e: React.SubmitEvent<HTMLFormElement>) => void;
  onCancel: () => void;
  headingRef: React.Ref<HTMLHeadingElement>;
}

/** The delete-rate soft-lock challenge (#3455): the purge again, with a factor. */
const SoftLockStage: React.FC<SoftLockStageProps> = ({
  softLock,
  promptKey,
  purpose,
  busy,
  canSubmit,
  password,
  onPasswordChange,
  onCodeChange,
  onSubmit,
  onCancel,
  headingRef,
}) => (
  <div className="purge-modal__stepup">
    <h3 className="purge-modal__stage-heading" tabIndex={-1} ref={headingRef}>
      Confirm it is you
    </h3>
    <form onSubmit={onSubmit}>
      <fieldset disabled={busy} className="purge-modal__form">
        <p className="purge-modal__stepup-body">
          You&apos;ve deleted several messages quickly. Confirm it&apos;s you to keep going.
        </p>

        {softLock.view === 'confirm' ? (
          <div className="purge-modal__softlock-prompt">
            <MFAVerifyPrompt
              key={promptKey}
              methods={softLock.methods}
              purpose={purpose}
              onVerify={onCodeChange}
              onCodeChange={onCodeChange}
              disabled={busy}
              error={softLock.error}
            />
          </div>
        ) : (
          <div className="purge-modal__field">
            <label htmlFor="purge-softlock-password">Password</label>
            <input
              id="purge-softlock-password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(ev) => onPasswordChange(ev.target.value)}
              aria-invalid={softLock.error !== undefined || undefined}
              aria-describedby={
                softLock.error === undefined ? undefined : 'purge-softlock-password-error'
              }
            />
            {softLock.error !== undefined && (
              <p
                className="purge-modal__softlock-error"
                id="purge-softlock-password-error"
                role="alert"
              >
                {softLock.error}
              </p>
            )}
          </div>
        )}

        <div className="purge-modal__actions">
          <button type="button" className="purge-modal__cancel" onClick={onCancel}>
            Cancel
          </button>
          <button type="submit" className="purge-modal__confirm" disabled={!canSubmit}>
            {busy ? (
              <>
                <LoadingSpinner size="small" inline /> Purging...
              </>
            ) : (
              'Confirm and Purge'
            )}
          </button>
        </div>
      </fieldset>
    </form>
  </div>
);

/**
 * What a DM or group purge actually deletes, shown from the first paint: a
 * 1:1 or group-member purge deletes only the actor's own messages and merely
 * hides everyone else's (#1352 spec §5), which users otherwise read as a purge
 * that "missed" messages. Channel and server copy already says it in the
 * sentence.
 */
function scopeNote(
  context: PurgeContext,
  role: 'admin' | 'member',
  scopeName: string
): string | null {
  switch (context) {
    case 'dm':
      return (
        'Your own messages are removed for both of you. ' +
        `Messages from ${scopeName} stay for them and are hidden only for you.`
      );
    case 'group':
      return role === 'admin'
        ? 'As a group admin, you remove messages for everyone in the group.'
        : 'Your own messages are removed for everyone. ' +
            'Messages from others stay for them and are hidden only for you.';
    default:
      return null;
  }
}

const PurgeMessagesModal: React.FC<PurgeMessagesModalProps> = ({
  context,
  isOpen,
  onClose,
  scopeId,
  scopeName,
  role = 'member',
  selfScopeOnly = false,
}) => {
  const [range, setRange] = useState<PurgeRange | null>(null);
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState(false);
  const [stage, setStage] = useState<Stage>('configure');
  const [result, setResult] = useState<TerminalPurgeResult | null>(null);
  // Wire secrets. Component-local state only — never a store, never a log,
  // never echoed into error copy ([internal]rules/observability.md).
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [stepUp, setStepUp] = useState<StepUpPurgeResult | null>(null);
  // #3455: the soft-lock challenge on screen, and the key that remounts the MFA
  // prompt empty after an `Invalid MFA code` (#3466 — a code is spent by a try).
  const [softLock, setSoftLock] = useState<SoftLockChallengeView | null>(null);
  const [softLockKey, setSoftLockKey] = useState(0);
  const firstRangeRef = useRef<HTMLSelectElement>(null);
  const stageHeadingRef = useRef<HTMLHeadingElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const codeRef = useRef<HTMLInputElement>(null);
  // Bumped on every open and close. A request carries the value it started
  // under, and its outcome is dropped if the dialog closed or reopened while
  // it was in flight: the mint plus the purge is two round trips, long enough
  // to write a stale outcome into the next open (#3509 frontend review).
  const openGenerationRef = useRef(0);

  const requestFocus = useSettingsNavStore((s) => s.requestFocus);
  const openSettings = useSettingsOverlayStore((s) => s.openSettings);
  const requireAuthBeforePurge = usePrivacyStore((s) => s.settings.requireAuthBeforePurge);

  // Fail closed on unknown: internal/dm/purge.go reads the same setting that
  // way, and a server too old to expose the field omits it entirely. Guessing
  // "off" would send a credential-less request that can only be refused.
  const stepUpRequired =
    (context === 'dm' || context === 'group') && requireAuthBeforePurge !== false;

  // Friction tracks irreversibility, not org scope — purging all time in a DM
  // earns the same pause as a server (spec R-11).
  const needsTypedConfirm = range === 'all' || context === 'server';
  const canConfirm = range !== null && (!needsTypedConfirm || typed === 'PURGE') && !busy;

  // The password field stays up through an `mfa_required` refusal. The seam
  // verifies the password FIRST, so that refusal is exactly what a correct
  // password with no code receives — hiding the field on it made the next
  // submit send no password, drew `password_required`, and burned a second
  // attempt from the budget the purge itself spends. Same fix, same reason, as
  // PurgeFenceStepUpDialog (#2792); the server's arms drive the copy.
  const showPassword = true;
  const canSubmitStepUp = !busy && (password !== '' || code !== '');
  // One route, one purpose: the WebAuthn token this prompt mints is spendable on
  // no other route (see MFAVerifyPrompt's `purpose`).
  const softLockPurpose: StepUpPurpose =
    context === 'server' ? 'messages.server_purge' : 'messages.channel_purge';
  const canSubmitSoftLock =
    !busy && (softLock?.view === 'password' ? password !== '' : code !== '');

  // A stage change moves focus to the stage heading. The dialog title stays put:
  // ui/Modal binds it to aria-labelledby, so renaming it mid-interaction renames
  // the dialog (WCAG 4.1.2 / 3.2.2). So does a soft-lock challenge that changes
  // factor without leaving the stage — the mint naming MFA after the password
  // was sent (#3509 review): the focused field unmounts, and without this focus
  // would fall out of the dialog. A code prompt then takes focus for its first
  // digit, which mounts after this runs.
  const softLockView = softLock?.view;
  useLayoutEffect(() => {
    if (stage === 'stepup' || stage === 'softlock') stageHeadingRef.current?.focus();
  }, [stage, softLockView]);

  // Unmounting would drop this state for free, but ChannelSettingsModal and
  // GroupInfoPanel render the dialog unconditionally behind a boolean `isOpen`,
  // so it stays mounted across close: the credentials would carry into the next
  // open, and so would a satisfied typed confirmation — "All messages" + PURGE
  // survives a cancel, and canConfirm is true on the reopened first paint. The
  // whole machine resets, not just the wire secrets.
  useEffect(() => {
    openGenerationRef.current += 1;
    if (isOpen) return;
    // The rule guards against wasted renders. Here the dialog is already closed, so the
    // extra render is of nothing, and resetting the moment it closes outranks that.
    // Moving the reset into onClose would miss a parent that flips isOpen without
    // routing through it, which is the case this guard exists for.
    /* eslint-disable @eslint-react/set-state-in-effect -- credential hygiene + confirm-friction reset, see above */
    setPassword('');
    setCode('');
    setBusy(false);
    setStage('configure');
    setResult(null);
    setStepUp(null);
    setSoftLock(null);
    setSoftLockKey(0);
    setRange(null);
    setTyped('');
    /* eslint-enable @eslint-react/set-state-in-effect -- reset block ends here */
  }, [isOpen]);

  // A rejected factor returns focus to the field that owns it. Keyed on the
  // result object, so a second wrong attempt of the same kind re-focuses too.
  useLayoutEffect(() => {
    const field = stepUpRefusedField(stepUp);
    if (field === 'password') passwordRef.current?.focus();
    else if (field === 'code') codeRef.current?.focus();
  }, [stepUp]);

  // The soft-lock gate runs before any purge batch, so nothing was purged.
  // Any factor that travelled with the refused request may be spent, so the
  // code is always dropped. The password never reaches here: the submit
  // dropped it as it sent it to the mint (#3509 frontend review). The prompt
  // remounts empty only when it STAYS on the confirm view with a per-attempt
  // error (#3466) — never on the first challenge.
  const showSoftLockChallenge = (next: SoftLockChallengeView) => {
    if (softLock?.view === 'confirm' && next.view === 'confirm' && next.error !== undefined) {
      setSoftLockKey((k) => k + 1);
    }
    setCode('');
    setSoftLock(next);
    setStage('softlock');
  };

  // The credential challenge is a stage, not an outcome. Drop only the
  // factor the server rejected, so a wrong password does not cost the user
  // a fresh code they already typed.
  const showStepUpChallenge = (outcome: StepUpPurgeResult) => {
    if (outcome.kind === 'invalidPassword') setPassword('');
    if (outcome.kind === 'invalidMfaCode') setCode('');
    // A refusal of the configure-stage submit carried no credentials, so it
    // is the challenge that opens the stage, not a mistake: it names no field
    // and focus stays on the stage heading. `stage` is the one the refused
    // request was sent from — this closure belongs to that render.
    const opensStage =
      stage === 'configure' &&
      (outcome.kind === 'passwordRequired' || outcome.kind === 'mfaRequired');
    setStepUp(opensStage ? null : outcome);
    setStage('stepup');
  };

  const applyOutcome = (outcome: PurgeOutcome) => {
    if (isSoftLockChallengeResult(outcome)) {
      showSoftLockChallenge(outcome.view);
      return;
    }
    if (isStepUpPurgeResult(outcome)) {
      showStepUpChallenge(outcome);
      return;
    }
    if (
      outcome.kind === 'success' ||
      outcome.kind === 'partial' ||
      outcome.kind === 'networkError'
    ) {
      // The actor cleans up from their own request, not from the echo. A
      // channel_purged/dm_purged broadcast is subscription-scoped, so the very
      // person who pressed the button may never receive one — waiting on it
      // leaves them reading purged content. The two uncertain outcomes dispatch
      // for a different reason: a 500 can arrive after batches committed and a
      // transport rejection cannot prove the request never landed, while no
      // event is emitted on any DM error path — so the refetch is the only way
      // back to the truth, and it matters most exactly when the outcome is
      // unknown.
      //
      // The server context's scopeId is a SERVER id, which no mounted channel
      // can match, so it stays null: `useMessageFetch` reads a null scope as
      // "refetch whatever is mounted". It travels alongside an explicit
      // `serverId` because the two carry different instructions — null says
      // which scope to REFETCH, serverId says which scopes to CLEAR. Relying on
      // the `server_purged` echo for the clear would fail in exactly the case
      // that needs it most: a transport rejection means the WebSocket is
      // plausibly down too, so the echo may never arrive and the actor's other
      // known channels of that server would keep serving purged plaintext.
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', {
          detail: context === 'server' ? { scopeId: null, serverId: scopeId } : { scopeId },
        })
      );
    }
    // Nothing beyond this point needs the credentials; drop them before the
    // result stage renders.
    setPassword('');
    setCode('');
    setResult(outcome);
    setStage('result');
  };

  // Runs one purge request and applies its outcome, unless the dialog closed
  // or reopened while it was in flight. Clearing `busy` matters: it disables
  // the fieldset that owns Cancel, and `dismissable={!busy}` removes the close
  // button and gates both Escape and the backdrop click. A stale request leaves
  // `busy` alone, because the close already reset it for the new open.
  const runPurge = async (args: PurgeArgs) => {
    const generation = openGenerationRef.current;
    setBusy(true);
    let outcome: PurgeOutcome;
    try {
      outcome = await purgeMessages(args);
    } catch {
      outcome = TRANSPORT_FAILURE;
    }
    if (generation !== openGenerationRef.current) return;
    applyOutcome(outcome);
    setBusy(false);
  };

  const handleConfirm = async () => {
    if (!canConfirm || range === null) return;
    if (stepUpRequired) {
      // Proactive determination, sequential disclosure (spec R-10): collect the
      // factors first rather than spending a rate-limited request discovering
      // that they are needed.
      setStage('stepup');
      return;
    }
    await runPurge({ context, scopeId, range });
  };

  const handleStepUpSubmit = async () => {
    if (!canSubmitStepUp || range === null) return;
    // Single-shot: whichever factors the actor has travel in the same request.
    // Probing for the requirement costs a call against the very purge budget the
    // user is trying to spend on the purge itself (spec R-7).
    await runPurge({
      context,
      scopeId,
      range,
      currentPassword: password || undefined,
      mfaCode: code || undefined,
    });
  };

  const handleSoftLockSubmit = async (e: React.SubmitEvent<HTMLFormElement>) => {
    e.preventDefault();
    if (!canSubmitSoftLock || range === null || softLock === null) return;
    const factor = softLock.view === 'password' ? { currentPassword: password } : { mfaCode: code };
    // The previous attempt's error leaves as this one starts, so an identical
    // refusal is announced again; the password leaves component state now,
    // because this request sends it to the mint (#3509 frontend review).
    setSoftLock(
      softLock.view === 'password'
        ? { view: 'password' }
        : { view: 'confirm', methods: softLock.methods }
    );
    if (softLock.view === 'password') setPassword('');
    // The same purge, re-sent with the factor the challenge asked for.
    await runPurge({ context, scopeId, range, ...factor, softLockPrior: softLock });
  };

  const handleGoToPrivacy = () => {
    // The focus request is consumed by SettingsPage's effect, which only runs
    // while SettingsPage is mounted — and this card is reachable only from a
    // DM/group entry point, with Settings closed. So open the overlay first,
    // exactly as utils/ui/openProfilePage.ts and openSubscriptionPage.ts do.
    // 'privacy' is the Privacy & Security pane's id in the SettingsSection
    // union; the control id is the toggle Task 7 adds, and the deck's dead-end
    // copy names that toggle verbatim so the user finds the words they were told.
    openSettings('app');
    requestFocus('privacy', 'requireAuthBeforePurge');
    onClose();
  };

  const sentence =
    range === null
      ? null
      : scopeSentence(context, scopeName, PURGE_RANGE_PHRASES[range], selfScopeOnly);
  const note = scopeNote(context, role, scopeName);

  return (
    <Modal
      isOpen={isOpen}
      onClose={onClose}
      title={TITLES[context]}
      width="medium"
      dismissable={!busy}
      initialFocusRef={firstRangeRef}
    >
      <div className="purge-modal__body">
        {stage === 'result' && result !== null && (
          <PurgeResult context={context} result={result} onDone={onClose} />
        )}

        {stage === 'stepup' && (
          <div className="purge-modal__stepup">
            {/* Focus target for the stage change. The dialog keeps its own
                title — this heading is what announces the new stage. */}
            <h3 className="purge-modal__stage-heading" tabIndex={-1} ref={stageHeadingRef}>
              Confirm it is you
            </h3>

            {stepUp?.kind === 'stepUpImpossible' ? (
              <div className="purge-modal__form">
                {/* No password and no MFA: there is nothing the user could type
                    that would work, so the stage offers no retryable field. */}
                <p className="purge-modal__deadend">
                  Your account signs in without a password, so we cannot confirm your identity here.
                  Turn off <strong>Require authentication before purging</strong> in Privacy &amp;
                  Security, then try again.
                </p>
                <div className="purge-modal__actions">
                  <button type="button" className="purge-modal__cancel" onClick={onClose}>
                    Cancel
                  </button>
                  <button
                    type="button"
                    className="purge-modal__confirm"
                    onClick={handleGoToPrivacy}
                  >
                    Go to Privacy &amp; Security
                  </button>
                </div>
              </div>
            ) : (
              <fieldset disabled={busy} className="purge-modal__form">
                <p className="purge-modal__stepup-body">
                  Purging messages is permanent, so we ask you to confirm your identity first.
                </p>

                <StepUpFields
                  showPassword={showPassword}
                  password={password}
                  onPasswordChange={setPassword}
                  code={code}
                  onCodeChange={setCode}
                  errors={stepUpFieldErrors(stepUp)}
                  passwordRef={passwordRef}
                  codeRef={codeRef}
                />

                <div className="purge-modal__actions">
                  <button type="button" className="purge-modal__cancel" onClick={onClose}>
                    Cancel
                  </button>
                  <button
                    type="button"
                    className="purge-modal__confirm"
                    disabled={!canSubmitStepUp}
                    onClick={handleStepUpSubmit}
                  >
                    {busy ? (
                      <>
                        <LoadingSpinner size="small" inline /> Purging...
                      </>
                    ) : (
                      'Confirm and Purge'
                    )}
                  </button>
                </div>
              </fieldset>
            )}
          </div>
        )}

        {stage === 'softlock' && softLock !== null && (
          <SoftLockStage
            softLock={softLock}
            promptKey={softLockKey}
            purpose={softLockPurpose}
            busy={busy}
            canSubmit={canSubmitSoftLock}
            password={password}
            onPasswordChange={setPassword}
            onCodeChange={setCode}
            onSubmit={handleSoftLockSubmit}
            onCancel={onClose}
            headingRef={stageHeadingRef}
          />
        )}

        {stage === 'configure' && (
          <fieldset disabled={busy} className="purge-modal__form">
            <PurgeRangePicker
              value={range}
              onChange={setRange}
              firstOptionRef={firstRangeRef}
              helper={context === 'server' ? SERVER_RANGE_HELPER : undefined}
            />

            {note !== null && <p className="purge-modal__scope-note">{note}</p>}

            {sentence !== null && (
              <p className="purge-modal__scope">
                <svg
                  className="purge-modal__scope-icon"
                  width="20"
                  height="20"
                  viewBox="0 0 24 24"
                  fill="none"
                  aria-hidden="true"
                >
                  <path
                    d="M12 3 1.5 21h21L12 3Zm0 6v6m0 3h.01"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
                <span>
                  {sentence.lead}
                  <strong>{sentence.name}</strong>
                  {sentence.tail}
                </span>
              </p>
            )}

            {needsTypedConfirm && (
              <label className="purge-modal__typed">
                <span>Type PURGE to confirm.</span>
                <input
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  placeholder="PURGE"
                  autoComplete="off"
                />
              </label>
            )}

            <div className="purge-modal__actions">
              <button type="button" className="purge-modal__cancel" onClick={onClose}>
                Cancel
              </button>
              <button
                type="button"
                className="purge-modal__confirm"
                disabled={!canConfirm}
                onClick={handleConfirm}
              >
                {busy ? (
                  <>
                    <LoadingSpinner size="small" inline /> Purging...
                  </>
                ) : (
                  'Purge Messages'
                )}
              </button>
            </div>
          </fieldset>
        )}
      </div>
    </Modal>
  );
};

export default PurgeMessagesModal;
