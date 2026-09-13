import { useEffect, useId, useMemo, useRef, useState } from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import type {
  ExpirationMutationResult,
  ExpirationPolicy,
  ExpirationPolicyReadResult,
  ExpirationRequest,
  ExpirationScope,
  ExpirationWindowSeconds,
} from '../../services/messaging/expirationPolicyApi';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import { useAuthStore } from '../../stores/auth/authStore';
import '../common/audioQualitySlider.css';
import './messageExpiration.css';

export interface MessageExpirationEditorProps {
  scope: ExpirationScope;
  policy: ExpirationPolicy | null;
  policyState: 'loading' | 'ready' | 'unavailable';
  canEdit: boolean;
  lockedDescription: string;
  onRefresh: () => Promise<ExpirationPolicyReadResult>;
  onApplyPolicy: (request: ExpirationRequest) => Promise<ExpirationMutationResult>;
  onMarkSeen: (revision: number) => void;
  onClose: () => void;
}

type Choice = 'apply' | 'new_only' | 'clear_pending' | 'leave_pending';
type Baseline = {
  scopeKey: string;
  lifecycle: ReturnType<typeof captureAuthLifecycle>;
  revision: number;
  windowSeconds: ExpirationWindowSeconds | null;
  backfillPending: boolean;
};

const stops: Array<{ label: string; value: ExpirationWindowSeconds | null }> = [
  { label: 'Off', value: null },
  { label: '1 hour', value: 3600 },
  { label: '24 hours', value: 86400 },
  { label: '7 days', value: 604800 },
  { label: '30 days', value: 2592000 },
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
  return baseline.backfillPending === policy?.backfillPending;
}

function policyRequest(
  draftWindow: ExpirationWindowSeconds | null,
  choice: Choice
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
  };
}

function matchesPartialPolicy(
  reread: ExpirationPolicyReadResult,
  candidate: ExpirationPolicy | undefined
): boolean {
  return (
    reread.kind === 'fresh' &&
    reread.policy.revision === candidate?.revision &&
    reread.policy.windowSeconds === candidate?.windowSeconds
  );
}

function matchesPolicyBaseline(policy: ExpirationPolicy, baseline: Baseline): boolean {
  return (
    policy.revision === baseline.revision &&
    policy.windowSeconds === baseline.windowSeconds &&
    !policy.backfillPending
  );
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

export default function MessageExpirationEditor({
  scope,
  policy,
  policyState,
  canEdit,
  lockedDescription,
  onRefresh,
  onApplyPolicy,
  onMarkSeen,
  onClose,
}: Readonly<MessageExpirationEditorProps>) {
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const [draftWindow, setDraftWindow] = useState<ExpirationWindowSeconds | null | undefined>();
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
  const scopeRef = useRef(scope);
  const authGenerationRef = useRef(authGeneration);
  const descriptionId = useId();
  const policyBaselineIdentity = policy
    ? `${policy.revision}:${policy.windowSeconds ?? 'off'}:${policy.backfillPending}`
    : 'none';
  const consentBaselineIdentity = baseline
    ? `${baseline.revision}:${baseline.windowSeconds ?? 'off'}:${baseline.backfillPending}`
    : 'none';
  const confirmationIdentity = `${scope.kind}:${scope.id}:${authGeneration}:${policyBaselineIdentity}:${consentBaselineIdentity}`;
  const confirmationIdentityRef = useRef(confirmationIdentity);

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
  useEffect(
    () => () => {
      setChoice(undefined);
      setAcknowledged(false);
      setDraftWindow(undefined);
      setBaseline(null);
      setConfirmationOpen(false);
      setResumeOpen(false);
    },
    [policyBaselineIdentity]
  );
  const editable =
    canEdit &&
    !forbidden &&
    policyState === 'ready' &&
    !policy?.backfillPending &&
    !mutationBlocked &&
    !needsRefresh;
  const stopDescriptionId = descriptionId;
  const lockReason = expirationLockReason(
    policyState,
    policy,
    forbidden,
    mutationBlocked,
    needsRefresh,
    lockedDescription
  );
  const selectedDuration =
    stops.find((stop) => stop.value === draftWindow)?.label ?? 'the selected time';
  const disclosure =
    draftWindow === null
      ? 'Cancel scheduled deletions stops applicable upcoming schedules and does not restore messages already deleted. Keep scheduled deletions leaves already scheduled messages to delete while future messages get no timer. Text, images, GIFs, attachments, and emojis already deleted cannot be recovered.'
      : `Applying to existing messages means eligible existing and future text, images, GIFs, attachments, and emojis are permanently deleted after ${selectedDuration} and cannot be recovered. New-only leaves already scheduled messages unchanged.`;

  const closeConfirmation = () => {
    if (
      !mountedRef.current ||
      authGenerationRef.current !== authGeneration ||
      confirmationIdentityRef.current !== confirmationIdentity ||
      `${scopeRef.current.kind}:${scopeRef.current.id}` !== `${scope.kind}:${scope.id}`
    )
      return;
    setConfirmationOpen(false);
    setResumeOpen(false);
    setChoice(undefined);
    setAcknowledged(false);
    setDraftWindow(undefined);
    setBaseline(null);
  };
  const selectWindow = (windowSeconds: ExpirationWindowSeconds | null) => {
    if (!editable || windowSeconds === policy?.windowSeconds) return;
    operationRef.current += 1;
    setDraftWindow(windowSeconds);
    setChoice(undefined);
    setAcknowledged(false);
    setError(null);
    setNeedsRefresh(false);
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

  const confirmPolicyChange = async () => {
    if (
      draftWindow === undefined ||
      !choice ||
      !acknowledged ||
      !baseline ||
      !canEdit ||
      forbidden ||
      mutationBlocked ||
      needsRefresh
    )
      return;
    if (!matchesCurrentPolicyBaseline(baseline, scope, policy)) return;
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
        baseline
      )
    )
      return;
    if (fresh.kind !== 'fresh') {
      setError('Refresh the policy before continuing.');
      throw new Error('Refresh the policy before continuing.');
    }
    if (!matchesPolicyBaseline(fresh.policy, baseline)) {
      closeConfirmation();
      setNeedsRefresh(true);
      setError('The policy changed. Review the refreshed timer before continuing.');
      return;
    }
    const result = await onApplyPolicy(policyRequest(draftWindow, choice));
    if (
      !isCurrentOperation(
        mountedRef.current,
        operation,
        operationRef.current,
        scopeRef.current,
        capturedScope,
        lifecycle
      )
    )
      return;
    if (result.kind === 'ok') {
      closeConfirmation();
      return;
    }
    if (result.kind === 'partial') {
      const reread = await onRefresh();
      if (
        !isCurrentOperation(
          mountedRef.current,
          operation,
          operationRef.current,
          scopeRef.current,
          capturedScope,
          lifecycle
        )
      )
        return;
      if (reread.kind === 'fresh' && matchesPartialPolicy(reread, result.candidate)) {
        onMarkSeen(reread.policy.revision);
      } else {
        setMutationBlocked(true);
        setNeedsRefresh(true);
        setError('Refresh the policy before continuing.');
      }
      closeConfirmation();
      return;
    }
    if (handleRejectedPolicyChange(result)) return;
    const message = semanticError(result) ?? 'This change could not be applied.';
    throw new Error(message);
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
      if (reread.kind === 'fresh' && matchesPartialPolicy(reread, result.candidate)) {
        onMarkSeen(reread.policy.revision);
      } else {
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
  const confirmationOptions = useMemo(
    () =>
      draftWindow === null ? (
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
          <label>
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={(event) => setAcknowledged(event.target.checked)}
            />{' '}
            I understand deleted messages cannot be recovered.
          </label>
        </div>
      ) : (
        <div className="message-expiration-options">
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
          <label>
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={(event) => setAcknowledged(event.target.checked)}
            />{' '}
            I understand deleted messages cannot be recovered.
          </label>
        </div>
      ),
    [acknowledged, choice, draftWindow]
  );

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
      <div className="settings-tier-labels message-expiration-stops">
        {stops.map((stop) => (
          <button
            key={stop.label}
            type="button"
            className={`settings-tier-label message-expiration-stop ${policyState === 'ready' && policy?.windowSeconds === stop.value ? 'active' : ''}`}
            aria-pressed={policyState === 'ready' && policy?.windowSeconds === stop.value}
            aria-disabled={!editable}
            aria-describedby={editable ? undefined : stopDescriptionId}
            onClick={() => selectWindow(stop.value)}
          >
            {stop.label}
          </button>
        ))}
      </div>
      <ConfirmActionModal
        key={`policy:${confirmationIdentity}`}
        isOpen={confirmationOpen}
        title={draftWindow === null ? 'Turn off message expiration' : 'Change message expiration'}
        message={disclosure}
        confirmLabel={draftWindow === null ? 'Turn off timer' : 'Apply timer'}
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
          baseline.backfillPending !== policy?.backfillPending
        }
        onConfirm={confirmPolicyChange}
        onClose={closeConfirmation}
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
        onClose={closeConfirmation}
      />
    </section>
  );
}
