import React, { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import PurgeRangePicker from './PurgeRangePicker';
import PurgeResult from './PurgeResult';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import {
  LEG_ONLY_WITHOUT_MFA,
  useStepUpFactor,
  type StepUpFactor,
  type StepUpFactorRefusal,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '../../hooks/auth/useStepUpFactor';
import { PURGE_RANGE_PHRASES, type PurgeRange } from '../../constants/purgeRanges';
import { softLockSeed, type DeleteRefusalView } from '../../services/messaging/deleteRefusal';
import {
  isSoftLockChallengeResult,
  isStepUpPurgeResult,
  pinModeFor,
  purgeMessages,
  type PinMode,
  type PurgeArgs,
  type PurgeContext,
  type PurgeResult as PurgeOutcome,
  type SoftLockChallengeView,
  type StepUpPurgeResult,
  type TerminalPurgeResult,
} from '../../services/messaging/purgeApi';
import { usePurgeKeepsPinnedAtOpen } from '../../hooks/messaging/usePurgeKeepsPinnedAtOpen';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  isAbortError,
} from '../../services/system/requestContext';
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
 * apiFetch's pre-dispatch fence (an account or server change) raised before
 * anything left, so nothing was purged and there is nothing to report (D7).
 */
const NOT_SENT: PurgeOutcome = { kind: 'notSent' };

/**
 * The prompt a soft-lock retry's code is typed into. `purgeMessages` reads an
 * `Invalid MFA code` refusal as that code's own (a challenge with an error) only
 * when it is told the previous view was a code prompt; the methods are not read.
 * Only the channel and server routes use it.
 */
const CODE_PROMPT: DeleteRefusalView = { view: 'confirm', methods: [] };

/**
 * What the credential stage says when the session is gone before the purge was
 * sent: the read's own `session` refusal, or an account or server change during
 * the activation. The words are the result stage's `sessionExpired` copy
 * (PurgeResult), because it is the same fact.
 */
const SESSION_EXPIRED_MESSAGE =
  'Your session expired. Nothing was purged. Sign in again, then try again.';

/** A step-up result that asks for another try at the credentials instead of ending the stage. */
function isRetryableRefusal(
  outcome: PurgeOutcome
): outcome is Exclude<StepUpPurgeResult, { kind: 'stepUpImpossible' }> {
  return isStepUpPurgeResult(outcome) && outcome.kind !== 'stepUpImpossible';
}

/**
 * A soft-lock challenge the credential fields cannot answer (`refusal: null`)
 * is no challenge: a refused password exchange that is no verdict on the
 * password, such as a rate limit, a server without the endpoint, or a lookup
 * that failed. Typing the password again answers none of them, so the purge
 * ends with the exchange's own words, as any other reply the fields cannot
 * word does. `purgeMessages` classified it from the mint's reason, so nothing
 * here reads the copy.
 */
function endExchangeRefusal(outcome: PurgeOutcome): PurgeOutcome {
  if (!isSoftLockChallengeResult(outcome) || outcome.refusal !== null) return outcome;
  const { view } = outcome;
  return { kind: 'softLockFailed', message: view.view === 'password' ? view.error : undefined };
}

/**
 * The credential hook's reading of a purge result. A refusal of what was
 * entered, or of the purge budget, is a `refusal` (a 429 provably read no
 * code), and so is a soft-lock challenge, whose refusal `purgeMessages`
 * classified. A purge that never left is `aborted` (D7). Any other reply,
 * including a challenge the fields cannot answer, is `answered`: the server
 * replied, so it may have read the code, and the dialog moves to the result
 * stage.
 */
function toSubmitOutcome(outcome: PurgeOutcome): StepUpSubmitOutcome {
  if (isRetryableRefusal(outcome)) return { kind: 'refusal', refusal: outcome };
  switch (outcome.kind) {
    case 'success':
      return { kind: 'success' };
    case 'networkError':
      return { kind: 'transport' };
    case 'rateLimited':
      return { kind: 'refusal', refusal: { kind: 'rateLimited' } };
    case 'softLockChallenge':
      return outcome.refusal === null
        ? { kind: 'answered' }
        : { kind: 'refusal', refusal: outcome.refusal };
    case 'notSent':
      return { kind: 'aborted' };
    default:
      return { kind: 'answered' };
  }
}

/** True when the reply asks the open credential stage for another try instead of ending it. */
function staysInStage(outcome: PurgeOutcome): boolean {
  return isRetryableRefusal(outcome) || isSoftLockChallengeResult(outcome);
}

/** True when the reply asks for credentials: it opens a credential stage. */
function isChallenge(outcome: PurgeOutcome): boolean {
  return isSoftLockChallengeResult(outcome) || isStepUpPurgeResult(outcome);
}

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

interface StepUpStageProps {
  factor: StepUpFactor;
  password: string;
  onPasswordChange: (value: string) => void;
  primaryRef: React.RefObject<HTMLButtonElement | null>;
  headingRef: React.RefObject<HTMLHeadingElement | null>;
  ariaDisabled: boolean;
  onActivate: () => void;
  onCancel: () => void;
  /** What happens to pinned messages, as the configure stage said (#3458). */
  pinRecap: React.ReactNode;
}

interface DmStepUpStageProps extends StepUpStageProps {
  /** The account holds neither a password nor MFA (the purge route's 400). */
  impossible: boolean;
  onGoToPrivacy: () => void;
}

/**
 * The credential fields and footer both purge step-ups share, below their
 * heading: the intro sentence, the picker, and the action-named primary
 * (`aria-disabled`, never natively disabled, so its guard can say what is missing).
 */
const StepUpForm: React.FC<StepUpStageProps & { intro: string }> = ({
  intro,
  factor,
  password,
  onPasswordChange,
  primaryRef,
  headingRef,
  ariaDisabled,
  onActivate,
  onCancel,
  pinRecap,
}) => {
  const busyLabel = factor.phase === 'ceremony' ? 'Waiting…' : 'Purging...';
  return (
    <div className="purge-modal__form">
      <p className="purge-modal__stepup-body">{intro}</p>
      {pinRecap}

      <StepUpCredentials
        factor={factor}
        password={password}
        onPasswordChange={onPasswordChange}
        primaryRef={primaryRef}
        headingRef={headingRef}
        sessionMessage={SESSION_EXPIRED_MESSAGE}
      />

      <div className="purge-modal__actions">
        <button
          type="button"
          className="purge-modal__cancel"
          disabled={factor.phase === 'submitting'}
          onClick={onCancel}
        >
          Cancel
        </button>
        <button
          ref={primaryRef}
          type="button"
          className="purge-modal__confirm"
          aria-disabled={ariaDisabled || undefined}
          onClick={onActivate}
        >
          {factor.phase === 'idle' ? (
            'Confirm and Purge'
          ) : (
            <>
              <LoadingSpinner size="small" inline /> {busyLabel}
            </>
          )}
        </button>
      </div>
    </div>
  );
};

/**
 * A credential stage's frame. The heading is the focus target for the stage
 * change; the dialog keeps its own title, and this is what announces the new stage.
 */
const StepUpShell: React.FC<{
  headingRef: React.RefObject<HTMLHeadingElement | null>;
  children: React.ReactNode;
}> = ({ headingRef, children }) => (
  <div className="purge-modal__stepup">
    <h3 className="purge-modal__stage-heading" tabIndex={-1} ref={headingRef}>
      Confirm it is you
    </h3>
    {children}
  </div>
);

/** The DM/group purge's credential stage (#1354, picker PR 2). */
const DmStepUpStage: React.FC<DmStepUpStageProps> = ({ impossible, onGoToPrivacy, ...stage }) => (
  <StepUpShell headingRef={stage.headingRef}>
    {impossible ? (
      <div className="purge-modal__form">
        {/* No password and no MFA: there is nothing the user could type
            that would work, so the stage offers no retryable field. */}
        <p className="purge-modal__deadend">
          Your account signs in without a password, so we cannot confirm your identity here. Turn
          off <strong>Require authentication before purging</strong> in Privacy &amp; Security, then
          try again.
        </p>
        <div className="purge-modal__actions">
          <button type="button" className="purge-modal__cancel" onClick={stage.onCancel}>
            Cancel
          </button>
          <button type="button" className="purge-modal__confirm" onClick={onGoToPrivacy}>
            Go to Privacy &amp; Security
          </button>
        </div>
      </div>
    ) : (
      <StepUpForm
        intro="Purging messages is permanent, so we ask you to confirm your identity first."
        {...stage}
      />
    )}
  </StepUpShell>
);

/**
 * The delete-rate soft-lock challenge (#3455): the purge again, with a factor.
 * Enrolment (E8) is a state of the credential fields, not of this stage.
 */
const SoftLockStepUpStage: React.FC<StepUpStageProps> = (stage) => (
  <StepUpShell headingRef={stage.headingRef}>
    <StepUpForm
      intro="You've deleted several messages quickly. Confirm it's you to keep going."
      {...stage}
    />
  </StepUpShell>
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

/**
 * A DM, or a group the viewer only belongs to: a purge there removes the
 * viewer's own messages and never reaches a peer's pins (#3458).
 */
function purgeLeavesPeerPins(context: PurgeContext, role: 'admin' | 'member'): boolean {
  return context === 'dm' || (context === 'group' && role === 'member');
}

/** Whose pins are left visible: the other person in a DM, otherwise "others". */
function peerLabel(context: PurgeContext, scopeName: string): string {
  return context === 'dm' ? scopeName : 'others';
}

/**
 * Said when a purge deleted the viewer's own pinned messages but cannot touch a
 * peer's: those stay visible, and unpinning is how to hide them (#3458, §18.2).
 * The dialog's pin sentence and the result both read it from here, so the two
 * cannot drift. Undefined unless pins were included in a DM or group-member purge.
 */
function peerPinNote(
  context: PurgeContext,
  role: 'admin' | 'member',
  scopeName: string,
  mode: PinMode
): string | undefined {
  if (mode !== 'include' || !purgeLeavesPeerPins(context, role)) return undefined;
  return `Pinned messages from ${peerLabel(context, scopeName)} stay visible. Unpin them to hide them.`;
}

/**
 * What the purge does with pinned messages (#3458). One string, so it reads as
 * one sentence wherever it lands. Empty when the server predates the option:
 * that server deletes pins, and today's copy already says nothing about them.
 */
function pinSentence(
  context: PurgeContext,
  role: 'admin' | 'member',
  scopeName: string,
  selfScopeOnly: boolean,
  mode: PinMode
): string {
  if (mode === 'unsupported') return '';
  const include = mode === 'include';
  const peerNote = peerPinNote(context, role, scopeName, mode);
  if (peerNote !== undefined) return `Your pinned messages will be deleted too. ${peerNote}`;
  if (purgeLeavesPeerPins(context, role)) {
    // Only keep mode reaches here: include already returned through peerNote.
    return `Your pinned messages are kept. Pinned messages from ${peerLabel(context, scopeName)} stay visible to you.`;
  }
  if (context === 'group') {
    return include
      ? 'Pinned messages will be deleted for everyone.'
      : 'Pinned messages are kept for everyone.';
  }
  if (selfScopeOnly) {
    return include ? 'Your pinned messages will be deleted too.' : 'Your pinned messages are kept.';
  }
  return include ? 'Pinned messages will be deleted too.' : 'Pinned messages are kept.';
}

/** The pin sentence, bold when pins go too: the wording and the weight both change. */
function pinText(sentence: string, mode: PinMode): React.ReactNode {
  return mode === 'include' ? <strong>{sentence}</strong> : sentence;
}

interface PinnedOptionProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
}

/** The "Include pinned messages" choice. A native checkbox: the whole row is the target. */
const PinnedOption: React.FC<PinnedOptionProps> = ({ checked, onChange }) => {
  const helperId = useId();
  return (
    <div className="purge-modal__option">
      <label>
        <input
          type="checkbox"
          checked={checked}
          onChange={(e) => onChange(e.target.checked)}
          aria-describedby={helperId}
        />
        <span className="purge-modal__option-label">Include pinned messages</span>
      </label>
      <p className="purge-modal__option-helper" id={helperId}>
        Pinned messages are kept unless you include them.
      </p>
    </div>
  );
};

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
  const [includePinned, setIncludePinned] = useState(false);
  // Every stage's copy and the wire value derive from one per-open sample, so
  // the option cannot vanish mid-step-up when a capability refresh lands.
  const pinMode = pinModeFor(usePurgeKeepsPinnedAtOpen(isOpen), includePinned);
  const [typed, setTyped] = useState('');
  const [busy, setBusy] = useState(false);
  const [stage, setStage] = useState<Stage>('configure');
  const [result, setResult] = useState<TerminalPurgeResult | null>(null);
  // The wire secret. Component-local state only — never a store, never a log,
  // never echoed into error copy ([internal]rules/observability.md). The code
  // lives in the factor hooks.
  const [password, setPassword] = useState('');
  // The account holds neither a password nor MFA, so the step-up stage has
  // nothing to ask for (the purge route's 400).
  const [stepUpImpossible, setStepUpImpossible] = useState(false);
  // #3455: the refusal that opened the soft-lock stage. It seeds the stage's
  // factor hook and is never read again: the hook words every later reply.
  const [softLockOpener, setSoftLockOpener] = useState<StepUpFactorRefusal | null>(null);
  const firstRangeRef = useRef<HTMLSelectElement>(null);
  const stageHeadingRef = useRef<HTMLHeadingElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);
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
  const privateConversation = context === 'dm' || context === 'group';
  const stepUpRequired = privateConversation && requireAuthBeforePurge !== false;

  // Friction tracks irreversibility, not org scope — purging all time in a DM
  // earns the same pause as a server (spec R-11).
  const needsTypedConfirm = range === 'all' || context === 'server';
  const canConfirm = range !== null && (!needsTypedConfirm || typed === 'PURGE') && !busy;

  // passwordLeg set to always: the password field stays up through an
  // `mfa_required` refusal. The seam verifies the password FIRST, so that
  // refusal is exactly what a correct password with no code receives — hiding
  // the field on it made the next submit send no password, drew
  // `password_required`, and burned a second attempt from the budget the purge
  // itself spends (#2792).
  //
  // `readFailure: 'block'`: the purge is irreversible and its attempt budget is
  // the one the purge itself spends, so a failed read blocks with Retry rather
  // than guessing which factors to ask for. The read starts when the dialog
  // opens, so the stage is ready on arrival; `stage` also enables it for a
  // credential-less purge the server refused anyway (a stale local setting).
  // It never runs for a channel or server purge.
  const factor = useStepUpFactor({
    enabled: isOpen && (stepUpRequired || (privateConversation && stage === 'stepup')),
    purpose: 'dm.purge',
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'block',
    allowBackup: true,
  });
  // The delete-rate soft-lock (#3455) of a channel or server self-purge, enabled
  // while its challenge is on screen. The seed is the refusal that opened it and
  // applies when this instance starts, which is the render that sets `enabled`
  // (G2). `whenNoMfa`: the own rule asks for a password only of an account with
  // no inline method, and it travels as a minted token (#3509). `passwordOnly`: a
  // failed read keeps the seeded methods rather than blocking a challenge that
  // already named what it wants. One route, one purpose.
  const softLockFactor = useStepUpFactor({
    enabled: isOpen && stage === 'softlock',
    purpose: context === 'server' ? 'messages.server_purge' : 'messages.channel_purge',
    passwordLeg: LEG_ONLY_WITHOUT_MFA,
    readFailure: 'passwordOnly',
    allowBackup: true,
    seed: softLockOpener,
  });
  const submitting = factor.phase === 'submitting' || softLockFactor.phase === 'submitting';

  // A stage change moves focus to the stage heading. The dialog title stays put:
  // ui/Modal binds it to aria-labelledby, so renaming it mid-interaction renames
  // the dialog (WCAG 4.1.2 / 3.2.2). So does the step-up dead end, which
  // unmounts the button that was just pressed: the focused field unmounts, and
  // without this focus would fall out of the dialog. Inside the credential stage
  // the factor's own focus table takes over (StepUpCredentials).
  useLayoutEffect(() => {
    if (stage === 'stepup' || stage === 'softlock') stageHeadingRef.current?.focus();
  }, [stage, stepUpImpossible]);

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
    setBusy(false);
    setStage('configure');
    setResult(null);
    setStepUpImpossible(false);
    setSoftLockOpener(null);
    setRange(null);
    setIncludePinned(false);
    setTyped('');
    /* eslint-enable @eslint-react/set-state-in-effect -- reset block ends here */
  }, [isOpen]);

  // The scope the open dialog was opened for. ChatView, DMChatArea and
  // GroupInfoPanel keep it mounted across a conversation change, so a scope that
  // changes underneath an open dialog closes it rather than carrying the chosen
  // range, a typed PURGE or a step-up stage onto the next scope.
  const openedScopeRef = useRef<string | null>(null);
  useEffect(() => {
    const scope = `${context}:${scopeId}`;
    if (!isOpen) {
      openedScopeRef.current = null;
      return;
    }
    if (openedScopeRef.current === null) openedScopeRef.current = scope;
    else if (openedScopeRef.current !== scope) onClose();
  }, [isOpen, context, scopeId, onClose]);

  // The soft-lock gate runs before any purge batch, so nothing was purged. Only
  // the first challenge gets here, from a purge sent without credentials: the
  // stage's own tries are worded by its factor hook (submitStepUp).
  const showSoftLockChallenge = (next: SoftLockChallengeView) => {
    setSoftLockOpener(softLockSeed(next));
    setStage('softlock');
  };

  // The credential challenge is a stage, not an outcome. A purge sent from the
  // configure stage carried no credentials, so a refusal of it is the challenge
  // that opens the stage, not a mistake: nothing was typed, the factor hook
  // reads the requirement afresh, and focus stays on the stage heading. A
  // refusal of what was typed never gets here — the hook words it in place.
  const showStepUpChallenge = (outcome: StepUpPurgeResult) => {
    setStepUpImpossible(outcome.kind === 'stepUpImpossible');
    setStage('stepup');
  };

  const applyOutcome = (outcome: PurgeOutcome) => {
    // Nothing left, so nothing changes: the stage and what was typed stay (D7).
    if (outcome.kind === 'notSent') return;
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
    setResult(outcome);
    setStage('result');
  };

  // Runs one purge request and applies its outcome, unless the dialog closed
  // or reopened while it was in flight. Clearing `busy` matters: it disables
  // the fieldset that owns Cancel, and `dismissable={!busy}` removes the close
  // button and gates both Escape and the backdrop click. A stale request leaves
  // `busy` alone, because the close already reset it for the new open.
  //
  // A challenge asks for credentials for the account and server that sent it,
  // so after a change it opens no stage: what would be typed there belongs to
  // neither. The purge was refused before any batch, so nothing is reported.
  const runPurge = async (args: PurgeArgs) => {
    const generation = openGenerationRef.current;
    const requestContext = captureApiRequestContext();
    setBusy(true);
    let outcome: PurgeOutcome;
    try {
      // A send that promises something about pins rechecks the capability
      // inside purgeMessages, immediately before the request.
      outcome = await purgeMessages(args, requestContext);
    } catch (error) {
      outcome = isAbortError(error) ? NOT_SENT : TRANSPORT_FAILURE;
    }
    if (generation !== openGenerationRef.current) return;
    if (!isChallenge(outcome) || apiRequestContextIsCurrent(requestContext)) applyOutcome(outcome);
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
    await runPurge({ context, scopeId, range, pinMode });
  };

  // The purge with the credentials a factor hook prepared, for the DM/group
  // step-up stage and the soft-lock stage alike (their contexts never overlap).
  // The hook owns the phase, so there is no `busy` here, and it holds the
  // spent-code rules, so only the result stage and the password are decided in
  // this file. Single-shot: whichever factors the actor has travel in the same
  // request. Probing for the requirement costs a call against the very purge
  // budget the user is trying to spend on the purge itself (spec R-7).
  const submitStepUp: StepUpSubmit = async (mfa, requestContext) => {
    if (range === null) return { kind: 'aborted' };
    const generation = openGenerationRef.current;
    const args: PurgeArgs = {
      context,
      scopeId,
      range,
      currentPassword: password || undefined,
      mfaCode: mfa,
      softLockPrior: CODE_PROMPT,
      pinMode,
    };
    // A self-purge's password goes to the mint, not to the route, so it leaves
    // component state as it is sent (#3509 frontend review).
    if (!privateConversation) setPassword('');
    let outcome: PurgeOutcome;
    try {
      outcome = endExchangeRefusal(await purgeMessages(args, requestContext));
    } catch (error) {
      // apiFetch's pre-dispatch fence (an account or server change): the
      // request never left, so nothing was purged and the code is unspent.
      if (isAbortError(error)) return { kind: 'aborted' };
      outcome = TRANSPORT_FAILURE;
    }
    const submitted = toSubmitOutcome(outcome);
    // A dialog closed or reopened during the request drops the outcome, and so
    // does an account or server change: the result, and the cache clear it
    // dispatches, belong to the old one.
    if (generation !== openGenerationRef.current || !apiRequestContextIsCurrent(requestContext)) {
      return submitted;
    }
    if (staysInStage(outcome)) {
      // The stage stays up and the hook words the reply. Only the password
      // the server rejected is dropped, so a wrong password does not cost the
      // user a fresh code they already typed.
      if (outcome.kind === 'invalidPassword') setPassword('');
    } else {
      applyOutcome(outcome);
    }
    return submitted;
  };

  const stepUp = stepUpActivation(factor, password, submitStepUp);
  const softLockStepUp = stepUpActivation(softLockFactor, password, submitStepUp);

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
  const pins = pinSentence(context, role, scopeName, selfScopeOnly, pinMode);
  const pinRecap =
    pins === '' ? null : <p className="purge-modal__pin-recap">{pinText(pins, pinMode)}</p>;

  return (
    <Modal
      isOpen={isOpen}
      onClose={onClose}
      title={TITLES[context]}
      width="medium"
      dismissable={!busy && !submitting}
      initialFocusRef={firstRangeRef}
    >
      <div className="purge-modal__body">
        {stage === 'result' && result !== null && (
          <PurgeResult
            context={context}
            result={result}
            pinMode={pinMode}
            peerPinNote={peerPinNote(context, role, scopeName, pinMode)}
            onDone={onClose}
          />
        )}

        {stage === 'stepup' && (
          <DmStepUpStage
            factor={factor}
            impossible={stepUpImpossible}
            password={password}
            onPasswordChange={setPassword}
            primaryRef={primaryRef}
            headingRef={stageHeadingRef}
            ariaDisabled={stepUp.ariaDisabled}
            onActivate={stepUp.activate}
            onCancel={onClose}
            onGoToPrivacy={handleGoToPrivacy}
            pinRecap={pinRecap}
          />
        )}

        {stage === 'softlock' && (
          <SoftLockStepUpStage
            factor={softLockFactor}
            password={password}
            onPasswordChange={setPassword}
            primaryRef={primaryRef}
            headingRef={stageHeadingRef}
            ariaDisabled={softLockStepUp.ariaDisabled}
            onActivate={softLockStepUp.activate}
            onCancel={onClose}
            pinRecap={pinRecap}
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

            {pinMode !== 'unsupported' && (
              <PinnedOption checked={includePinned} onChange={setIncludePinned} />
            )}

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
                  {pins !== '' && (
                    <span className="purge-modal__scope-pins">{pinText(pins, pinMode)}</span>
                  )}
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
