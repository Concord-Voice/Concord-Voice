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
 * voice call state machine. Five terminal/transient states per spec §5.2.
 *
 *   idle              → no call activity
 *   outgoing-ringing  → caller is awaiting accept/decline/timeout
 *   incoming-ringing  → callee sees IncomingCallBanner, ringtone looping
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
  | { kind: 'in-call' }
  | { kind: 'ending' };

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
 * Throws if the conversation isn't loaded in dmStore (shouldn't happen
 * at the call site — DMConversationContextMenu only renders the Voice
 * Call item from a conversation row).
 */
export async function initiateDMCall(conversationId: string): Promise<void> {
  const dmState = useDMStore.getState();
  const conversation = dmState.conversations.find((c) => c.id === conversationId);
  if (!conversation) {
    throw new Error(`Conversation ${conversationId} not found in dmStore`);
  }

  // Optimistic state transition — UI surfaces OutgoingCallModal at this point
  useVoiceStore.getState().setCallState({
    kind: 'outgoing-ringing',
    conversationId,
    ringId: '',
    calleeUserIds: [],
    startedAt: Date.now(),
    declinedUserIds: [],
  });

  // Start ringback audio
  notificationSoundService.playLoop('call-outgoing');

  let response: Response;
  try {
    response = await apiFetch(`/api/v1/dm/conversations/${conversationId}/voice/ring`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    });
  } catch (err) {
    notificationSoundService.stopLoop('call-outgoing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    throw err;
  }

  if (!response.ok) {
    notificationSoundService.stopLoop('call-outgoing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    const errBody = await response.text().catch(() => '');
    throw new Error(`Failed to ring (HTTP ${response.status}): ${errBody}`);
  }

  const data = (await response.json()) as {
    ring_id: string;
    ring_started_at: string;
    ringing_user_ids: string[];
  };

  // Update state with server-issued ring_id + callee set
  useVoiceStore.getState().setCallState({
    kind: 'outgoing-ringing',
    conversationId,
    ringId: data.ring_id,
    calleeUserIds: data.ringing_user_ids,
    startedAt: Date.parse(data.ring_started_at),
    declinedUserIds: [],
  });
}

/**
 * cancelOutgoingCall lets the caller cancel their own ring before any
 * callee accepts. POSTs /voice/cancel; stops ringback; transitions to idle.
 *
 * Idempotent: if callState isn't outgoing-ringing (e.g., already canceled
 * via a server event), the state mutation + audio cleanup still runs as
 * a defensive no-op. The /cancel POST is skipped if there's no ring to
 * cancel (callState !== outgoing-ringing).
 */
export async function cancelOutgoingCall(): Promise<void> {
  const state = useVoiceStore.getState().callState;
  if (state.kind !== 'outgoing-ringing') {
    // No active outgoing ring — defensive cleanup only.
    notificationSoundService.stopLoop('call-outgoing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    return;
  }

  const { conversationId } = state;
  try {
    await apiFetch(`/api/v1/dm/conversations/${conversationId}/voice/cancel`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    });
  } catch (err) {
    // POST failure is observable but doesn't block local cleanup. The
    // server may continue ringing callees until the 45s timeout, but the
    // renderer-side state still needs to transition to idle so the UI
    // recovers (silent-failure-hunter #1231 finding — bare `finally`
    // without `catch` made the POST error invisible).
    console.error('DM voice cancel POST failed:', sanitizeErrForLog(err));
  } finally {
    notificationSoundService.stopLoop('call-outgoing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
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
  notificationSoundService.stopLoop('call-ringing');
  try {
    await voiceService.joinChannel(conversationId, 'dm');
    useVoiceStore.getState().setCallState({ kind: 'in-call' });
  } catch (err) {
    // joinChannel failure (mediasoup error, network drop, server 500): reset
    // to idle so the UI doesn't lock the callee in incoming-ringing with no
    // way out (silent-failure-hunter #1231 finding). The error is rethrown
    // so the caller (click handler in IncomingCallBanner) can surface it.
    console.error('DM voice accept failed:', sanitizeErrForLog(err));
    useVoiceStore.getState().setCallState({ kind: 'idle' });
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
    notificationSoundService.stopLoop('call-ringing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    return;
  }
  const { conversationId } = state;
  try {
    await apiFetch(`/api/v1/dm/conversations/${conversationId}/voice/decline`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    });
  } catch (err) {
    // Same posture as cancelOutgoingCall: observable POST error but local
    // state still transitions in `finally` so the UI recovers
    // (silent-failure-hunter #1231 finding).
    console.error('DM voice decline POST failed:', sanitizeErrForLog(err));
  } finally {
    notificationSoundService.stopLoop('call-ringing');
    useVoiceStore.getState().setCallState({ kind: 'idle' });
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
  const store = useVoiceStore.getState();
  const state = store.callState;
  if (state.kind !== 'outgoing-ringing' && state.kind !== 'incoming-ringing') {
    return;
  }
  if (state.conversationId !== payload.conversation_id) {
    return;
  }

  // Caller flow: someone-accepted is the cue to actually join
  if (state.kind === 'outgoing-ringing' && payload.canceled_by === 'someone_accepted') {
    notificationSoundService.stopLoop('call-outgoing');
    const { conversationId } = state;
    // Fire-and-forget joinChannel. The voiceService internally manages
    // state transitions during/after join; on success it'll be in-call.
    void voiceService.joinChannel(conversationId, 'dm').then(
      () => store.setCallState({ kind: 'in-call' }),
      (err) => {
        // Join failed; revert to idle. The caller's UI can surface this.
        console.error('Failed to join DM voice call after accept:', sanitizeErrForLog(err));
        store.setCallState({ kind: 'idle' });
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
  const store = useVoiceStore.getState();
  const state = store.callState;
  if (state.kind !== 'outgoing-ringing') {
    return;
  }
  if (state.conversationId !== payload.conversation_id) {
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
  const store = useVoiceStore.getState();
  const state = store.callState;
  if (state.kind !== 'outgoing-ringing' && state.kind !== 'incoming-ringing') {
    return;
  }
  if (state.conversationId !== payload.conversation_id) {
    return;
  }
  notificationSoundService.stopLoop('call-outgoing');
  notificationSoundService.stopLoop('call-ringing');
  store.setCallState({ kind: 'idle' });
}
