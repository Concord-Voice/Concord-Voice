// Tests for the DM voice call state machine (callStateMachine.ts).
//
// Covers all 8 exported functions:
//   - initiateDMCall, cancelOutgoingCall (caller flow)
//   - acceptIncomingCall, declineIncomingCall (callee flow)
//   - handleCallInvited, handleCallCanceled, handleCallDeclined, handleCallTimedOut
//     (4 WS event handlers)
//
// Mocks at the module boundary (apiClient, notificationSoundService, voiceService)
// per [internal]rules/tests.md "MSW for API mocking" — but for this partial-module
// service we mock the singleton imports directly since they're called as
// dependencies, not via fetch.
//
// Filed per PR #1231 SonarCloud Quality Gate coverage gap (callStateMachine.ts
// was at 0% coverage before this test file landed).

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';

// ── Mocks ─────────────────────────────────────────────────────────────────

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/notificationSoundService', () => ({
  notificationSoundService: {
    playLoop: vi.fn(),
    stopLoop: vi.fn(),
    stopAllLoops: vi.fn(),
    play: vi.fn(),
    isLooping: vi.fn().mockReturnValue(false),
  },
}));

vi.mock('@/renderer/services/voiceService', () => ({
  voiceService: {
    joinChannel: vi.fn(),
  },
}));

import { apiFetch } from '@/renderer/services/apiClient';
import { notificationSoundService } from '@/renderer/services/notificationSoundService';
import { voiceService } from '@/renderer/services/voiceService';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import type { DMConversation } from '@/renderer/stores/dmStore';
import type { CallState } from '@/renderer/services/voiceService/callStateMachine';

import {
  initiateDMCall,
  cancelOutgoingCall,
  acceptIncomingCall,
  declineIncomingCall,
  handleCallInvited,
  handleCallCanceled,
  handleCallDeclined,
  handleCallTimedOut,
} from '@/renderer/services/voiceService/callStateMachine';

const mockApiFetch = vi.mocked(apiFetch);
const mockPlayLoop = vi.mocked(notificationSoundService.playLoop);
const mockStopLoop = vi.mocked(notificationSoundService.stopLoop);
const mockJoinChannel = vi.mocked(voiceService.joinChannel);

// ── Test helpers ──────────────────────────────────────────────────────────

const CONVERSATION_ID = '11111111-1111-1111-1111-111111111111';
const RING_ID = '22222222-2222-2222-2222-222222222222';
const RING_STARTED_AT = '2026-05-28T13:00:00.000Z';
const RING_TIMEOUT_SECONDS = 30;
const OTHER_CONVERSATION_ID = '99999999-9999-9999-9999-999999999999';

function seedConversation(): void {
  // Add a minimal conversation row so initiateDMCall's lookup succeeds.
  useDMStore.setState({
    conversations: [
      {
        id: CONVERSATION_ID,
        is_group: false,
        name: null,
        avatar_url: null,
        last_message_preview: null,
        last_activity_at: '2026-05-28T13:00:00.000Z',
        unread_count: 0,
        participants: [
          {
            user_id: 'caller-id',
            username: 'caller',
            display_name: null,
            avatar_url: null,
            role: 'admin',
          },
          {
            user_id: 'callee-id',
            username: 'callee',
            display_name: null,
            avatar_url: null,
            role: 'member',
          },
        ],
      } as never,
    ],
  });
}

function ringResponse(): Response {
  return {
    ok: true,
    status: 200,
    json: async () => ({
      ring_id: RING_ID,
      ring_started_at: RING_STARTED_AT,
      ringing_user_ids: ['callee-id'],
    }),
    text: async () => '',
  } as Response;
}

function errorResponse(status: number, body: string): Response {
  return {
    ok: false,
    status,
    json: async () => ({}),
    text: async () => body,
  } as Response;
}

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset();
  mockPlayLoop.mockReset();
  mockStopLoop.mockReset();
  mockJoinChannel.mockReset();
});

// ── initiateDMCall ────────────────────────────────────────────────────────

describe('initiateDMCall', () => {
  it('throws when the conversation is not in dmStore', async () => {
    await expect(initiateDMCall('not-a-real-id')).rejects.toThrow(/not found in dmStore/);
  });

  it('optimistically sets outgoing-ringing state, starts ringback, and updates with server ring_id', async () => {
    seedConversation();
    mockApiFetch.mockResolvedValueOnce(ringResponse());

    await initiateDMCall(CONVERSATION_ID);

    expect(mockPlayLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockApiFetch).toHaveBeenCalledWith(
      `/api/v1/dm/conversations/${CONVERSATION_ID}/voice/ring`,
      expect.objectContaining({ method: 'POST' })
    );
    const final = useVoiceStore.getState().callState;
    expect(final.kind).toBe('outgoing-ringing');
    if (final.kind === 'outgoing-ringing') {
      expect(final.conversationId).toBe(CONVERSATION_ID);
      expect(final.ringId).toBe(RING_ID);
      expect(final.calleeUserIds).toEqual(['callee-id']);
      expect(final.startedAt).toBe(Date.parse(RING_STARTED_AT));
    }
  });

  it('rolls back to idle and stops ringback when apiFetch throws (network error)', async () => {
    seedConversation();
    const netErr = new Error('network');
    mockApiFetch.mockRejectedValueOnce(netErr);

    await expect(initiateDMCall(CONVERSATION_ID)).rejects.toBe(netErr);

    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('rolls back to idle and surfaces HTTP error body when response !ok', async () => {
    seedConversation();
    mockApiFetch.mockResolvedValueOnce(errorResponse(409, 'already ringing'));

    await expect(initiateDMCall(CONVERSATION_ID)).rejects.toThrow(/HTTP 409.*already ringing/);

    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('handles response.text() throwing gracefully (catches and uses empty body)', async () => {
    seedConversation();
    const failingTextResponse = {
      ok: false,
      status: 500,
      text: async () => {
        throw new Error('body read failed');
      },
      json: async () => ({}),
    } as unknown as Response;
    mockApiFetch.mockResolvedValueOnce(failingTextResponse);

    await expect(initiateDMCall(CONVERSATION_ID)).rejects.toThrow(/HTTP 500/);
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });
});

// ── cancelOutgoingCall ────────────────────────────────────────────────────

describe('cancelOutgoingCall', () => {
  it('defensive cleanup when not in outgoing-ringing — stopLoop + idle, no POST', async () => {
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    await cancelOutgoingCall();
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockApiFetch).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('POSTs cancel + transitions to idle on success', async () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    mockApiFetch.mockResolvedValueOnce({ ok: true } as Response);

    await cancelOutgoingCall();

    expect(mockApiFetch).toHaveBeenCalledWith(
      `/api/v1/dm/conversations/${CONVERSATION_ID}/voice/cancel`,
      expect.objectContaining({ method: 'POST' })
    );
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('swallows POST errors but still cleans up via finally block', async () => {
    // Updated for the silent-failure fix (#1231): the POST error is now
    // caught + logged inside cancelOutgoingCall/declineIncomingCall so the
    // promise resolves rather than rejecting. The renderer-side cleanup
    // (stopLoop + idle) still runs via the finally block. The error becomes
    // observable via the console.error log instead of an unhandled rejection.
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    mockApiFetch.mockRejectedValueOnce(new Error('network'));

    await expect(cancelOutgoingCall()).resolves.toBeUndefined();

    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
    expect(errorSpy).toHaveBeenCalledWith(expect.stringContaining('cancel POST failed'), 'network');
    errorSpy.mockRestore();
  });
});

// ── acceptIncomingCall ────────────────────────────────────────────────────

describe('acceptIncomingCall', () => {
  it('no-ops when not in incoming-ringing', async () => {
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    await acceptIncomingCall();
    expect(mockJoinChannel).not.toHaveBeenCalled();
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('stops ringtone, joins voice channel, transitions to in-call', async () => {
    useVoiceStore.getState().setCallState({
      kind: 'incoming-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      caller: { userId: 'caller-id', username: 'caller' },
      expiresAt: Date.now() + 30000,
    });
    mockJoinChannel.mockResolvedValueOnce(undefined as never);

    await acceptIncomingCall();

    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(mockJoinChannel).toHaveBeenCalledWith(CONVERSATION_ID, 'dm');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
  });
});

// ── declineIncomingCall ───────────────────────────────────────────────────

describe('declineIncomingCall', () => {
  it('defensive cleanup when not in incoming-ringing — stopLoop + idle, no POST', async () => {
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    await declineIncomingCall();
    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(mockApiFetch).not.toHaveBeenCalled();
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('POSTs decline + transitions to idle on success', async () => {
    useVoiceStore.getState().setCallState({
      kind: 'incoming-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      caller: { userId: 'caller-id', username: 'caller' },
      expiresAt: Date.now() + 30000,
    });
    mockApiFetch.mockResolvedValueOnce({ ok: true } as Response);

    await declineIncomingCall();

    expect(mockApiFetch).toHaveBeenCalledWith(
      `/api/v1/dm/conversations/${CONVERSATION_ID}/voice/decline`,
      expect.objectContaining({ method: 'POST' })
    );
    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('swallows POST errors but still cleans up via finally block', async () => {
    // Same update as cancelOutgoingCall: POST error is now caught + logged
    // rather than re-thrown (silent-failure fix #1231). Local state still
    // transitions to idle so the UI recovers.
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useVoiceStore.getState().setCallState({
      kind: 'incoming-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      caller: { userId: 'caller-id', username: 'caller' },
      expiresAt: Date.now() + 30000,
    });
    mockApiFetch.mockRejectedValueOnce(new Error('network'));

    await expect(declineIncomingCall()).resolves.toBeUndefined();

    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringContaining('decline POST failed'),
      'network'
    );
    errorSpy.mockRestore();
  });
});

// ── handleCallInvited ─────────────────────────────────────────────────────

describe('handleCallInvited', () => {
  const baseInvitedPayload = {
    conversation_id: CONVERSATION_ID,
    is_group: false,
    ring_id: RING_ID,
    caller: {
      user_id: 'caller-id',
      username: 'caller',
      display_name: 'Caller Display',
      avatar_url: 'https://example.com/avatar.png',
    },
    ring_started_at: RING_STARTED_AT,
    ring_timeout_seconds: RING_TIMEOUT_SECONDS,
  } as never;

  it('no-ops when callState is not idle (already in-call)', () => {
    useVoiceStore.getState().setCallState({ kind: 'in-call' });
    handleCallInvited(baseInvitedPayload);
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
    expect(mockPlayLoop).not.toHaveBeenCalled();
  });

  it('sets incoming-ringing state and starts ringtone when idle', () => {
    handleCallInvited(baseInvitedPayload);
    const final = useVoiceStore.getState().callState;
    expect(final.kind).toBe('incoming-ringing');
    if (final.kind === 'incoming-ringing') {
      expect(final.conversationId).toBe(CONVERSATION_ID);
      expect(final.ringId).toBe(RING_ID);
      expect(final.caller).toEqual({
        userId: 'caller-id',
        username: 'caller',
        displayName: 'Caller Display',
        avatarUrl: 'https://example.com/avatar.png',
      });
      expect(final.expiresAt).toBe(Date.parse(RING_STARTED_AT) + RING_TIMEOUT_SECONDS * 1000);
    }
    expect(mockPlayLoop).toHaveBeenCalledWith('call-ringing');
  });

  it('handles missing optional caller fields (display_name, avatar_url undefined)', () => {
    handleCallInvited({
      ...baseInvitedPayload,
      caller: { user_id: 'caller-id', username: 'caller' },
    } as never);
    const final = useVoiceStore.getState().callState;
    if (final.kind === 'incoming-ringing') {
      expect(final.caller.displayName).toBeUndefined();
      expect(final.caller.avatarUrl).toBeUndefined();
    }
  });

  // ── Group context (#1219 R10) ────────────────────────────────────────────

  it('threads is_group=true into the incoming-ringing state', () => {
    handleCallInvited({ ...baseInvitedPayload, is_group: true } as never);
    const final = useVoiceStore.getState().callState as Extract<
      CallState,
      { kind: 'incoming-ringing' }
    >;
    expect(final.kind).toBe('incoming-ringing');
    expect(final.isGroup).toBe(true);
  });

  it('threads is_group=false into the incoming-ringing state (1:1)', () => {
    handleCallInvited({ ...baseInvitedPayload, is_group: false } as never);
    const final = useVoiceStore.getState().callState as Extract<
      CallState,
      { kind: 'incoming-ringing' }
    >;
    expect(final.kind).toBe('incoming-ringing');
    expect(final.isGroup).toBe(false);
  });
});

// ── handleCallCanceled ────────────────────────────────────────────────────

describe('handleCallCanceled', () => {
  // canceled_by must match the schema's enum exactly (caller | all_declined |
  // someone_accepted | server_error). Use a typed default of 'caller' instead
  // of the placeholder 'caller_cancel' that doesn't exist in the schema
  // (Copilot #1231 finding C13).
  const canceledPayload = (
    canceled_by: 'caller' | 'all_declined' | 'someone_accepted' | 'server_error' = 'caller',
    conversation_id = CONVERSATION_ID
  ) => ({ conversation_id, ring_id: RING_ID, canceled_by }) as never;

  it('no-ops when state is idle', () => {
    handleCallCanceled(canceledPayload('caller'));
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('no-ops when state.conversationId does not match payload.conversation_id', () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallCanceled(canceledPayload('caller', OTHER_CONVERSATION_ID));
    expect(useVoiceStore.getState().callState.kind).toBe('outgoing-ringing');
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('caller path: someone_accepted → stops ringback, joinChannel → in-call', async () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    let resolveJoin: () => void = () => {};
    mockJoinChannel.mockReturnValueOnce(
      new Promise<void>((res) => {
        resolveJoin = res;
      }) as never
    );

    handleCallCanceled(canceledPayload('someone_accepted'));
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockJoinChannel).toHaveBeenCalledWith(CONVERSATION_ID, 'dm');

    resolveJoin();
    await new Promise((r) => setTimeout(r, 0));
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'in-call' });
  });

  it('caller path: someone_accepted but joinChannel fails → reverts to idle', async () => {
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    mockJoinChannel.mockRejectedValueOnce(new Error('media plane down'));

    handleCallCanceled(canceledPayload('someone_accepted'));
    await new Promise((r) => setTimeout(r, 0));

    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
    expect(errorSpy).toHaveBeenCalledWith(
      expect.stringContaining('Failed to join'),
      'media plane down'
    );
    errorSpy.mockRestore();
  });

  it('caller path: caller_cancel → stops both audios + idle', () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallCanceled(canceledPayload('caller'));
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('callee path: incoming-ringing receives any cancel → cleanup + idle', () => {
    useVoiceStore.getState().setCallState({
      kind: 'incoming-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      caller: { userId: 'caller-id', username: 'caller' },
      expiresAt: Date.now() + 30000,
    });
    handleCallCanceled(canceledPayload('caller'));
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });
});

// ── handleCallDeclined ────────────────────────────────────────────────────

describe('handleCallDeclined', () => {
  const declinedPayload = (conversation_id = CONVERSATION_ID) =>
    ({ conversation_id, ring_id: RING_ID, decliner_user_id: 'callee-id' }) as never;

  it('no-ops when state is not outgoing-ringing', () => {
    useVoiceStore.getState().setCallState({ kind: 'idle' });
    handleCallDeclined(declinedPayload());
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('no-ops when state.conversationId does not match payload.conversation_id', () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallDeclined(declinedPayload(OTHER_CONVERSATION_ID));
    expect(useVoiceStore.getState().callState.kind).toBe('outgoing-ringing');
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('valid decline → stops ringback, transitions to idle', () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallDeclined(declinedPayload());
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  // ── Group decline tally (#1219 R2) ──────────────────────────────────────

  // Seed a NORMALIZED camelCase group conversation directly (NOT via the
  // snake-case seedConversation() helper, whose `is_group` cast `as never`
  // leaves `conv.isGroup` undefined — which would silently run the 1:1 path).
  function seedGroupConversation(): void {
    useDMStore.setState({
      conversations: [
        {
          id: CONVERSATION_ID,
          isGroup: true,
          isPersonal: false,
          name: 'Squad',
          participants: [
            { userId: 'b', username: 'Bob' },
            { userId: 'c', username: 'Charlie' },
          ],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2026-05-28T13:00:00.000Z',
        } as DMConversation,
      ],
    });
  }

  it('group: first decline keeps outgoing-ringing and records declinedUserIds', () => {
    seedGroupConversation();
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['b', 'c'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallDeclined({
      conversation_id: CONVERSATION_ID,
      ring_id: RING_ID,
      decliner_user_id: 'b',
    } as never);
    const s = useVoiceStore.getState().callState as Extract<
      CallState,
      { kind: 'outgoing-ringing' }
    >;
    expect(s.kind).toBe('outgoing-ringing');
    expect(s.declinedUserIds).toEqual(['b']);
    // Group decline does NOT stop ringback (terminal idle is driven by
    // handleCallCanceled('all_declined'), not this handler).
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('group: duplicate decline does not double-record the same decliner', () => {
    seedGroupConversation();
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['b', 'c'],
      startedAt: 1000,
      declinedUserIds: ['b'],
    });
    handleCallDeclined({
      conversation_id: CONVERSATION_ID,
      ring_id: RING_ID,
      decliner_user_id: 'b',
    } as never);
    const s = useVoiceStore.getState().callState as Extract<
      CallState,
      { kind: 'outgoing-ringing' }
    >;
    expect(s.kind).toBe('outgoing-ringing');
    expect(s.declinedUserIds).toEqual(['b']);
  });

  it('1:1 / absent conversation: decline → idle (fallback when conv not in dmStore)', () => {
    // resetAllStores() cleared dmStore, so the conversation is absent →
    // treated as 1:1 → idle.
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallDeclined(declinedPayload());
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });
});

// ── handleCallTimedOut ────────────────────────────────────────────────────

describe('handleCallTimedOut', () => {
  const timedOutPayload = (conversation_id = CONVERSATION_ID) =>
    ({ conversation_id, ring_id: RING_ID }) as never;

  it('no-ops when state is idle', () => {
    handleCallTimedOut(timedOutPayload());
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
    expect(mockStopLoop).not.toHaveBeenCalled();
  });

  it('no-ops when state.conversationId does not match payload.conversation_id', () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallTimedOut(timedOutPayload(OTHER_CONVERSATION_ID));
    expect(useVoiceStore.getState().callState.kind).toBe('outgoing-ringing');
  });

  it('outgoing-ringing → stops both audios + idle', () => {
    useVoiceStore.getState().setCallState({
      kind: 'outgoing-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      calleeUserIds: ['callee-id'],
      startedAt: 1000,
      declinedUserIds: [],
    });
    handleCallTimedOut(timedOutPayload());
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });

  it('incoming-ringing → stops both audios + idle', () => {
    useVoiceStore.getState().setCallState({
      kind: 'incoming-ringing',
      conversationId: CONVERSATION_ID,
      ringId: RING_ID,
      caller: { userId: 'caller-id', username: 'caller' },
      expiresAt: Date.now() + 30000,
    });
    handleCallTimedOut(timedOutPayload());
    expect(mockStopLoop).toHaveBeenCalledWith('call-outgoing');
    expect(mockStopLoop).toHaveBeenCalledWith('call-ringing');
    expect(useVoiceStore.getState().callState).toEqual({ kind: 'idle' });
  });
});
