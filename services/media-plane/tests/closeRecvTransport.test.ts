import { describe, expect, it, vi } from 'vitest';

import {
  acknowledgeCloseRecvTransport,
  handleCloseRecvTransport,
} from '../src/lib/closeRecvTransport.js';

describe('handleCloseRecvTransport (#2206)', () => {
  it('uses authenticated socket identity and ignores spoofed payload identity', () => {
    const closeRecvTransport = vi.fn().mockReturnValue(true);

    const result = handleCloseRecvTransport(
      { closeRecvTransport },
      'room-authenticated',
      'user-authenticated',
      {
        transportId: ' recv-1 ',
        roomId: 'room-spoofed',
        userId: 'user-spoofed',
      }
    );

    expect(result).toEqual({ success: true });
    expect(closeRecvTransport).toHaveBeenCalledWith(
      'room-authenticated',
      'user-authenticated',
      'recv-1'
    );
  });

  it.each([undefined, null, {}, { transportId: '' }, { transportId: '   ' }, { transportId: 42 }])(
    'rejects an invalid transport ID without touching RoomManager: %j',
    (payload) => {
      const closeRecvTransport = vi.fn();

      const result = handleCloseRecvTransport({ closeRecvTransport }, 'room-1', 'user-1', payload);

      expect(result).toEqual({ error: 'transportId is required' });
      expect(closeRecvTransport).not.toHaveBeenCalled();
    }
  );

  it('acknowledges unknown or non-owned transport IDs without enumeration', () => {
    const closeRecvTransport = vi.fn().mockReturnValue(false);

    const result = handleCloseRecvTransport({ closeRecvTransport }, 'room-1', 'user-1', {
      transportId: 'not-owned',
    });

    expect(result).toEqual({ success: true });
    expect(closeRecvTransport).toHaveBeenCalledWith('room-1', 'user-1', 'not-owned');
  });

  it('rejects a socket that is not in a room', () => {
    const closeRecvTransport = vi.fn();

    const result = handleCloseRecvTransport({ closeRecvTransport }, undefined, 'user-1', {
      transportId: 'recv-1',
    });

    expect(result).toEqual({ error: 'Not in a room' });
    expect(closeRecvTransport).not.toHaveBeenCalled();
  });

  it('ignores a non-function Socket.IO acknowledgement argument', () => {
    expect(() =>
      acknowledgeCloseRecvTransport({ not: 'a callback' }, { success: true })
    ).not.toThrow();
  });
});
