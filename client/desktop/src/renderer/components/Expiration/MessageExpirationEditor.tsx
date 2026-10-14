import { useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Pin } from 'lucide-react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import DangerousActionStepUpDialog, {
  type DangerousActionSendResult,
} from '../Auth/DangerousActionStepUpDialog';
import { focusTargetIn } from '../ui/focusTarget';
import { useStepUpHandoff } from '../../hooks/auth/useStepUpHandoff';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import {
  EXPIRATION_WINDOW_OPTIONS,
  type ExpirationMutationResult,
  type ExpirationPolicy,
  type ExpirationPolicyReadResult,
  type ExpirationRequest,
  type ExpirationScope,
  type ExpirationWindowSeconds,
} from '../../services/messaging/expirationPolicyApi';
import {
  describeFailureWith,
  FIRST_SEND_SESSION_CHANGED,
} from '../../services/system/dangerousActionRequest';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { stepUpSeed } from '../../services/system/stepUpSeed';
import { openVerificationSetup } from '../../utils/ui/openVerificationSetup';
import { useAuthStore } from '../../stores/auth/authStore';
import { useExpirationPinnedPolicyUnsupported } from '../../stores/ui/clientConfigStore';
import PinnedRetentionOption, { isPinnedRetentionAvailable } from './PinnedRetentionOption';
import '../common/audioQualitySlider.css';
import './messageExpiration.css';

export interface MessageExpirationEditorProps {
  scope: ExpirationScope;
  policy: ExpirationPolicy | null;
  policyState: 'loading' | 'ready' | 'unavailable';
  canEdit: boolean;
  lockedDescription: string;
  onRefresh: () => Promise<ExpirationPolicyReadResult>;
  /** `context` is the capture the PATCH is admitted against: the first send's, on both sends. */
  onApplyPolicy: (
    request: ExpirationRequest,
    context?: ApiRequestContext
  ) => Promise<ExpirationMutationResult>;
  onClose: () => void;
}

type Choice = 'apply' | 'new_only' | 'clear_pending' | 'leave_pending';
type Baseline = {
  scopeKey: string;
  lifecycle: ReturnType<typeof captureAuthLifecycle>;
  revision: number;
  windowSeconds: ExpirationWindowSeconds | null;
  backfillPending: boolean;
  /** The pin claim the consent was given against (#3459, FD T20). Part of the baseline because
   *  a rollback can erase it at an unchanged revision, and a confirmation about pins must not
   *  survive the claim it was shown beside. */
  includePinned: boolean | undefined;
};

/** What the person is told is true right now, after a change of the pin switch landed. Rendered
 *  in an always-mounted `role="status"` region: it must exist BEFORE its text changes, or a
 *  screen reader has nothing registered to announce. The OFF sentence is deliberately hedged:
 *  stamps already past due are not rescued (C-NORES), so "kept" is true of every pin that was
 *  still on the clock and not of one that had run out. */
const PINNED_EXPIRY_ON_STATUS =
  'Pinned messages now expire. Any whose timer has run out are being deleted.';
const PINNED_EXPIRY_OFF_STATUS =
  'Pinned messages are now kept. Any whose timer had already run out may still be deleted in the next few minutes.';

// 'Off' is the editor's own zero stop; the windows come from the canonical map so a new one
// cannot be added to the schema and forgotten here.
const stops: Array<{ label: string; value: ExpirationWindowSeconds | null }> = [
  { label: 'Off', value: null },
  ...EXPIRATION_WINDOW_OPTIONS,
];

function semanticError(result: ExpirationMutationResult): string | null {
  if (result.kind === 'ambiguous')
    return 'We couldn’t confirm whether this change was applied. Refresh the policy before continuing.';
  if (result.kind === 'conflict')
    return 'This policy changed. Refresh the policy before continuing.';
  if (result.kind !== 'rejected') return null;
  if (result.reason === 'forbidden') return 'The server refused this change.';
  if (result.reason === 'notFound') return 'This conversation is no longer available.';
  if (result.reason === 'rateLimited')
    return result.retryAfterSeconds === undefined
      ? 'Too many changes. Try again later.'
      : `Too many changes. Try again in ${result.retryAfterSeconds} seconds.`;
  if (result.reason === 'sessionExpired') return 'Your session has expired.';
  return 'This change could not be applied.';
}

function expirationLockReason(
  policyState: MessageExpirationEditorProps['policyState'],
  policy: ExpirationPolicy | null,
  forbidden: boolean,
  mutationBlocked: boolean,
  needsRefresh: boolean,
  lockedDescription: string
): string {
  if (policyState === 'loading') return 'Loading message expiration.';
  if (policyState === 'unavailable') {
    return 'Message expiration is unavailable. Refresh the policy to try again.';
  }
  if (policy?.backfillPending) {
    return 'Timer controls are unavailable while existing messages are processing.';
  }
  if (forbidden) return 'The server refused this change.';
  if (mutationBlocked || needsRefresh) return 'Refresh the policy before continuing.';
  return lockedDescription;
}

function policyBaseline(scope: ExpirationScope, policy: ExpirationPolicy | null): Baseline | null {
  if (!policy) return null;
  return {
    scopeKey: `${scope.kind}:${scope.id}`,
    lifecycle: captureAuthLifecycle(),
    revision: policy.revision,
    windowSeconds: policy.windowSeconds,
    backfillPending: policy.backfillPending,
    includePinned: policy.includePinned,
  };
}

function isCurrentOperation(
  mounted: boolean,
  operation: number,
  currentOperation: number,
  currentScope: ExpirationScope,
  capturedScope: string,
  lifecycle: ReturnType<typeof captureAuthLifecycle>
): boolean {
  return (
    mounted &&
    operation === currentOperation &&
    `${currentScope.kind}:${currentScope.id}` === capturedScope &&
    isSameAuthLifecycle(lifecycle)
  );
}

function matchesCurrentPolicyBaseline(
  baseline: Baseline,
  scope: ExpirationScope,
  policy: ExpirationPolicy | null
): boolean {
  if (baseline.scopeKey !== `${scope.kind}:${scope.id}`) return false;
  if (!isSameAuthLifecycle(baseline.lifecycle)) return false;
  if (baseline.revision !== policy?.revision) return false;
  if (baseline.windowSeconds !== policy?.windowSeconds) return false;
  if (baseline.includePinned !== policy?.includePinned) return false;
  return baseline.backfillPending === policy?.backfillPending;
}

/** `includePinned` rides only when the person changed the switch (#3459): `undefined` leaves the
 *  key off the body entirely, which is what an older server's closed schema and a window-only
 *  change both need. A `clear` never carries it; the server refuses the key there. */
function policyRequest(
  draftWindow: ExpirationWindowSeconds | null,
  choice: Choice,
  includePinned: boolean | undefined
): ExpirationRequest {
  if (draftWindow === null) {
    return {
      mode: 'clear',
      retroactive: choice as Extract<Choice, 'clear_pending' | 'leave_pending'>,
    };
  }
  return {
    mode: 'set',
    window_seconds: draftWindow,
    retroactive: choice as Extract<Choice, 'apply' | 'new_only'>,
    ...(includePinned === undefined ? {} : { include_pinned: includePinned }),
  };
}

function matchesPartialPolicy(
  reread: ExpirationPolicyReadResult,
  candidate: ExpirationPolicy | undefined
): boolean {
  return (
    reread.kind === 'fresh' &&
    reread.policy.revision === candidate?.revision &&
    reread.policy.windowSeconds === candidate?.windowSeconds &&
    reread.policy.includePinned === candidate?.includePinned
  );
}

function matchesPolicyBaseline(policy: ExpirationPolicy, baseline: Baseline): boolean {
  return (
    policy.revision === baseline.revision &&
    policy.windowSeconds === baseline.windowSeconds &&
    policy.includePinned === baseline.includePinned &&
    !policy.backfillPending
  );
}

/** The switch's value before anything is staged: the server's, except under no timer, where a
 *  server's own `false` is the only honest reading and a skewed `true` must not seed a checked
 *  box (the next `set` would turn pinned expiry on by accident). */
function startingInclude(
  policy: ExpirationPolicy | null,
  appliedWindow: ExpirationWindowSeconds | null | undefined
): boolean {
  if (typeof appliedWindow !== 'number') return false;
  return policy?.includePinned === true;
}

/**
 * The ONE pin sentence the window-change confirmation carries (#3459), or null when nothing true
 * can be said. Null for: no switch available (an older server deletes pins, and the existing
 * disclosure already speaks for it), and a change to Off (a `clear` has no pin semantics).
 *
 * Only "Apply to existing messages" restamps existing pins under the new window, so only then is
 * "older than <window>" what gets deleted. "Only new messages" gives each kept pin back the stamp
 * it was parked with, under whatever window was in force then, or none at all when the timer was
 * off (#3459 Codex review); what is due then is whatever pin's own timer has run out.
 */
function pinConfirmationLine(
  available: boolean,
  nextWindow: ExpirationWindowSeconds | null,
  finalInclude: boolean,
  includeChanged: boolean,
  retroactive: boolean,
  windowLabel: string,
  fromOff: boolean
): string | null {
  if (!available || nextWindow === null) return null;
  if (!finalInclude) return 'Pinned messages are kept.';
  if (!includeChanged) return 'Pinned messages expire like all other messages.';
  // From Off nothing was kept, so the server has no due pins to delete before it answers: under
  // Apply the sweep deletes the ones now past their window within minutes, and under Only new
  // messages nothing is stamped at all (#3459 Codex review).
  if (fromOff) {
    return retroactive
      ? `Pinned messages will expire too, and any older than ${windowLabel} are deleted within the next few minutes.`
      : 'Pinned messages will expire too.';
  }
  return retroactive
    ? `Pinned messages will expire too, and any older than ${windowLabel} are deleted right away.`
    : 'Pinned messages will expire too, and any whose timer has already run out are deleted right away.';
}

interface StagingInput {
  policy: ExpirationPolicy | null;
  policyState: MessageExpirationEditorProps['policyState'];
  draftWindow: ExpirationWindowSeconds | null | undefined;
  draftInclude: boolean | undefined;
  capabilityUnsupported: boolean;
  retroactive: boolean;
}

/**
 * Everything the editor derives from the policy and what is staged, as one pure function so the
 * component stays readable (and inside Sonar's cognitive-complexity limit).
 *
 * Three distinct window facts, deliberately not collapsed into one:
 *   appliedWindow  - what the server currently enforces (undefined while unknown)
 *   draftWindow    - what the user has staged, undefined when nothing is staged
 *   selectedWindow - what the row should highlight right now
 * `null` is a real value here (Off), so every comparison uses `undefined` for "unknown"/"unstaged"
 * and never a falsy test, which would fold Off into both.
 */
function deriveStaging({
  policy,
  policyState,
  draftWindow,
  draftInclude,
  capabilityUnsupported,
  retroactive,
}: StagingInput) {
  // A refresh that keeps the policy it is re-reading (the confirmation's own pre-send read, a
  // reconnect) puts the state in `loading` for its duration. The VIEW keeps describing the policy
  // already held, so an open confirmation does not retitle itself for that moment (#3459 Codex
  // review); `editable` still reads the real state, so nothing can be staged until the read lands.
  // The switch is drawn from the view too, LOCKED for the refresh rather than unmounted, so a
  // keyboard user focused on it does not drop to <body> when a refresh starts (#3459 Codex review).
  const viewState = policyState === 'loading' && policy ? 'ready' : policyState;
  const appliedWindow = viewState === 'ready' ? (policy?.windowSeconds ?? null) : undefined;
  const selectedWindow = draftWindow === undefined ? appliedWindow : draftWindow;
  // A window change is a staged window that differs from the one in force. A pins-only change
  // (#3459) leaves `draftWindow` undefined and moves only the switch.
  const hasWindowChange = draftWindow !== undefined && draftWindow !== appliedWindow;
  // The switch exists only when the SAME rule the component applies says so (render gate), and
  // what is staged is honoured only while it does: a value staged before the capability flipped
  // is not sent just because the control is no longer drawn.
  const pinnedAvailable = isPinnedRetentionAvailable({
    policy,
    policyState: viewState,
    effectiveWindow: selectedWindow,
    capabilityUnsupported,
  });
  const startInclude = startingInclude(policy, appliedWindow);
  const stagedInclude = pinnedAvailable ? draftInclude : undefined;
  const hasIncludeChange = stagedInclude !== undefined && stagedInclude !== startInclude;
  const finalInclude = stagedInclude ?? startInclude;
  // Pins-only ON is the one change whose dedicated confirmation replaces the window one.
  const pinsOnlyOn = !hasWindowChange && hasIncludeChange && stagedInclude === true;
  const disclosureWindow = draftWindow === undefined ? appliedWindow : draftWindow;
  const selectedDuration =
    stops.find((stop) => stop.value === disclosureWindow)?.label ?? 'the selected time';
  return {
    viewState,
    appliedWindow,
    selectedWindow,
    hasWindowChange,
    startInclude,
    hasIncludeChange,
    // `include_pinned` for the request: defined only when the person changed the switch.
    includeToSend: hasIncludeChange ? stagedInclude : undefined,
    hasPendingChange: hasWindowChange || hasIncludeChange,
    finalInclude,
    pinsOnlyOn,
    disclosure: confirmationDisclosure(pinsOnlyOn, draftWindow, selectedDuration),
    pinLine: pinConfirmationLine(
      pinnedAvailable,
      disclosureWindow ?? null,
      finalInclude,
      hasIncludeChange,
      retroactive,
      selectedDuration,
      appliedWindow === null
    ),
  };
}

/** The confirmation's main sentence, by what is being confirmed. */
function confirmationDisclosure(
  pinsOnlyOn: boolean,
  draftWindow: ExpirationWindowSeconds | null | undefined,
  selectedDuration: string
): string {
  if (pinsOnlyOn) {
    // Pins-only ON is `new_only` by construction, so each kept pin gets back the stamp it was
    // parked with, set under whatever window was in force when it was sent: its age against the
    // current window says nothing about whether it is due.
    return 'Pinned messages will expire like every other message. Any pinned message whose timer has already run out is deleted right away.';
  }
  if (draftWindow === null) {
    return 'Cancel scheduled deletions stops applicable upcoming schedules and does not restore messages already deleted. Keep scheduled deletions leaves already scheduled messages to delete while future messages get no timer. Text, images, GIFs, attachments, and emojis already deleted cannot be recovered.';
  }
  return `Applying to existing messages means eligible existing and future text, images, GIFs, attachments, and emojis are permanently deleted after ${selectedDuration} and cannot be recovered. New-only leaves already scheduled messages unchanged.`;
}

function isCurrentBaselineOperation(
  mounted: boolean,
  operation: number,
  currentOperation: number,
  currentScope: ExpirationScope,
  capturedScope: string,
  lifecycle: ReturnType<typeof captureAuthLifecycle>,
  baseline: Baseline
): boolean {
  return (
    isCurrentOperation(
      mounted,
      operation,
      currentOperation,
      currentScope,
      capturedScope,
      lifecycle
    ) && isSameAuthLifecycle(baseline.lifecycle)
  );
}

function isRejectedReason(
  result: ExpirationMutationResult,
  reason: Extract<ExpirationMutationResult, { kind: 'rejected' }>['reason']
): boolean {
  return result.kind === 'rejected' && result.reason === reason;
}

function requiresPolicyRefresh(result: ExpirationMutationResult): boolean {
  return result.kind === 'ambiguous' || result.kind === 'conflict';
}

type ShortenRequest = Extract<ExpirationRequest, { mode: 'set' }>;

/** The refused change, frozen as the first send made it, while the verification dialog is up. */
interface PendingShorten {
  /** The re-send adds `mfa_code` to this and changes nothing else. `include_pinned` stays on it
   *  when the first send carried it (#3459). */
  request: ShortenRequest;
  /** The change turns pinned expiry ON (the server gates false-to-true like a shortening). */
  pinsOn: boolean;
  /** The change also moves the window, so the dialog names both rather than only the pins. */
  windowChange: boolean;
  refusal: StepUpFactorRefusal;
  /** The scope and account it was refused under: it is abandoned if either moves. */
  sessionKey: string;
  /**
   * The account and server the first send went out as (C82), the dialog's `capture`: the
   * re-send and "Set up verification" work against it, never one the dialog takes on mounting.
   */
  context: ApiRequestContext;
}

const describeShortenFailure = describeFailureWith('This change could not be applied.');

/** What the verification dialog says, by what the refused change does. The purpose stays
 *  `channels.expiration_shorten` for all three: the server gates enabling pinned expiry with the
 *  same purpose as a shortening, so only the words differ. */
function stepUpCopy(pending: PendingShorten | null): { intro: string; primaryLabel: string } {
  if (pending?.pinsOn && pending.windowChange) {
    return {
      intro:
        'This server asks you to verify before you change how long messages are kept and let pinned messages expire.',
      primaryLabel: 'Apply timer',
    };
  }
  if (pending?.pinsOn) {
    return {
      intro: 'This server asks you to verify before pinned messages start to expire.',
      primaryLabel: 'Expire pinned messages',
    };
  }
  return {
    intro: 'This server asks you to verify before you shorten how long messages are kept.',
    primaryLabel: 'Apply timer',
  };
}

/**
 * What the dialog is told about an answer that is about verification: the gate's own, and a dead
 * session. Null means it is the editor's to settle, as it is for the first send.
 */
function verificationAnswer(result: ExpirationMutationResult): DangerousActionSendResult | null {
  if (result.kind === 'gated') return { kind: 'refused', status: result.status, body: result.body };
  if (isRejectedReason(result, 'sessionExpired'))
    return { kind: 'refused', status: 401, body: null };
  return null;
}

export default function MessageExpirationEditor({
  scope,
  policy,
  policyState,
  canEdit,
  lockedDescription,
  onRefresh,
  onApplyPolicy,
  onClose,
}: Readonly<MessageExpirationEditorProps>) {
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const [draftWindow, setDraftWindow] = useState<ExpirationWindowSeconds | null | undefined>();
  // The staged pin switch (#3459). `undefined` is "not touched" and is what keeps `include_pinned`
  // off the request: it is sent ONLY when this is defined, so a window-only change never
  // re-asserts a value the person did not choose (and an older server never sees the key).
  const [draftInclude, setDraftInclude] = useState<boolean | undefined>();
  const [applying, setApplying] = useState(false);
  const [statusText, setStatusText] = useState('');
  const [optionFocusTick, setOptionFocusTick] = useState(0);
  const [choice, setChoice] = useState<Choice>();
  const [acknowledged, setAcknowledged] = useState(false);
  const [confirmationOpen, setConfirmationOpen] = useState(false);
  const [resumeOpen, setResumeOpen] = useState(false);
  const [mutationBlocked, setMutationBlocked] = useState(false);
  const [forbidden, setForbidden] = useState(false);
  const [needsRefresh, setNeedsRefresh] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [baseline, setBaseline] = useState<Baseline | null>(null);
  const operationRef = useRef(0);
  const mountedRef = useRef(true);
  const stopsRef = useRef<HTMLDivElement>(null);
  const applyRef = useRef<HTMLDivElement>(null);
  const optionRef = useRef<HTMLDivElement>(null);
  const scopeRef = useRef(scope);
  const authGenerationRef = useRef(authGeneration);
  const descriptionId = useId();
  const pinnedPolicyUnsupported = useExpirationPinnedPolicyUnsupported();
  // `includePinned` is in both identities (FD T20): a pin claim that changed, or was erased at an
  // unchanged revision by the equal-revision merge, must retire a consent given beside the old one.
  const policyBaselineIdentity = policy
    ? `${policy.revision}:${policy.windowSeconds ?? 'off'}:${policy.backfillPending}:${policy.includePinned ?? 'unset'}`
    : 'none';
  const consentBaselineIdentity = baseline
    ? `${baseline.revision}:${baseline.windowSeconds ?? 'off'}:${baseline.backfillPending}:${baseline.includePinned ?? 'unset'}`
    : 'none';
  const confirmationIdentity = `${scope.kind}:${scope.id}:${authGeneration}:${policyBaselineIdentity}:${consentBaselineIdentity}`;
  const confirmationIdentityRef = useRef(confirmationIdentity);
  // The policy the status region last spoke for: its revision and its pin claim. A newer policy
  // shown, whether it landed before or after this answer, supersedes the announcement, and so does
  // the same revision with a different or absent claim (the equal-revision merge drops the claim
  // when an older server answers after a rollback): "Pinned messages are now kept" must not stand
  // beside a policy that no longer keeps them (#3459). An OLDER revision is the prop not yet
  // caught up with the answer, and says nothing about it.
  const [announced, setAnnounced] = useState<{ revision: number; include: boolean } | null>(null);
  const announcementSuperseded =
    announced !== null &&
    policy !== null &&
    (policy.revision > announced.revision ||
      (policy.revision === announced.revision && policy.includePinned !== announced.include));

  scopeRef.current = scope;
  authGenerationRef.current = authGeneration;
  confirmationIdentityRef.current = confirmationIdentity;

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      operationRef.current += 1;
    };
  }, []);
  useEffect(
    () => () => {
      setChoice(undefined);
      setAcknowledged(false);
      setDraftWindow(undefined);
      setDraftInclude(undefined);
      setApplying(false);
      // The announcement belongs to the scope and account it was made in; it is NOT cleared by
      // the policy-identity effect below, because the policy changing is exactly what a
      // successful apply does and the announcement is about to be read.
      setStatusText('');
      setBaseline(null);
      setConfirmationOpen(false);
      setResumeOpen(false);
      setForbidden(false);
      setMutationBlocked(false);
      setNeedsRefresh(false);
      setError(null);
    },
    [authGeneration, scope.id, scope.kind]
  );
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- retain the staged choice unless the new policy already applies it
    setDraftWindow((current) => (current === policy?.windowSeconds ? undefined : current));
    // The pin switch follows the same rule: a staged value the new policy already applies is dropped.
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- retain the staged switch unless the new policy already applies it
    setDraftInclude((current) => (current === policy?.includePinned ? undefined : current));
    return () => {
      setChoice(undefined);
      setAcknowledged(false);
      setBaseline(null);
      setConfirmationOpen(false);
      setResumeOpen(false);
    };
  }, [policyBaselineIdentity, policy?.windowSeconds, policy?.includePinned]);
  // Returns focus to the switch after an include-only apply, once the commit that removed the
  // Apply button (and the confirmation's own focus restore, which targets that button) has run.
  // Without it focus falls to <body>. Effects run after the commit, so this wins the race.
  useEffect(() => {
    if (optionFocusTick === 0) return;
    focusTargetIn(optionRef.current)?.focus();
  }, [optionFocusTick]);
  const editable =
    canEdit &&
    !forbidden &&
    policyState === 'ready' &&
    !policy?.backfillPending &&
    !mutationBlocked &&
    !needsRefresh;
  // While a direct apply is out, nothing may be staged: a change made then would bump
  // `operationRef`, so the editor would ignore the answer while the store still committed it,
  // and the policy-identity cleanup would then drop the newer draft (#3459 Codex review).
  const stageable = editable && !applying;
  const stopDescriptionId = descriptionId;
  const lockReason = expirationLockReason(
    policyState,
    policy,
    forbidden,
    mutationBlocked,
    needsRefresh,
    lockedDescription
  );
  const {
    viewState,
    appliedWindow,
    selectedWindow,
    hasWindowChange,
    startInclude,
    hasIncludeChange,
    includeToSend,
    hasPendingChange,
    finalInclude,
    pinsOnlyOn,
    disclosure,
    pinLine,
  } = deriveStaging({
    policy,
    policyState,
    draftWindow,
    draftInclude,
    capabilityUnsupported: pinnedPolicyUnsupported,
    retroactive: choice === 'apply',
  });
  // A confirmation opens only over a pending change, but the change can stop being one while the
  // dialog is up: a pins-only ON staged while capabilities were unknown is dropped once the
  // capability read says the server does not support it. Kept open, the dialog would retitle
  // itself as a timer confirmation whose Confirm sends nothing and closes as if it had applied
  // (#3459 Codex review), so it is shown only while there is something to confirm, or while an
  // apply is already under way.
  const confirmationShown = confirmationOpen && (hasPendingChange || applying);
  // The switch is drawn while a refresh runs, so it unmounts only when it truly stops applying (the
  // capability read says the server lacks it, or the window goes Off). If it held focus then, the
  // browser drops focus to <body>; put it on the active stop instead (#3459 Codex review).
  const optionShown = isPinnedRetentionAvailable({
    policy,
    policyState: viewState,
    effectiveWindow: selectedWindow,
    capabilityUnsupported: pinnedPolicyUnsupported,
  });
  const focusInOptionRef = useRef(false);
  useEffect(() => {
    const track = (event: FocusEvent) => {
      focusInOptionRef.current =
        event.target instanceof Node && optionRef.current?.contains(event.target) === true;
    };
    document.addEventListener('focusin', track);
    return () => document.removeEventListener('focusin', track);
  }, []);
  useLayoutEffect(() => {
    if (optionShown || !focusInOptionRef.current) return;
    focusInOptionRef.current = false;
    if (document.activeElement !== document.body) return;
    const stops = stopsRef.current;
    (
      stops?.querySelector<HTMLElement>('.message-expiration-stop.active') ??
      stops?.querySelector<HTMLElement>('.message-expiration-stop')
    )?.focus();
  }, [optionShown]);

  /** Is the dialog we are about to tear down still the one this render owns?
   *
   *  Guards an unmount, an auth-generation change, a different confirmation identity, and a
   *  scope switch — any of which means the close belongs to a dialog that is already gone. */
  const confirmationStillOurs = () =>
    mountedRef.current &&
    authGenerationRef.current === authGeneration &&
    confirmationIdentityRef.current === confirmationIdentity &&
    `${scopeRef.current.kind}:${scopeRef.current.id}` === `${scope.kind}:${scope.id}`;

  /** Dismiss the dialog and LEAVE the staged choice alone.
   *
   *  This is what the dialog's own Cancel does. The two acts are deliberately different —
   *  the comment on the Discard button says so in as many words — and they were collapsed
   *  into one function until review caught it: cancelling the confirmation silently did what
   *  Discard does, throwing away a staged window the user had not decided to abandon. The
   *  only way back was to re-pick it, with no indication anything had been dropped. */
  const dismissConfirmation = () => {
    if (!confirmationStillOurs()) return;
    setConfirmationOpen(false);
    setResumeOpen(false);
    setChoice(undefined);
    setAcknowledged(false);
    setBaseline(null);
  };

  /** Dismiss the dialog AND drop the staged choice.
   *
   *  For the terminal paths only — a mutation that landed, or one that failed in a way that
   *  makes the staged window meaningless (a refused window, a scope that moved). A failure
   *  the user can retry keeps the dialog open instead and never reaches here, which is what
   *  lets `aria-pressed` keep reporting the window actually in force. */
  const closeConfirmation = () => {
    if (!confirmationStillOurs()) return;
    dismissConfirmation();
    setDraftWindow(undefined);
    setDraftInclude(undefined);
  };

  // A shortening the server wants verified is handed to the verification dialog in place of the
  // confirmation (#3456, D-5). Dismissing the dialog ends like the confirmation's own Cancel: the
  // staged window stays staged. The change is frozen for its scope, account, and policy baseline;
  // if any moves it is abandoned, not parked, so it cannot come back as a prompt to apply it.
  const sessionKey = `${scope.kind}:${scope.id}:${authGeneration}:${policyBaselineIdentity}`;
  const { pending, ending, handOff, confirmClosed, endStepUp } =
    useStepUpHandoff<PendingShorten>(dismissConfirmation);
  const stepUp = pending?.sessionKey === sessionKey ? pending : null;
  useEffect(() => {
    if (pending !== null && pending.sessionKey !== sessionKey) endStepUp();
  }, [pending, sessionKey, endStepUp]);
  const stepUpWords = stepUpCopy(stepUp);
  // The confirmation's words follow what is being confirmed: pins turning ON are their own
  // question, Off is another, and every other window change keeps the original wording.
  let confirmationTitle = 'Change message expiration';
  let confirmationLabel = 'Apply timer';
  if (pinsOnlyOn) {
    confirmationTitle = 'Expire pinned messages too';
    confirmationLabel = 'Expire pinned messages';
  } else if (draftWindow === null) {
    confirmationTitle = 'Turn off message expiration';
    confirmationLabel = 'Turn off timer';
  }

  // Selecting a stop STAGES it; it does not mutate. The user must then press Apply,
  // which opens the confirmation. Before this split a single click on a label went
  // straight to a destructive-sounding modal, and the only cue for which window was
  // already in force was a font-weight change — too soft to read as "this is your
  // current retention policy" (found by looking at it, not by a test).
  const selectWindow = (windowSeconds: ExpirationWindowSeconds | null) => {
    if (!stageable) return;
    operationRef.current += 1;
    // Re-picking the window already in force clears the pending change rather than
    // staging a no-op, so the row always shows exactly one highlight.
    setDraftWindow(windowSeconds === appliedWindow ? undefined : windowSeconds);
    // Off has no pin semantics, and a `clear` is refused the key: the staged switch goes with
    // any selection that leaves no window to be excepted from.
    if (windowSeconds === null) setDraftInclude(undefined);
    setChoice(undefined);
    setAcknowledged(false);
    setError(null);
    setStatusText('');
    setNeedsRefresh(false);
  };

  // Staging the switch is staging, like a stop: nothing is sent until Apply. Returning it to the
  // value it started at drops the draft rather than staging a no-op, for the same reason
  // re-picking the applied window does, and so `include_pinned` is not sent for a non-change.
  const stageInclude = (next: boolean) => {
    if (!stageable) return;
    operationRef.current += 1;
    setDraftInclude(next === startInclude ? undefined : next);
    setChoice(undefined);
    setAcknowledged(false);
    setError(null);
    setStatusText('');
    setNeedsRefresh(false);
  };

  // The baseline is captured HERE rather than at selection time. It fences the mutation
  // against a policy that moved underneath us, and a staged choice may sit unapplied for
  // any length of time — capturing it at selection would age the fence by exactly that
  // gap and make `confirmDisabled`'s revision comparison reject legitimate applies.
  const openConfirmation = () => {
    if (!editable || !hasWindowChange) return;
    operationRef.current += 1;
    setChoice(undefined);
    setAcknowledged(false);
    setBaseline(policyBaseline(scope, policy));
    setConfirmationOpen(true);
  };
  // Pins-only ON has its own confirmation, with the choice fixed: it is `new_only` by
  // construction (the retroactive question is about the window, and the window is unchanged),
  // so there is no radio to answer and nothing to leave undecided.
  const openPinsConfirmation = () => {
    if (!editable || !pinsOnlyOn) return;
    operationRef.current += 1;
    setChoice('new_only');
    setAcknowledged(false);
    setBaseline(policyBaseline(scope, policy));
    setConfirmationOpen(true);
  };
  const setPolicyChoice = (next: Choice) => {
    setChoice(next);
    setAcknowledged(false);
  };
  const refresh = async () => {
    const lifecycle = captureAuthLifecycle();
    const scopeKey = `${scope.kind}:${scope.id}`;
    if (
      !mountedRef.current ||
      `${scopeRef.current.kind}:${scopeRef.current.id}` !== scopeKey ||
      !isSameAuthLifecycle(lifecycle)
    )
      return { kind: 'superseded' as const };
    const operation = ++operationRef.current;
    const result = await onRefresh();
    if (
      !mountedRef.current ||
      operation !== operationRef.current ||
      `${scopeRef.current.kind}:${scopeRef.current.id}` !== scopeKey ||
      !isSameAuthLifecycle(lifecycle)
    )
      return { kind: 'superseded' as const };
    if (result.kind === 'fresh') {
      setMutationBlocked(false);
      setError(null);
      setNeedsRefresh(false);
    }
    return result;
  };
  const handleRejectedPolicyChange = (result: ExpirationMutationResult) => {
    if (isRejectedReason(result, 'forbidden')) setForbidden(true);
    if (isRejectedReason(result, 'notFound')) {
      void refresh();
      onClose();
      return true;
    }
    if (!requiresPolicyRefresh(result)) return false;
    setMutationBlocked(true);
    setNeedsRefresh(true);
    closeConfirmation();
    setError(semanticError(result) ?? 'This change could not be applied.');
    return true;
  };

  /** True while the component, scope, account and operation are still the ones the request began under. */
  const isCurrent = (
    operation: number,
    capturedScope: string,
    lifecycle: ReturnType<typeof captureAuthLifecycle>
  ) =>
    isCurrentOperation(
      mountedRef.current,
      operation,
      operationRef.current,
      scopeRef.current,
      capturedScope,
      lifecycle
    );

  /** Says, in the always-mounted status region, what a landed pin-switch change means. Only a
   *  `set` that carried `include_pinned` has one: a window-only change says nothing about pins. */
  const announceInclude = (request: ExpirationRequest | undefined, landed: ExpirationPolicy) => {
    if (request?.mode !== 'set' || request.include_pinned === undefined) return;
    setAnnounced({ revision: landed.revision, include: request.include_pinned });
    setStatusText(request.include_pinned ? PINNED_EXPIRY_ON_STATUS : PINNED_EXPIRY_OFF_STATUS);
  };

  /** Settles an answer to the change, or returns the sentence for a failure the user can retry. */
  const settlePolicyResult = async (
    result: ExpirationMutationResult,
    operation: number,
    capturedScope: string,
    lifecycle: ReturnType<typeof captureAuthLifecycle>,
    request?: ExpirationRequest
  ): Promise<string | null> => {
    if (result.kind === 'ok') {
      announceInclude(request, result.policy);
      closeConfirmation();
      return null;
    }
    if (result.kind === 'partial') {
      const reread = await onRefresh();
      if (!isCurrent(operation, capturedScope, lifecycle)) return null;
      if (!(reread.kind === 'fresh' && matchesPartialPolicy(reread, result.candidate))) {
        setMutationBlocked(true);
        setNeedsRefresh(true);
        setError('Refresh the policy before continuing.');
      }
      closeConfirmation();
      return null;
    }
    if (handleRejectedPolicyChange(result)) return null;
    return semanticError(result) ?? 'This change could not be applied.';
  };

  /**
   * The one send path for a staged change, whether it was confirmed in a modal or applied directly
   * (an OFF-only pin change has no modal). Every mutation passes the same fence: a fresh read, the
   * baseline it was staged against, and the revision (X11). A change that skips the modal skips
   * only the question, never the checks. Throws the sentence for a failure the person can retry.
   */
  const sendPolicyChange = async (stagedBaseline: Baseline, stagedChoice: Choice) => {
    // The window to send: the staged one, or the applied one for a pins-only change (which
    // staged no window). Off (`null`) with nothing staged is not a change this path sends.
    const requestWindow = draftWindow === undefined ? appliedWindow : draftWindow;
    if (requestWindow === undefined) return;
    if (!matchesCurrentPolicyBaseline(stagedBaseline, scope, policy)) return;
    const operation = ++operationRef.current;
    const capturedScope = `${scope.kind}:${scope.id}`;
    const lifecycle = captureAuthLifecycle();
    const fresh = await onRefresh();
    if (
      !isCurrentBaselineOperation(
        mountedRef.current,
        operation,
        operationRef.current,
        scopeRef.current,
        capturedScope,
        lifecycle,
        stagedBaseline
      )
    )
      return;
    if (fresh.kind !== 'fresh') {
      setError('Refresh the policy before continuing.');
      throw new Error('Refresh the policy before continuing.');
    }
    if (!matchesPolicyBaseline(fresh.policy, stagedBaseline)) {
      closeConfirmation();
      setNeedsRefresh(true);
      setError('The policy changed. Review the refreshed timer before continuing.');
      return;
    }
    const request = policyRequest(requestWindow, stagedChoice, includeToSend);
    // Captured before the send and sent against, so a challenge carries the account and server
    // it asked (C82).
    const context = captureApiRequestContext();
    const result = await onApplyPolicy(request, context);
    if (!isCurrent(operation, capturedScope, lifecycle)) return;
    const refusal = result.kind === 'gated' ? stepUpSeed(result.status, result.body) : null;
    if (refusal !== null && request.mode === 'set') {
      // A challenge that lands after a server change belongs to the old server: no dialog.
      if (!apiRequestContextIsCurrent(context)) throw new Error(FIRST_SEND_SESSION_CHANGED);
      handOff({
        request,
        pinsOn: request.include_pinned === true,
        windowChange: request.window_seconds !== stagedBaseline.windowSeconds,
        refusal,
        sessionKey,
        context,
      });
      return;
    }
    const failure = await settlePolicyResult(result, operation, capturedScope, lifecycle, request);
    if (failure !== null) throw new Error(failure);
  };

  const confirmPolicyChange = async () => {
    if (
      !hasPendingChange ||
      !choice ||
      !acknowledged ||
      !baseline ||
      !canEdit ||
      forbidden ||
      mutationBlocked ||
      needsRefresh
    )
      return;
    await sendPolicyChange(baseline, choice);
    // An include-only change leaves no Apply button to return to (it goes with the staged
    // draft), and the confirmation's own focus restore targets that button.
    if (!hasWindowChange && mountedRef.current) setOptionFocusTick((tick) => tick + 1);
  };

  /** OFF-only: turning pinned expiry off asks nothing (it can only keep more), so it skips the
   *  modal but not the fence. It is `new_only` by construction, the window being unchanged. */
  const applyIncludeDirectly = async () => {
    if (!editable || !hasIncludeChange || hasWindowChange || applying) return;
    const directBaseline = policyBaseline(scope, policy);
    if (directBaseline === null) return;
    setApplying(true);
    setError(null);
    try {
      await sendPolicyChange(directBaseline, 'new_only');
      if (mountedRef.current) setOptionFocusTick((tick) => tick + 1);
    } catch (error_) {
      // A modal shows a retryable failure inside itself; with none, the editor's alert says it.
      if (mountedRef.current) {
        setError(error_ instanceof Error ? error_.message : 'This change could not be applied.');
      }
    } finally {
      if (mountedRef.current) setApplying(false);
    }
  };

  /** Apply routes by WHAT changed (#3459): a window change keeps its confirmation (plus one pin
   *  line); pins turning ON get their own, because they delete; pins turning OFF delete nothing
   *  and apply at once. */
  const pressApply = () => {
    if (!editable || applying || !hasPendingChange) return;
    if (hasWindowChange) openConfirmation();
    else if (pinsOnlyOn) openPinsConfirmation();
    else void applyIncludeDirectly();
  };
  /** The dialog's send: the frozen change again, with the proven code, admitted against `context`. */
  const resendWithCode = async (
    mfaCode: string | undefined,
    context: ApiRequestContext
  ): Promise<DangerousActionSendResult> => {
    if (stepUp === null) return { kind: 'aborted' };
    const operation = ++operationRef.current;
    const capturedScope = `${scope.kind}:${scope.id}`;
    const lifecycle = captureAuthLifecycle();
    const { request } = stepUp;
    const result = await onApplyPolicy(
      mfaCode === undefined ? request : { ...request, mfa_code: mfaCode },
      context
    );
    // `aborted` is the fence refusing a stale `context`: nothing was sent, so nothing is settled.
    if (result.kind === 'aborted' || !isCurrent(operation, capturedScope, lifecycle))
      return { kind: 'aborted' };
    // A plain 403 locks the editor, and the dialog cannot say so: it would keep "The server
    // refused this change." beside a live primary that sends the same change again. The dialog
    // ends instead, and the editor's own alert is the one place the refusal is said.
    if (isRejectedReason(result, 'forbidden')) {
      setForbidden(true);
      closeConfirmation();
      setError(semanticError(result));
      endStepUp();
      return { kind: 'aborted' };
    }
    const answer = verificationAnswer(result);
    if (answer !== null) return answer;
    const failure = await settlePolicyResult(result, operation, capturedScope, lifecycle, request);
    // No HTTP status: this is the editor's own sentence, worded for the dialog's banner.
    return failure === null
      ? { kind: 'ok' }
      : { kind: 'refused', status: 0, body: { error: failure } };
  };
  const confirmResume = async () => {
    if (
      !acknowledged ||
      !policy?.backfillPending ||
      !baseline ||
      !canEdit ||
      forbidden ||
      mutationBlocked ||
      needsRefresh ||
      baseline.scopeKey !== `${scope.kind}:${scope.id}` ||
      !isSameAuthLifecycle(baseline.lifecycle) ||
      baseline.revision !== policy.revision ||
      !baseline.backfillPending
    )
      return;
    const operation = ++operationRef.current;
    const capturedScope = `${scope.kind}:${scope.id}`;
    const lifecycle = captureAuthLifecycle();
    const revision = policy.revision;
    const fresh = await onRefresh();
    if (
      !mountedRef.current ||
      operation !== operationRef.current ||
      `${scopeRef.current.kind}:${scopeRef.current.id}` !== capturedScope ||
      !isSameAuthLifecycle(lifecycle) ||
      !isSameAuthLifecycle(baseline.lifecycle)
    )
      return;
    if (
      fresh.kind !== 'fresh' ||
      !fresh.policy.backfillPending ||
      fresh.policy.revision !== revision
    ) {
      closeConfirmation();
      setNeedsRefresh(true);
      setError('The policy changed. Review the refreshed timer before continuing.');
      return;
    }
    const result = await onApplyPolicy({ mode: 'resume', revision });
    if (
      !mountedRef.current ||
      operation !== operationRef.current ||
      `${scopeRef.current.kind}:${scopeRef.current.id}` !== capturedScope ||
      !isSameAuthLifecycle(lifecycle)
    )
      return;
    if (result.kind === 'ok') {
      closeConfirmation();
      return;
    }
    if (result.kind === 'partial') {
      const reread = await refresh();
      if (
        !mountedRef.current ||
        `${scopeRef.current.kind}:${scopeRef.current.id}` !== capturedScope ||
        !isSameAuthLifecycle(lifecycle) ||
        reread.kind === 'superseded'
      )
        return;
      if (!(reread.kind === 'fresh' && matchesPartialPolicy(reread, result.candidate))) {
        setMutationBlocked(true);
        setNeedsRefresh(true);
        setError('Refresh the policy before continuing.');
      }
      closeConfirmation();
      return;
    }
    if (isRejectedReason(result, 'notFound')) {
      void refresh();
      onClose();
      return;
    }
    if (requiresPolicyRefresh(result)) {
      setMutationBlocked(true);
      setNeedsRefresh(true);
      closeConfirmation();
      setError(semanticError(result) ?? 'This change could not be applied.');
      return;
    }
    if (isRejectedReason(result, 'forbidden')) setForbidden(true);
    const message = semanticError(result) ?? 'This change could not be applied.';
    throw new Error(message);
  };
  const confirmationOptions = useMemo(() => {
    const acknowledgement = (
      <label>
        <input
          type="checkbox"
          checked={acknowledged}
          onChange={(event) => setAcknowledged(event.target.checked)}
        />{' '}
        I understand deleted messages cannot be recovered.
      </label>
    );
    // Pins turning ON: no radio, because the choice is fixed (`new_only`). The warning block
    // carries `--danger` on its glyph, border and tint only; the sentence is `--text-primary`
    // (`--danger` as label text fails AA in 9 of 30 scheme x theme combinations).
    if (pinsOnlyOn) {
      return (
        <div className="message-expiration-options">
          <div className="message-expiration-pin-warning">
            <Pin size={18} className="message-expiration-pin-warning__icon" aria-hidden="true" />
            <p>
              Deleted pinned messages are gone for good, along with their text, images, GIFs,
              attachments, and emojis.
            </p>
          </div>
          {acknowledgement}
        </div>
      );
    }
    // The one pin line a window change carries. Absent when nothing true can be said about pins.
    const pinParagraph =
      pinLine === null ? null : <p className="message-expiration-pin-line">{pinLine}</p>;
    if (draftWindow === null) {
      // No pin line here: a `clear` has no pin semantics (`pinConfirmationLine` is null for Off).
      return (
        <div className="message-expiration-options">
          <label>
            <input
              type="radio"
              name="expiration-choice"
              checked={choice === 'clear_pending'}
              onChange={() => setPolicyChoice('clear_pending')}
            />{' '}
            Cancel scheduled deletions
          </label>
          <label>
            <input
              type="radio"
              name="expiration-choice"
              checked={choice === 'leave_pending'}
              onChange={() => setPolicyChoice('leave_pending')}
            />{' '}
            Keep scheduled deletions
          </label>
          {acknowledgement}
        </div>
      );
    }
    return (
      <div className="message-expiration-options">
        {pinParagraph}
        <label>
          <input
            type="radio"
            name="expiration-choice"
            checked={choice === 'apply'}
            onChange={() => setPolicyChoice('apply')}
          />{' '}
          Apply to existing messages
        </label>
        <label>
          <input
            type="radio"
            name="expiration-choice"
            checked={choice === 'new_only'}
            onChange={() => setPolicyChoice('new_only')}
          />{' '}
          Only new messages
        </label>
        {acknowledgement}
      </div>
    );
  }, [acknowledged, choice, draftWindow, pinLine, pinsOnlyOn]);

  return (
    <section className="message-expiration-editor" aria-label="Message expiration">
      {!editable && <p id={stopDescriptionId}>{lockReason}</p>}
      {policyState === 'loading' && <p>Loading message expiration…</p>}
      {policyState === 'unavailable' && (
        <>
          <p>Message expiration unavailable</p>
          <button type="button" onClick={() => void refresh()}>
            Retry
          </button>
        </>
      )}
      {policyState === 'ready' && policy?.backfillPending && (
        <>
          <p>Still processing existing messages.</p>
          <button
            type="button"
            disabled={
              !canEdit || forbidden || mutationBlocked || needsRefresh || policyState !== 'ready'
            }
            onClick={() => {
              operationRef.current += 1;
              setAcknowledged(false);
              setBaseline(policyBaseline(scope, policy));
              setResumeOpen(true);
            }}
          >
            Resume processing
          </button>
        </>
      )}
      {error && (
        <p className="message-expiration-error" role="alert">
          {error}
        </p>
      )}
      {needsRefresh && (
        <button type="button" onClick={() => void refresh()}>
          Refresh policy
        </button>
      )}
      {/* `aria-pressed` means IN FORCE, not staged. Tracking the staged choice instead was
          tried and reverted: a mutation that FAILS leaves the draft staged (the throwing path
          keeps the dialog open rather than calling `closeConfirmation`, which is what clears
          it), so the row would report a window the server had just refused as the pressed one
          — the editor asserting a change that did not happen. An sr-only "(current)" marker
          was also tried and removed, because it mutates the accessible NAME that every
          consumer queries by. The staged choice needs no separate announcement: the user
          just activated the stop, so focus is on it when Apply appears. */}
      <div ref={stopsRef} className="settings-tier-labels message-expiration-stops">
        {stops.map((stop) => (
          <button
            key={stop.label}
            type="button"
            className={[
              'settings-tier-label',
              'message-expiration-stop',
              selectedWindow === stop.value ? 'active' : '',
              hasWindowChange && draftWindow === stop.value ? 'pending' : '',
              appliedWindow === stop.value ? 'applied' : '',
            ]
              .filter(Boolean)
              .join(' ')}
            aria-pressed={appliedWindow === stop.value}
            aria-disabled={!stageable}
            aria-describedby={editable ? undefined : stopDescriptionId}
            onClick={() => selectWindow(stop.value)}
          >
            {stop.label}
          </button>
        ))}
      </div>
      <PinnedRetentionOption
        ref={optionRef}
        policy={policy}
        policyState={viewState}
        effectiveWindow={selectedWindow}
        checked={finalInclude}
        locked={!stageable}
        lockDescriptionId={stopDescriptionId}
        onChange={stageInclude}
      />
      {hasPendingChange && (
        <div ref={applyRef} className="message-expiration-apply">
          {/* `aria-disabled` while a direct apply is out, not `disabled`: this button is the
              focused element, and a natively disabled one would drop focus to <body>. */}
          <button
            type="button"
            className="message-expiration-apply-button"
            onClick={pressApply}
            aria-disabled={applying || undefined}
            disabled={!canEdit || forbidden || mutationBlocked || needsRefresh}
          >
            Apply
          </button>
          {/* "Discard", not "Cancel". The confirmation dialog this Apply opens has its own
              Cancel, and two buttons with the same accessible name in one view is ambiguous
              to a screen reader — and to anyone reading the row. This one abandons a staged
              choice; that one dismisses a dialog. Different acts, different words. */}
          <button
            type="button"
            className="message-expiration-cancel-button"
            onClick={() => {
              setDraftWindow(undefined);
              setDraftInclude(undefined);
            }}
          >
            Discard
          </button>
        </div>
      )}
      {/* Always mounted, even when empty: a live region must exist BEFORE its text changes, or a
          screen reader has nothing registered to announce. Says what a landed pin-switch change
          means, in the spec's words; it is not a second description of the switch. */}
      <output className="message-expiration-status">
        {announcementSuperseded ? '' : statusText}
      </output>
      <ConfirmActionModal
        key={`policy:${confirmationIdentity}`}
        isOpen={confirmationShown && stepUp === null && !ending}
        title={confirmationTitle}
        message={disclosure}
        confirmLabel={confirmationLabel}
        loadingLabel="Applying…"
        extraContent={confirmationOptions}
        confirmDisabled={
          !choice ||
          !acknowledged ||
          !baseline ||
          !canEdit ||
          forbidden ||
          mutationBlocked ||
          needsRefresh ||
          baseline.scopeKey !== `${scope.kind}:${scope.id}` ||
          !isSameAuthLifecycle(baseline.lifecycle) ||
          baseline.revision !== policy?.revision ||
          baseline.windowSeconds !== policy?.windowSeconds ||
          baseline.includePinned !== policy?.includePinned ||
          baseline.backfillPending !== policy?.backfillPending
        }
        onConfirm={confirmPolicyChange}
        onClose={confirmClosed}
      />
      <ConfirmActionModal
        key={`resume:${confirmationIdentity}`}
        isOpen={resumeOpen}
        title="Resume message expiration processing"
        message="Resume processing the current message expiration policy."
        confirmLabel="Resume processing"
        loadingLabel="Applying…"
        extraContent={
          <label>
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={(event) => setAcknowledged(event.target.checked)}
            />{' '}
            I understand deleted messages cannot be recovered.
          </label>
        }
        confirmDisabled={
          !acknowledged ||
          !baseline ||
          !canEdit ||
          forbidden ||
          mutationBlocked ||
          needsRefresh ||
          baseline.scopeKey !== `${scope.kind}:${scope.id}` ||
          !isSameAuthLifecycle(baseline.lifecycle) ||
          baseline.revision !== policy?.revision ||
          !policy?.backfillPending
        }
        onConfirm={confirmResume}
        onClose={dismissConfirmation}
      />
      <DangerousActionStepUpDialog
        isOpen={stepUp !== null}
        purpose="channels.expiration_shorten"
        seed={stepUp?.refusal}
        capture={stepUp?.context}
        intro={stepUpWords.intro}
        primaryLabel={stepUpWords.primaryLabel}
        busyLabel="Applying…"
        send={resendWithCode}
        describeFailure={describeShortenFailure}
        onSuccess={endStepUp}
        onClose={endStepUp}
        onSetUpVerification={() => {
          void openVerificationSetup({ returnTo: { kind: 'chat' }, closeHost: endStepUp });
        }}
        // The confirmation that had focus is gone: back to Apply while a change is still staged,
        // else to the pin switch (an include-only change leaves no Apply), else to the stops.
        focusFallback={() =>
          focusTargetIn(applyRef.current) ??
          focusTargetIn(optionRef.current) ??
          focusTargetIn(stopsRef.current)
        }
      />
    </section>
  );
}
