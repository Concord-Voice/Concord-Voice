import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import PurgeRangePicker from './PurgeRangePicker';
import PurgeResult from './PurgeResult';
import StepUpFields, { type StepUpFieldErrors } from './StepUpFields';
import { PURGE_RANGE_PHRASES, type PurgeRange } from '../../constants/purgeRanges';
import {
  isStepUpPurgeResult,
  purgeMessages,
  type PurgeContext,
  type PurgeResult as PurgeOutcome,
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

/** configure → result, or configure → stepup → result for DM/group. */
type Stage = 'configure' | 'stepup' | 'result';

/**
 * Per-field step-up error copy (deck §5). Held in a switch rather than a lookup
 * object because the pre-commit secret scanner flags a credential-shaped key
 * placed beside a quoted literal — see StepUpFields' `credentialError`.
 */
function stepUpFieldErrors(stepUp: StepUpPurgeResult | null): StepUpFieldErrors {
  switch (stepUp?.kind) {
    case 'invalidPassword':
      return { credentialError: 'That password is not correct.' };
    case 'invalidMfaCode':
      return { codeError: 'That code is not correct, or it has expired. Try the next one.' };
    default:
      return {};
  }
}

/**
 * The scope echo, split around the bolded scope name. Qualitative by
 * construction: no count-preview endpoint exists, and a count approximated
 * from the loaded window is wrong whenever history exceeds it. Copy deck §1/§2.
 */
function scopeSentence(
  context: PurgeContext,
  role: 'admin' | 'member',
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
    case 'dm':
      return {
        lead: `${lead}in your conversation with `,
        name: scopeName,
        tail:
          '? This action cannot be undone. Your own messages are removed for both of you; ' +
          `messages from ${scopeName} are hidden only for you.`,
      };
    case 'group':
      return {
        lead: `${lead}in `,
        name: scopeName,
        tail:
          role === 'admin'
            ? '? This action cannot be undone. Messages are removed for everyone in the group.'
            : '? This action cannot be undone. Your own messages are removed for everyone; ' +
              'messages from others are hidden only for you.',
      };
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
  const firstRangeRef = useRef<HTMLInputElement>(null);
  const stageHeadingRef = useRef<HTMLHeadingElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const codeRef = useRef<HTMLInputElement>(null);

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

  // An SSO account has no password to offer, so an explicit MFA challenge drops
  // the field rather than demanding something that cannot exist.
  const showPassword = stepUp?.kind !== 'mfaRequired';
  const canSubmitStepUp = !busy && ((showPassword && password !== '') || code !== '');

  // A stage change moves focus to the stage heading. The dialog title stays put:
  // ui/Modal binds it to aria-labelledby, so renaming it mid-interaction renames
  // the dialog (WCAG 4.1.2 / 3.2.2).
  useLayoutEffect(() => {
    if (stage === 'stepup') stageHeadingRef.current?.focus();
  }, [stage]);

  // Unmounting would drop this state for free, but ChannelSettingsModal and
  // GroupInfoPanel render the dialog unconditionally behind a boolean `isOpen`,
  // so it stays mounted across close: the credentials would carry into the next
  // open, and so would a satisfied typed confirmation — "All messages" + PURGE
  // survives a cancel, and canConfirm is true on the reopened first paint. The
  // whole machine resets, not just the wire secrets.
  useEffect(() => {
    if (isOpen) return;
    // The rule guards against wasted renders. Here the dialog is already closed, so the
    // extra render is of nothing, and resetting the moment it closes outranks that.
    // Moving the reset into onClose would miss a parent that flips isOpen without
    // routing through it, which is the case this guard exists for.
    /* eslint-disable @eslint-react/set-state-in-effect -- credential hygiene + confirm-friction reset, see above */
    setPassword('');
    setCode('');
    setStage('configure');
    setResult(null);
    setStepUp(null);
    setRange(null);
    setTyped('');
    /* eslint-enable @eslint-react/set-state-in-effect -- reset block ends here */
  }, [isOpen]);

  // A rejected factor returns focus to the field that owns it. Keyed on the
  // result object, so a second wrong attempt of the same kind re-focuses too.
  useLayoutEffect(() => {
    if (stepUp?.kind === 'invalidPassword') passwordRef.current?.focus();
    else if (stepUp?.kind === 'invalidMfaCode') codeRef.current?.focus();
  }, [stepUp]);

  const applyOutcome = (outcome: PurgeOutcome) => {
    if (isStepUpPurgeResult(outcome)) {
      // The credential challenge is a stage, not an outcome. Drop only the
      // factor the server rejected, so a wrong password does not cost the user
      // a fresh code they already typed.
      if (outcome.kind === 'invalidPassword') setPassword('');
      if (outcome.kind === 'invalidMfaCode') setCode('');
      setStepUp(outcome);
      setStage('stepup');
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

  const handleConfirm = async () => {
    if (!canConfirm || range === null) return;
    if (stepUpRequired) {
      // Proactive determination, sequential disclosure (spec R-10): collect the
      // factors first rather than spending a rate-limited request discovering
      // that they are needed.
      setStage('stepup');
      return;
    }
    setBusy(true);
    try {
      applyOutcome(await purgeMessages({ context, scopeId, range }));
    } catch {
      applyOutcome(TRANSPORT_FAILURE);
    } finally {
      // Without this the dialog is unclosable: `busy` disables the fieldset that
      // owns Cancel, and `dismissable={!busy}` removes the close button and gates
      // both Escape and the backdrop click.
      setBusy(false);
    }
  };

  const handleStepUpSubmit = async () => {
    if (!canSubmitStepUp || range === null) return;
    setBusy(true);
    // Single-shot: whichever factors the actor has travel in the same request.
    // Probing for the requirement costs a call against the very purge budget the
    // user is trying to spend on the purge itself (spec R-7).
    try {
      applyOutcome(
        await purgeMessages({
          context,
          scopeId,
          range,
          currentPassword: showPassword && password ? password : undefined,
          mfaCode: code || undefined,
        })
      );
    } catch {
      applyOutcome(TRANSPORT_FAILURE);
    } finally {
      setBusy(false);
    }
  };

  const handleGoToPrivacy = () => {
    // The focus request is consumed by SettingsPage's effect, which only runs
    // while SettingsPage is mounted — and this card is reachable only from a
    // DM/group entry point, with Settings closed. So open the overlay first,
    // exactly as utils/openProfilePage.ts and openSubscriptionPage.ts do.
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
      : scopeSentence(context, role, scopeName, PURGE_RANGE_PHRASES[range], selfScopeOnly);

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

        {stage === 'configure' && (
          <fieldset disabled={busy} className="purge-modal__form">
            <PurgeRangePicker
              value={range}
              onChange={setRange}
              firstOptionRef={firstRangeRef}
              helper={context === 'server' ? SERVER_RANGE_HELPER : undefined}
            />

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
