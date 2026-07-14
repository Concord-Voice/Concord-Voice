// Partial module of voiceService — owns the DM call state machine for
// ringing, accepting, declining, canceling, and the 4 incoming WS event
// handlers. Per spec [internal]specs/2026-05-27-1209-dm-group-voice-calls-design.md §7.1.
//
// Architecture: extends the monolithic voiceService singleton via this
// partial-module file (chosen over separate dmCallService or generic
// CallSession interface at brainstorming time — see spec §3.8). The
// methods here are exported as free functions; the main voiceService.ts
// imports + re-exports them so callers see one cohesive singleton.
//
// Skeleton commit (Task D3): types + method stubs. Caller flow + callee
// flow + 4 WS handler implementations come in Tasks E1, E2, E3 per the
// implementation plan.

import type {
  DMVoiceCallInvitedPayload,
  DMVoiceCallCanceledPayload,
  DMVoiceCallDeclinedPayload,
  DMVoiceCallTimedOutPayload,
} from '../../types/ws-events';
import { useVoiceStore } from '../../stores/voiceStore';
import { useDMStore } from '../../stores/dmStore';
import { apiFetch } from '../apiClient';
import { notificationSoundService } from '../notificationSoundService';
import { voiceService } from '../voiceService';

/**
 * sanitizeErrForLog strips ASCII control characters from an unknown error
 * value and caps the resulting string at 200 chars. This is the same
 * pattern voiceService.ts uses for Sonar S4790 — taint-tracking treats
 * the regex + length cap as a sanitization sink, and removing CR/LF
 * blocks log-injection attacks via thrown error strings.
 *
 * Defined locally here (rather than imported from voiceService) to
 * avoid the circular-import that would arise from this partial-module
 * file importing back into its own host singleton.
 */
function sanitizeErrForLog(err: unknown): string {
  const raw = err instanceof Error ? err.message : 'non-Error thrown';
  return raw.replace(/[\x00-\x1F\x7F]/g, '').slice(0, 200);
}

/**
 * CallerInfo is the minimal user-identity payload received in the
 * dm_voice_call_invited event and stored in the incoming-ringing
 * CallState. The IncomingCallBanner renders displayName ?? username +
 * the avatar (when present).
 */
export interface CallerInfo {
  userId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
}

/**
 * CallState is a discriminated union describing the renderer-side DM
 * voice call state machine. Six terminal/transient states per spec §5.2.
 *
 *   idle              → no call activity
 *   outgoing-ringing  → caller is awaiting accept/decline/timeout
 *   incoming-ringing  → callee sees IncomingCallBanner, ringtone looping
 *   joining           → accepted ring is establishing the media session
 *   in-call           → both sides have completed POST /voice/join
 *   ending            → hang-up initiated, brief fade-out before idle
 *
 * The discriminated union shape lets TypeScript narrow at consumer
 * sites (e.g., `if (state.kind === 'outgoing-ringing') { state.ringId... }`).
 */
export type CallState =
  | { kind: 'idle' }
  | {
      kind: 'outgoing-ringing';
      conversationId: string;
      ringId: string;
      calleeUserIds: string[];
      startedAt: number;
      /**
       * Group decline tally (#1219 R2): user IDs of callees who have
       * declined this ring. Renderer-internal — CallState is not a wire
       * type, so this never appears in a WS payload. For DM 1:1 it stays
       * empty (a single decline transitions straight to idle). For groups
       * the caller stays in outgoing-ringing while this accumulates;
       * terminal idle is driven by handleCallCanceled('all_declined').
       */
      declinedUserIds: string[];
    }
  | {
      kind: 'incoming-ringing';
      conversationId: string;
      ringId: string;
      caller: CallerInfo;
      expiresAt: number;
      /**
       * Group context (#1219 R10): whether this incoming ring is for a group
       * DM. Threaded from the now-emitted `is_group` boolean on the
       * dm_voice_call_invited event (B3). The IncomingCallBanner uses this to
       * show group context (the group name from dmStore) so a callee can
       * distinguish a group ring from a 1:1 ring. Renderer-internal — CallState
       * is not a wire type.
       */
      isGroup: boolean;
    }
  | { kind: 'joining'; conversationId: string; ringId: string }
  | { kind: 'in-call' }
  | { kind: 'ending' };

type OutgoingRingingState = Extract<CallState, { kind: 'outgoing-ringing' }>;

// The control plane expires unanswered rings after 45 seconds. A renderer
// request that never settles must not own the global call slot longer than
// the ring it attempted to create. The request itself remains alive so a
// later response can still be cleaned up by its exact server-issued ring ID.
const RING_REQUEST_OWNERSHIP_TIMEOUT_MS = 45_000;

interface PendingRingOperation {
  state: OutgoingRingingState;
  cancelRequested: boolean;
  ownershipRevision: number;
  ownershipTimer: ReturnType<typeof setTimeout> | null;
  reconcileLateEvents: boolean;
  queuedEvents: Array<{
    ringId: string;
    replay: () => void;
    reconcilesFailedCancel: boolean;
  }>;
}

interface RingResponsePayload {
  ring_id: string;
  ring_started_at: string;
  ringing_user_ids: string[];
}

function queueEventForUnresolvedRing(
  conversationId: string,
  ringId: string,
  replay: () => void,
  reconcilesFailedCancel = false
): boolean {
  const operations = unresolvedRingOperations.get(conversationId);
  if (!operations) return false;

  for (const operation of operations) {
    operation.queuedEvents.push({ ringId, replay, reconcilesFailedCancel });
  }
  return operations.size > 0;
}

const pendingRingOperations = new Map<string, PendingRingOperation>();
const unresolvedRingOperations = new Map<string, Set<PendingRingOperation>>();
let callOwnershipRevision = 0;

function trackUnresolvedRingOperation(
  conversationId: string,
  operation: PendingRingOperation
): void {
  const operations = unresolvedRingOperations.get(conversationId) ?? new Set();
  operations.add(operation);
  unresolvedRingOperations.set(conversationId, operations);
}

function finishUnresolvedRingOperation(
  conversationId: string,
  operation: PendingRingOperation
): void {
  const operations = unresolvedRingOperations.get(conversationId);
  if (!operations) return;
  operations.delete(operation);
  if (operations.size === 0) unresolvedRingOperations.delete(conversationId);
}

function finishPendingRingOperation(conversationId: string, operation: PendingRingOperation): void {
  if (operation.ownershipTimer !== null) {
    clearTimeout(operation.ownershipTimer);
    operation.ownershipTimer = null;
  }
  if (pendingRingOperations.get(conversationId) === operation) {
    pendingRingOperations.delete(conversationId);
  }
}

function expirePendingRingOwnership(conversationId: string, operation: PendingRingOperation): void {
  if (pendingRingOperations.get(conversationId) !== operation) return;

  // Retire only local ownership. The still-running request keeps this
  // operation in its closure and will exact-cancel the returned ring ID.
  operation.cancelRequested = true;
  finishPendingRingOperation(conversationId, operation);
  if (useVoiceStore.getState().callState === operation.state) {
    notificationSoundService.stopLoop('call-outgoing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
  }
}

useVoiceStore.subscribe((state, previousState) => {
  const previousCall = previousState.callState;
  if (previousCall.kind === 'idle' && state.callState.kind !== 'idle') {
    callOwnershipRevision += 1;
  }
  if (state.callState.kind !== 'idle' || previousCall.kind !== 'outgoing-ringing') return;

  const operation = pendingRingOperations.get(previousCall.conversationId);
  if (operation?.state !== previousCall || operation.cancelRequested) return;

  // A store/account reset must not leave a hung /ring request owning every
  // future call. Mark it for exact cleanup if a response arrives, then release
  // local ownership immediately.
  operation.cancelRequested = true;
  operation.reconcileLateEvents = false;
  finishPendingRingOperation(previousCall.conversationId, operation);
});

function ringRequestBody(ringId: string): { body?: string } {
  return ringId ? { body: JSON.stringify({ ring_id: ringId }) } : {};
}

async function cancelRingRequest(conversationId: string, ringId: string): Promise<Response> {
  return apiFetch(`/api/v1/dm/conversations/${conversationId}/voice/cancel`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    ...ringRequestBody(ringId),
  });
}

function resolvedOutgoingState(
  conversationId: string,
  data: RingResponsePayload
): OutgoingRingingState {
  return {
    kind: 'outgoing-ringing',
    conversationId,
    ringId: data.ring_id,
    calleeUserIds: data.ringing_user_ids,
    startedAt: Date.parse(data.ring_started_at),
    declinedUserIds: [],
  };
}

function rollbackPendingRing(
  conversationId: string,
  operation: PendingRingOperation,
  optimisticState: OutgoingRingingState
): void {
  finishUnresolvedRingOperation(conversationId, operation);
  finishPendingRingOperation(conversationId, operation);
  if (useVoiceStore.getState().callState !== optimisticState) return;
  notificationSoundService.stopLoop('call-outgoing');
  useVoiceStore.getState().setCallState({ kind: 'idle' });
}

async function reconcileCanceledRingResponse(
  conversationId: string,
  operation: PendingRingOperation,
  data: RingResponsePayload
): Promise<void> {
  let cancelResponse: Response | undefined;
  try {
    cancelResponse = await cancelRingRequest(conversationId, data.ring_id);
  } catch (err) {
    console.error('Late DM voice cancel POST failed:', sanitizeErrForLog(err));
  }

  const queuedEvents = operation.queuedEvents.filter(
    (event) => event.ringId === data.ring_id && event.reconcilesFailedCancel
  );
  const canReconcileAuthoritativeEvent =
    cancelResponse !== undefined &&
    !cancelResponse.ok &&
    operation.reconcileLateEvents &&
    callOwnershipRevision === operation.ownershipRevision &&
    useVoiceStore.getState().callState.kind === 'idle' &&
    queuedEvents.length > 0;

  finishUnresolvedRingOperation(conversationId, operation);
  finishPendingRingOperation(conversationId, operation);
  if (!canReconcileAuthoritativeEvent) return;

  // The exact cancel lost because the server already completed this ring.
  // Restore correlated state long enough to replay the authoritative event.
  useVoiceStore.getState().setCallState(resolvedOutgoingState(conversationId, data));
  for (const event of queuedEvents) event.replay();
}

async function cancelSupersededRingResponse(
  conversationId: string,
  operation: PendingRingOperation,
  ringId: string
): Promise<void> {
  try {
    await cancelRingRequest(conversationId, ringId);
  } catch (err) {
    console.error('Superseded DM voice cancel POST failed:', sanitizeErrForLog(err));
  } finally {
    finishUnresolvedRingOperation(conversationId, operation);
    finishPendingRingOperation(conversationId, operation);
  }
}

function settleResolvedRing(
  conversationId: string,
  operation: PendingRingOperation,
  data: RingResponsePayload
): void {
  finishUnresolvedRingOperation(conversationId, operation);
  finishPendingRingOperation(conversationId, operation);
  useVoiceStore.getState().setCallState(resolvedOutgoingState(conversationId, data));
  for (const event of operation.queuedEvents) {
    if (event.ringId === data.ring_id) event.replay();
  }
}

// ── Public caller-side methods (Task E1 implements) ────────────────────

/**
 * initiateDMCall starts an outgoing DM voice call ring per spec §7.2.
 *
 * Optimistic state transition: set callState to outgoing-ringing BEFORE
 * the POST so the UI surfaces the OutgoingCallModal immediately. Then
 * POST /ring. On success, update state with the server-returned ring_id.
 * On failure, roll callState back to idle and rethrow so the UI can
 * surface the error.
 *
 * Throws if another call owns the global renderer call state, or if the
 * conversation isn't loaded in dmStore (shouldn't happen at the call site —
 * DMConversationContextMenu only renders the Voice Call item from a row).
 */
export async function initiateDMCall(conversationId: string): Promise<void> {
  if (useVoiceStore.getState().callState.kind !== 'idle' || pendingRingOperations.size !== 0) {
    throw new Error('Another voice call is already in progress');
  }

  const dmState = useDMStore.getState();
  if (!dmState.conversations.some((c) => c.id === conversationId)) {
    throw new Error(`Conversation ${conversationId} not found in dmStore`);
  }

  // Optimistic state transition — UI surfaces OutgoingCallModal at this point
  const optimisticState: OutgoingRingingState = {
    kind: 'outgoing-ringing',
    conversationId,
    ringId: '',
    calleeUserIds: [],
    startedAt: Date.now(),
    declinedUserIds: [],
  };
  const operation: PendingRingOperation = {
    state: optimisticState,
    cancelRequested: false,
    ownershipRevision: 0,
    ownershipTimer: null,
    reconcileLateEvents: true,
    queuedEvents: [],
  };
  pendingRingOperations.set(conversationId, operation);
  trackUnresolvedRingOperation(conversationId, operation);
  useVoiceStore.getState().setCallState(optimisticState);
  operation.ownershipRevision = callOwnershipRevision;
  operation.ownershipTimer = setTimeout(
    () => expirePendingRingOwnership(conversationId, operation),
    RING_REQUEST_OWNERSHIP_TIMEOUT_MS
  );

  // Start ringback audio
  notificationSoundService.playLoop('call-outgoing');

  let response: Response;
  try {
    response = await apiFetch(`/api/v1/dm/conversations/${conversationId}/voice/ring`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    });
  } catch (err) {
    rollbackPendingRing(conversationId, operation, optimisticState);
    throw err;
  }

  if (!response.ok) {
    rollbackPendingRing(conversationId, operation, optimisticState);
    const errBody = await response.text().catch(() => '');
    throw new Error(`Failed to ring (HTTP ${response.status}): ${errBody}`);
  }

  let data: RingResponsePayload;
  try {
    data = (await response.json()) as typeof data;
  } catch (err) {
    rollbackPendingRing(conversationId, operation, optimisticState);
    throw err;
  }

  if (operation.cancelRequested) {
    await reconcileCanceledRingResponse(conversationId, operation, data);
    return;
  }

  const currentState = useVoiceStore.getState().callState;
  if (currentState !== optimisticState) {
    await cancelSupersededRingResponse(conversationId, operation, data.ring_id);
    return;
  }

  settleResolvedRing(conversationId, operation, data);
}

/**
 * cancelOutgoingCall lets the caller cancel their own ring before any
 * callee accepts. POSTs /voice/cancel; stops ringback; transitions to idle.
 *
 * Idempotent: if callState isn't outgoing-ringing, the /cancel POST is
 * skipped. An idle state still gets defensive ringback cleanup; joining and
 * in-call states keep ownership because the accepted ring is no longer
 * cancelable through this endpoint.
 */
export async function cancelOutgoingCall(): Promise<void> {
  const state = useVoiceStore.getState().callState;
  if (state.kind !== 'outgoing-ringing') {
    // Joining/in-call are deliberately non-cancelable through the ring endpoint.
    if (state.kind === 'idle') notificationSoundService.stopLoop('call-outgoing');
    return;
  }

  const { conversationId, ringId } = state;
  const pendingOperation = pendingRingOperations.get(conversationId);
  if (pendingOperation?.state === state) {
    pendingOperation.cancelRequested = true;
    // Release global call ownership immediately. The operation remains in
    // unresolvedRingOperations so its eventual response can still be canceled
    // by exact ring ID without blocking a successor call in the meantime.
    finishPendingRingOperation(conversationId, pendingOperation);
  }
  try {
    // A blank ID means the /ring request has not resolved. Sending the legacy
    // wildcard cancel could hit a successor ring; the request owner issues an
    // exact-ID cancel as soon as its response arrives instead.
    if (ringId) await cancelRingRequest(conversationId, ringId);
  } catch (err) {
    // POST failure is observable but doesn't block local cleanup. The
    // server may continue ringing callees until the 45s timeout, but the
    // renderer-side state still needs to transition to idle so the UI
    // recovers (silent-failure-hunter #1231 finding — bare `finally`
    // without `catch` made the POST error invisible).
    console.error('DM voice cancel POST failed:', sanitizeErrForLog(err));
  } finally {
    if (useVoiceStore.getState().callState === state) {
      notificationSoundService.stopLoop('call-outgoing');
      useVoiceStore.getState().setCallState({ kind: 'idle' });
    }
  }
}

// ── Public callee-side methods (Task E2 implements) ────────────────────

/**
 * acceptIncomingCall transitions the callee from incoming-ringing to
 * in-call per spec §7.3. Stops the ringtone, calls the existing
 * voiceService.joinChannel(convId, 'dm') to actually enter the room,
 * and transitions callState to 'in-call' on success.
 *
 * Idempotent: if callState isn't incoming-ringing, no-ops gracefully.
 * voiceService.joinChannel failures bubble up and the caller is
 * responsible for falling back to idle (rare path; left to the UI).
 */
export async function acceptIncomingCall(): Promise<void> {
  const state = useVoiceStore.getState().callState;
  if (state.kind !== 'incoming-ringing') {
    return;
  }
  const { conversationId } = state;
  const joiningState: CallState = {
    kind: 'joining',
    conversationId,
    ringId: state.ringId,
  };
  useVoiceStore.getState().setCallState(joiningState);
  notificationSoundService.stopLoop('call-ringing');
  try {
    await voiceService.joinChannel(conversationId, 'dm');
    if (useVoiceStore.getState().callState === joiningState) {
      useVoiceStore.getState().setCallState({ kind: 'in-call' });
    }
  } catch (err) {
    // joinChannel failure (mediasoup error, network drop, server 500): reset
    // to idle so the UI doesn't lock the callee in incoming-ringing with no
    // way out (silent-failure-hunter #1231 finding). The error is rethrown
    // so the caller (click handler in IncomingCallBanner) can surface it.
    console.error('DM voice accept failed:', sanitizeErrForLog(err));
    if (useVoiceStore.getState().callState === joiningState) {
      useVoiceStore.getState().setCallState({ kind: 'idle' });
    }
    throw err;
  }
}

/**
 * declineIncomingCall rejects an incoming call per spec §7.3. POSTs
 * /voice/decline; stops ringtone; transitions to idle regardless of
 * POST outcome (best-effort).
 */
export async function declineIncomingCall(): Promise<void> {
  const state = useVoiceStore.getState().callState;
  if (state.kind !== 'incoming-ringing') {
    if (state.kind === 'idle') notificationSoundService.stopLoop('call-ringing');
    return;
  }
  const { conversationId, ringId } = state;
  try {
    await apiFetch(`/api/v1/dm/conversations/${conversationId}/voice/decline`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      ...ringRequestBody(ringId),
    });
  } catch (err) {
    // Same posture as cancelOutgoingCall: observable POST error but local
    // state still transitions in `finally` so the UI recovers
    // (silent-failure-hunter #1231 finding).
    console.error('DM voice decline POST failed:', sanitizeErrForLog(err));
  } finally {
    if (useVoiceStore.getState().callState === state) {
      notificationSoundService.stopLoop('call-ringing');
      useVoiceStore.getState().setCallState({ kind: 'idle' });
    }
  }
}

// ── WS event handlers (Task E2 implements; E3 wires registration) ───────

/**
 * handleCallInvited is invoked by useWebSocketMessages when a
 * dm_voice_call_invited event arrives. Sets the incoming-ringing state
 * with caller identity from the payload and starts the ringtone loop.
 *
 * No-op if the callee is already in-call or has another incoming-ringing
 * (defensive — server should never send invited for an active-call user
 * since the existing voice_state_update path tracks that, but be safe).
 */
export function handleCallInvited(payload: DMVoiceCallInvitedPayload): void {
  const currentKind = useVoiceStore.getState().callState.kind;
  if (currentKind !== 'idle') {
    return;
  }
  useVoiceStore.getState().setCallState({
    kind: 'incoming-ringing',
    conversationId: payload.conversation_id,
    ringId: payload.ring_id,
    caller: {
      userId: payload.caller.user_id,
      username: payload.caller.username,
      displayName: payload.caller.display_name,
      avatarUrl: payload.caller.avatar_url,
    },
    expiresAt: Date.parse(payload.ring_started_at) + payload.ring_timeout_seconds * 1000,
    isGroup: payload.is_group,
  });
  notificationSoundService.playLoop('call-ringing');
}

/**
 * handleCallCanceled handles the dm_voice_call_canceled event per spec
 * §7.3 step 6. Behavior branches on the current callState + the cancel
 * reason (canceled_by).
 *
 * Critical caller-side path: when the caller is in outgoing-ringing AND
 * canceled_by === 'someone_accepted', this is the SIGNAL that the call
 * is now live. The caller fires its own voiceService.joinChannel(convId,
 * 'dm') to enter the room, then transitions to in-call. The async join
 * happens via a fire-and-forget promise — handlers are sync per the
 * WS event dispatcher.
 *
 * For all other paths (caller-cancel, all-declined, server-error,
 * or callee receiving any cancel), stop audio + transition to idle.
 */
export function handleCallCanceled(payload: DMVoiceCallCanceledPayload): void {
  const queuedForUnresolvedRing = queueEventForUnresolvedRing(
    payload.conversation_id,
    payload.ring_id,
    () => handleCallCanceled(payload),
    true
  );
  const store = useVoiceStore.getState();
  const state = store.callState;
  if (state.kind !== 'outgoing-ringing' && state.kind !== 'incoming-ringing') {
    return;
  }
  if (state.conversationId !== payload.conversation_id) {
    return;
  }
  if (state.kind === 'outgoing-ringing' && state.ringId === '' && queuedForUnresolvedRing) {
    return;
  }
  if (state.ringId !== payload.ring_id) {
    return;
  }

  // Caller flow: someone-accepted is the cue to actually join
  if (state.kind === 'outgoing-ringing' && payload.canceled_by === 'someone_accepted') {
    notificationSoundService.stopLoop('call-outgoing');
    const { conversationId } = state;
    const joiningState: CallState = {
      kind: 'joining',
      conversationId,
      ringId: state.ringId,
    };
    store.setCallState(joiningState);
    // Fire-and-forget joinChannel. The voiceService internally manages
    // state transitions during/after join; on success it'll be in-call.
    void voiceService.joinChannel(conversationId, 'dm').then(
      () => {
        if (useVoiceStore.getState().callState === joiningState) {
          store.setCallState({ kind: 'in-call' });
        }
      },
      (err) => {
        // Join failed; revert to idle. The caller's UI can surface this.
        console.error('Failed to join DM voice call after accept:', sanitizeErrForLog(err));
        if (useVoiceStore.getState().callState === joiningState) {
          store.setCallState({ kind: 'idle' });
        }
      }
    );
    return;
  }

  // All other cancel reasons (caller-cancel, all-declined, server-error,
  // or callee receiving any cancel) — clean up + go idle
  notificationSoundService.stopLoop('call-outgoing');
  notificationSoundService.stopLoop('call-ringing');
  store.setCallState({ kind: 'idle' });
}

/**
 * handleCallDeclined handles dm_voice_call_declined per spec §7.3 step 6.
 *
 * For DM 1:1 (and any conversation absent from dmStore — defensive
 * fallback): a single decline is the terminal decline → stop ringback +
 * transition to idle (unchanged #1209 behavior).
 *
 * For groups (#1219 R2): the caller STAYS in outgoing-ringing while the
 * per-decliner tally (declinedUserIds) accumulates. This handler never
 * transitions to idle for a group — terminal idle is driven solely by
 * handleCallCanceled('all_declined') when the last callee declines (the
 * server emits dm_voice_call_canceled with canceled_by='all_declined').
 * The group `isGroup` flag is read from the NORMALIZED camelCase
 * conversation in dmStore (matching DMConversationContextMenu.tsx).
 */
export function handleCallDeclined(payload: DMVoiceCallDeclinedPayload): void {
  const queuedForUnresolvedRing = queueEventForUnresolvedRing(
    payload.conversation_id,
    payload.ring_id,
    () => handleCallDeclined(payload)
  );
  const store = useVoiceStore.getState();
  const state = store.callState;
  if (state.kind !== 'outgoing-ringing') {
    return;
  }
  if (state.conversationId !== payload.conversation_id) {
    return;
  }
  if (state.ringId === '' && queuedForUnresolvedRing) {
    return;
  }
  if (state.ringId !== payload.ring_id) {
    return;
  }

  const conv = useDMStore.getState().conversations.find((c) => c.id === state.conversationId);
  const isGroup = conv?.isGroup === true; // absent conversation → treat as 1:1

  if (isGroup) {
    const declinedUserIds = state.declinedUserIds.includes(payload.decliner_user_id)
      ? state.declinedUserIds
      : [...state.declinedUserIds, payload.decliner_user_id];
    // Stay outgoing-ringing; only the tally changes. Audio keeps playing
    // because remaining callees may still accept. Terminal idle arrives via
    // handleCallCanceled('all_declined') if everyone declines.
    store.setCallState({ ...state, declinedUserIds });
    return;
  }

  notificationSoundService.stopLoop('call-outgoing');
  store.setCallState({ kind: 'idle' });
}

/**
 * handleCallTimedOut handles dm_voice_call_timed_out per spec §7.3
 * step 7. Stops both ringback and ringtone audio (whichever was playing
 * depending on caller vs callee perspective) and transitions to idle.
 */
export function handleCallTimedOut(payload: DMVoiceCallTimedOutPayload): void {
  const queuedForUnresolvedRing = queueEventForUnresolvedRing(
    payload.conversation_id,
    payload.ring_id,
    () => handleCallTimedOut(payload),
    true
  );
  const store = useVoiceStore.getState();
  const state = store.callState;
  if (state.kind !== 'outgoing-ringing' && state.kind !== 'incoming-ringing') {
    return;
  }
  if (state.conversationId !== payload.conversation_id) {
    return;
  }
  if (state.kind === 'outgoing-ringing' && state.ringId === '' && queuedForUnresolvedRing) {
    return;
  }
  if (state.ringId !== payload.ring_id) {
    return;
  }
  notificationSoundService.stopLoop('call-outgoing');
  notificationSoundService.stopLoop('call-ringing');
  store.setCallState({ kind: 'idle' });
}
