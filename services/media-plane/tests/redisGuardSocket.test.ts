import { createServer, type Socket } from 'node:net';
import { afterEach, expect, it, vi } from 'vitest';
import './mocks/logger.js';

vi.mock('@/config/index.js', () => ({
  config: { redisUrl: '', roomPlacementEnabled: true },
}));

import { config } from '@/config/index.js';
import { RedisService } from '../src/lib/redis.js';

const sockets = new Set<Socket>();
let blackhole = true;

afterEach(() => {
  for (const socket of sockets) socket.destroy();
  sockets.clear();
  blackhole = true;
});

it('closes an open black-holed MGET and recovers with no pending owner reads', async () => {
  const server = createServer((socket) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    let pending = Buffer.alloc(0);
    socket.on('data', (chunk) => {
      pending = Buffer.concat([pending, chunk]);
      while (pending.length) {
        const end = pending.indexOf('\r\n');
        if (end < 0) return;
        const count = Number(pending.subarray(1, end).toString());
        if (pending[0] !== 42 || !Number.isInteger(count)) throw new Error('bad RESP command');
        let offset = end + 2;
        const args: string[] = [];
        for (let i = 0; i < count; i++) {
          const next = pending.indexOf('\r\n', offset);
          if (next < 0) return;
          const length = Number(pending.subarray(offset + 1, next).toString());
          if (pending[offset] !== 36 || !Number.isInteger(length) || length < 0)
            throw new Error('bad RESP argument');
          offset = next + 2;
          if (pending.length < offset + length + 2) return;
          args.push(pending.subarray(offset, offset + length).toString());
          offset += length + 2;
        }
        pending = pending.subarray(offset);
        switch (args[0]?.toUpperCase()) {
          case 'HELLO':
            socket.write(
              '%7\r\n+server\r\n+redis\r\n+version\r\n+7.2.0\r\n+proto\r\n:3\r\n+id\r\n:1\r\n+mode\r\n+standalone\r\n+role\r\n+master\r\n+modules\r\n*0\r\n'
            );
            break;
          case 'CLIENT':
          case 'SET':
            socket.write('+OK\r\n');
            break;
          case 'INCR':
            socket.write(':17\r\n');
            break;
          case 'GET':
            socket.write('_\r\n');
            break;
          case 'MGET':
            if (!blackhole) socket.write('*3\r\n_\r\n_\r\n_\r\n');
            break;
          default:
            throw new Error(`unexpected RESP command ${args[0]}`);
        }
      }
    });
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('RESP fixture address missing');
  (config as { redisUrl: string }).redisUrl = `redis://127.0.0.1:${address.port}`;
  const service = new RedisService();
  try {
    await service.connect();
    await service.initializeMediaInstance('blue');
    await expect(service.guardRoomCreation('room-1')).rejects.toMatchObject({
      code: 'room_owner_unavailable',
    });
    expect(service.pendingRoomOwnerReadCount()).toBe(0);
    blackhole = false;
    await vi.waitFor(() => expect(service.healthState().redisReachable).toBe(true));
    await expect(service.guardRoomCreation('room-2')).resolves.toBeUndefined();
    expect(service.pendingRoomOwnerReadCount()).toBe(0);
  } finally {
    await service.close();
    for (const socket of sockets) socket.destroy();
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}, 10_000);
