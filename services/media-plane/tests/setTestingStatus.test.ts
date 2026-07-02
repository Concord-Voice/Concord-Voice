import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import {
  handleSetTestingStatus,
  TESTING_STATUS_WINDOW_MS,
  TESTING_STATUS_MAX_CHANGES,
  type SetTestingStatusRoomManager,
  type SetTestingStatusSocket,
} from '../src/lib/setTestingStatus.js';

function makeFakes() {
  const setParticipantTestingStatus = vi.fn();
  const getParticipant = vi.fn().mockReturnValue({
    socketId: 'socket-1',
    isTesting: false,
  });
  const emit = vi.fn();
  const to = vi.fn().mockReturnValue({ emit });
  const roomManager: SetTestingStatusRoomManager = { setParticipantTestingStatus, getParticipant };
  const socket: SetTestingStatusSocket = { id: 'socket-1', to };
  return { roomManager, socket, setParticipantTestingStatus, getParticipant, to, emit };
}

describe('handleSetTestingStatus (#1163)', () => {
  it('records the authenticated user status and broadcasts participant-testing-changed', () => {
    const { roomManager, socket, setParticipantTestingStatus, to, emit } = makeFakes();

    const result = handleSetTestingStatus(roomManager, socket, 'room-1', 'user-1', {
      isTesting: true,
    });

    expect(result).toEqual({ success: true });
    expect(setParticipantTestingStatus).toHaveBeenCalledWith('room-1', 'user-1', true);
    expect(to).toHaveBeenCalledWith('room-1');
    expect(emit).toHaveBeenCalledWith('participant-testing-changed', {
      userId: 'user-1',
      isTesting: true,
    });
  });

  it('passes the false state through', () => {
    const { roomManager, socket, setParticipantTestingStatus, getParticipant, emit } = makeFakes();
    getParticipant.mockReturnValue({ socketId: 'socket-1', isTesting: true });

    const result = handleSetTestingStatus(roomManager, socket, 'room-1', 'user-1', {
      isTesting: false,
    });

    expect(result).toEqual({ success: true });
    expect(setParticipantTestingStatus).toHaveBeenCalledWith('room-1', 'user-1', false);
    expect(emit).toHaveBeenCalledWith('participant-testing-changed', {
      userId: 'user-1',
      isTesting: false,
    });
  });

  it('rejects when the socket is not in a room, without mutating state', () => {
    const { roomManager, socket, setParticipantTestingStatus, to } = makeFakes();

    const result = handleSetTestingStatus(roomManager, socket, undefined, 'user-1', {
      isTesting: true,
    });

    expect(result).toEqual({ error: 'Not in a room' });
    expect(setParticipantTestingStatus).not.toHaveBeenCalled();
    expect(to).not.toHaveBeenCalled();
  });

  it('rejects a non-boolean isTesting payload, without mutating state', () => {
    const { roomManager, socket, setParticipantTestingStatus, to } = makeFakes();

    const result = handleSetTestingStatus(roomManager, socket, 'room-1', 'user-1', {
      isTesting: 'yes',
    });

    expect(result).toEqual({ error: 'invalid_payload' });
    expect(setParticipantTestingStatus).not.toHaveBeenCalled();
    expect(to).not.toHaveBeenCalled();
  });

  it.each([null, undefined, 'yes', 1])(
    'rejects malformed payload %s without mutating state',
    (payload) => {
      const { roomManager, socket, setParticipantTestingStatus, to } = makeFakes();

      const result = handleSetTestingStatus(roomManager, socket, 'room-1', 'user-1', payload);

      expect(result).toEqual({ error: 'invalid_payload' });
      expect(setParticipantTestingStatus).not.toHaveBeenCalled();
      expect(to).not.toHaveBeenCalled();
    }
  );

  it('does not rebroadcast unchanged status', () => {
    const { roomManager, socket, setParticipantTestingStatus, getParticipant, to } = makeFakes();
    getParticipant.mockReturnValue({ socketId: 'socket-1', isTesting: true });

    const result = handleSetTestingStatus(roomManager, socket, 'room-1', 'user-1', {
      isTesting: true,
    });

    expect(result).toEqual({ success: true });
    expect(setParticipantTestingStatus).not.toHaveBeenCalled();
    expect(to).not.toHaveBeenCalled();
  });

  it('rejects stale sockets that no longer own the participant', () => {
    const { roomManager, socket, setParticipantTestingStatus, getParticipant, to } = makeFakes();
    getParticipant.mockReturnValue({ socketId: 'socket-2', isTesting: false });

    const result = handleSetTestingStatus(roomManager, socket, 'room-1', 'user-1', {
      isTesting: true,
    });

    expect(result).toEqual({ error: 'Not in a room' });
    expect(setParticipantTestingStatus).not.toHaveBeenCalled();
    expect(to).not.toHaveBeenCalled();
  });

  describe('sliding-window rate limit (#2030 — #1163 §7.5 interim mitigation)', () => {
    beforeEach(() => {
      vi.useFakeTimers();
      vi.setSystemTime(1_000_000);
    });
    afterEach(() => {
      vi.useRealTimers();
    });

    /**
     * Wire the fake like the real RoomManager: an accepted change flips the
     * participant's isTesting, so alternating toggles are state-changing.
     */
    function makeStatefulFakes() {
      const fakes = makeFakes();
      const participant = { socketId: 'socket-1', isTesting: false };
      fakes.getParticipant.mockReturnValue(participant);
      fakes.setParticipantTestingStatus.mockImplementation(
        (_room: string, _user: string, v: boolean) => {
          participant.isTesting = v;
        }
      );
      return { ...fakes, participant };
    }

    function toggle(fakes: ReturnType<typeof makeStatefulFakes>) {
      return handleSetTestingStatus(fakes.roomManager, fakes.socket, 'room-1', 'user-1', {
        isTesting: !fakes.participant.isTesting,
      });
    }

    it('never rejects a legitimate fast test cycle (happy path)', () => {
      const fakes = makeStatefulFakes();

      // Two rapid start/stop cycles = 4 state changes, well under the budget.
      for (let i = 0; i < 4; i++) {
        expect(toggle(fakes)).toEqual({ success: true });
      }
      expect(fakes.emit).toHaveBeenCalledTimes(4);
    });

    it('rejects a toggle flood beyond the in-window budget with zero broadcasts', () => {
      const fakes = makeStatefulFakes();

      for (let i = 0; i < TESTING_STATUS_MAX_CHANGES; i++) {
        expect(toggle(fakes)).toEqual({ success: true });
      }
      const flood = toggle(fakes);

      expect(flood).toEqual({ error: 'rate_limited' });
      expect(fakes.emit).toHaveBeenCalledTimes(TESTING_STATUS_MAX_CHANGES);
      expect(fakes.setParticipantTestingStatus).toHaveBeenCalledTimes(TESTING_STATUS_MAX_CHANGES);
    });

    it('self-heals once the window slides past (no permanent lockout)', () => {
      const fakes = makeStatefulFakes();

      for (let i = 0; i < TESTING_STATUS_MAX_CHANGES; i++) toggle(fakes);
      expect(toggle(fakes)).toEqual({ error: 'rate_limited' });

      vi.setSystemTime(1_000_000 + TESTING_STATUS_WINDOW_MS + 1);

      expect(toggle(fakes)).toEqual({ success: true });
    });

    it('does not count idempotent same-state calls against the budget', () => {
      const fakes = makeStatefulFakes();

      // Same-state calls no-op (no broadcast) and must not consume budget.
      for (let i = 0; i < TESTING_STATUS_MAX_CHANGES * 2; i++) {
        expect(
          handleSetTestingStatus(fakes.roomManager, fakes.socket, 'room-1', 'user-1', {
            isTesting: false,
          })
        ).toEqual({ success: true });
      }
      // A real state change is still allowed afterwards.
      expect(toggle(fakes)).toEqual({ success: true });
    });
  });
});
