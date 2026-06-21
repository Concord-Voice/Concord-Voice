import { describe, it, expect, vi } from 'vitest';

import {
  handleSetDeafen,
  type SetDeafenRoomManager,
  type SetDeafenSocket,
} from '../src/lib/setDeafen.js';

function makeFakes() {
  const setParticipantDeafen = vi.fn();
  const emit = vi.fn();
  const to = vi.fn().mockReturnValue({ emit });
  const roomManager: SetDeafenRoomManager = { setParticipantDeafen };
  const socket: SetDeafenSocket = { to };
  return { roomManager, socket, setParticipantDeafen, to, emit };
}

describe('handleSetDeafen (#685)', () => {
  it('records the flag and broadcasts participant-deafen-changed on success', () => {
    const { roomManager, socket, setParticipantDeafen, to, emit } = makeFakes();

    const result = handleSetDeafen(roomManager, socket, 'room-1', 'user-1', true);

    expect(result).toEqual({ success: true });
    expect(setParticipantDeafen).toHaveBeenCalledWith('room-1', 'user-1', true);
    expect(to).toHaveBeenCalledWith('room-1');
    expect(emit).toHaveBeenCalledWith('participant-deafen-changed', {
      userId: 'user-1',
      isDeafened: true,
    });
  });

  it('passes the false state through on undeafen', () => {
    const { roomManager, socket, setParticipantDeafen, emit } = makeFakes();

    const result = handleSetDeafen(roomManager, socket, 'room-1', 'user-1', false);

    expect(result).toEqual({ success: true });
    expect(setParticipantDeafen).toHaveBeenCalledWith('room-1', 'user-1', false);
    expect(emit).toHaveBeenCalledWith('participant-deafen-changed', {
      userId: 'user-1',
      isDeafened: false,
    });
  });

  it('rejects when the socket is not in a room, without mutating state', () => {
    const { roomManager, socket, setParticipantDeafen, to } = makeFakes();

    const result = handleSetDeafen(roomManager, socket, undefined, 'user-1', true);

    expect(result).toEqual({ error: 'Not in a room' });
    expect(setParticipantDeafen).not.toHaveBeenCalled();
    expect(to).not.toHaveBeenCalled();
  });

  it('rejects a non-boolean isDeafened payload, without mutating state', () => {
    const { roomManager, socket, setParticipantDeafen, to } = makeFakes();

    const result = handleSetDeafen(roomManager, socket, 'room-1', 'user-1', 'yes');

    expect(result).toEqual({ error: 'invalid_payload' });
    expect(setParticipantDeafen).not.toHaveBeenCalled();
    expect(to).not.toHaveBeenCalled();
  });
});
