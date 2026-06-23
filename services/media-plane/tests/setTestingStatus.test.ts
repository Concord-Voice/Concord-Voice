import { describe, it, expect, vi } from 'vitest';

import {
  handleSetTestingStatus,
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
});
