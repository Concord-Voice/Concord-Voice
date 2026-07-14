import { describe, expect, it, vi } from 'vitest';
import {
  cleanupEmptyDMJoin,
  KeyedJoinFence,
  rollbackSocketRoomJoin,
  SocketRoomClaim,
  runSocketBoundJoin,
  withKeyedJoinFence,
} from '../src/lib/socketJoinLifecycle.js';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('runSocketBoundJoin', () => {
  it('does not join when the socket disconnects while authorization is pending', async () => {
    const authorization = deferred<{ allowed: boolean }>();
    let connected = true;
    const join = vi.fn(async () => ({ room: 'dm-1' }));
    const rollback = vi.fn(async () => undefined);

    const operation = runSocketBoundJoin({
      authorize: () => authorization.promise,
      isAllowed: (access) => access.allowed,
      isConnected: () => connected,
      join,
      rollback,
    });
    connected = false;
    authorization.resolve({ allowed: true });

    await expect(operation).resolves.toEqual({
      access: { allowed: true },
      status: 'canceled',
    });
    expect(join).not.toHaveBeenCalled();
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rolls back the exact participant when disconnect lands during room admission', async () => {
    const joined = deferred<{ room: string }>();
    let connected = true;
    const join = vi.fn(() => joined.promise);
    const rollback = vi.fn(async () => undefined);
    const operation = runSocketBoundJoin({
      authorize: async () => ({ allowed: true }),
      isAllowed: (access) => access.allowed,
      isConnected: () => connected,
      join,
      rollback,
    });
    await vi.waitFor(() => expect(join).toHaveBeenCalledOnce());
    connected = false;
    joined.resolve({ room: 'dm-1' });

    await expect(operation).resolves.toEqual({
      access: { allowed: true },
      status: 'canceled',
    });
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rolls back when admission throws after it may have mutated room state', async () => {
    const rollback = vi.fn(async () => undefined);
    const failure = new Error('server mute failed');

    await expect(
      runSocketBoundJoin({
        authorize: async () => ({ allowed: true }),
        isAllowed: (access) => access.allowed,
        isConnected: () => true,
        join: async () => {
          throw failure;
        },
        rollback,
      })
    ).rejects.toBe(failure);
    expect(rollback).toHaveBeenCalledOnce();
  });

  it('rejects when a canceled join cannot be rolled back', async () => {
    const failure = new Error('rollback failed');
    await expect(
      runSocketBoundJoin({
        authorize: async () => ({ allowed: true }),
        isAllowed: (access) => access.allowed,
        isConnected: () => false,
        join: vi.fn(),
        rollback: async () => {
          throw failure;
        },
      })
    ).rejects.toBe(failure);
  });
});

it('serializes admission and rollback for the same room', async () => {
  const fence = new KeyedJoinFence();
  const releaseFirst = await fence.acquire('dm-1');
  let secondAcquired = false;
  const second = fence.acquire('dm-1').then((release) => {
    secondAcquired = true;
    return release;
  });
  await Promise.resolve();
  expect(secondAcquired).toBe(false);
  expect(fence.pending('dm-1')).toBe(2);
  releaseFirst();
  const releaseSecond = await second;
  expect(secondAcquired).toBe(true);
  expect(fence.pending('dm-1')).toBe(1);
  releaseSecond();
  expect(fence.pending('dm-1')).toBe(0);
});

it('releases the room fence when a stalled authorization aborts', async () => {
  vi.useFakeTimers();
  try {
    const fence = new KeyedJoinFence();
    const first = withKeyedJoinFence(fence, 'dm-1', async () => {
      const controller = new AbortController();
      setTimeout(() => controller.abort(), 5_000);
      await new Promise((_, reject) => {
        controller.signal.addEventListener('abort', () => reject(controller.signal.reason), {
          once: true,
        });
      });
    });
    const firstRejected = first.then(
      () => false,
      () => true
    );
    const second = withKeyedJoinFence(fence, 'dm-1', async () => 'joined');

    await vi.advanceTimersByTimeAsync(5_000);
    await expect(firstRejected).resolves.toBe(true);
    await expect(second).resolves.toBe('joined');
    expect(fence.pending('dm-1')).toBe(0);
  } finally {
    vi.useRealTimers();
  }
});

it('allows one in-flight or admitted room per socket so disconnect leaves no ghost', async () => {
  const claim = new SocketRoomClaim();
  const admittedRooms = new Set<string>();
  let joinedRoomId: string | undefined;

  const join = (roomId: string) => {
    const release = claim.claim(joinedRoomId, roomId);
    if (!release) return false;
    admittedRooms.add(roomId);
    joinedRoomId = roomId;
    release();
    return true;
  };

  expect(join('dm-a')).toBe(true);
  expect(join('dm-b')).toBe(false);
  expect([...admittedRooms]).toEqual(['dm-a']);

  admittedRooms.delete(joinedRoomId!);
  joinedRoomId = undefined;
  expect(admittedRooms).toHaveLength(0);
  expect(claim.claim(joinedRoomId, 'dm-b')).toBeTypeOf('function');
});

it('rejects a second room while the first socket admission is still pending', () => {
  const claim = new SocketRoomClaim();
  const releaseFirst = claim.claim(undefined, 'dm-a');

  expect(releaseFirst).toBeTypeOf('function');
  expect(claim.claim(undefined, 'dm-b')).toBeUndefined();

  releaseFirst?.();
  expect(claim.claim(undefined, 'dm-b')).toBeTypeOf('function');
});

describe('cleanupEmptyDMJoin', () => {
  it('preserves the admitted call while a queued successor takes over the empty room', async () => {
    const room = { callId: 'call-a', participantCount: 0 };
    const closeEmptyRoom = vi.fn(async () => 'terminal' as const);
    const releaseAuthorization = vi.fn(async () => undefined);

    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom,
        queuedJoinCount: 2,
        releaseAuthorization,
        room,
      })
    ).resolves.toBe('deferred');
    expect(closeEmptyRoom).not.toHaveBeenCalled();
    expect(releaseAuthorization).not.toHaveBeenCalled();

    room.participantCount = 1;
    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom,
        queuedJoinCount: 1,
        releaseAuthorization,
        room,
      })
    ).resolves.toBe('active');
    expect(closeEmptyRoom).not.toHaveBeenCalled();
    expect(releaseAuthorization).not.toHaveBeenCalled();
  });

  it('terminalizes exactly once when the last queued successor also fails', async () => {
    const closeEmptyRoom = vi.fn(async () => 'terminal' as const);
    const releaseAuthorization = vi.fn(async () => undefined);

    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom,
        queuedJoinCount: 1,
        releaseAuthorization,
        room: { callId: 'call-a', participantCount: 0 },
      })
    ).resolves.toBe('terminal');
    expect(closeEmptyRoom).toHaveBeenCalledExactlyOnceWith('call-a');
    expect(releaseAuthorization).not.toHaveBeenCalled();
  });

  it('silently discards an unadmitted local room before releasing its reservation', async () => {
    const order: string[] = [];

    await expect(
      cleanupEmptyDMJoin({
        authorizedCallId: 'call-a',
        closeEmptyRoom: async () => {
          order.push('close');
          return 'discarded';
        },
        queuedJoinCount: 1,
        releaseAuthorization: async () => {
          order.push('release');
        },
        room: { callId: 'call-a', participantCount: 0 },
      })
    ).resolves.toBe('released');
    expect(order).toEqual(['close', 'release']);
  });
});

it('uses exact-owner normal teardown when a channel socket disconnects during admission', async () => {
  const leaveChannelIfOwned = vi.fn(async () => true);
  const removeDMParticipant = vi.fn();
  const cleanupDMJoin = vi.fn(async () => undefined);

  await expect(
    runSocketBoundJoin({
      authorize: async () => ({ allowed: true }),
      isAllowed: (access) => access.allowed,
      isConnected: () => false,
      join: vi.fn(),
      rollback: () =>
        rollbackSocketRoomJoin({
          cleanupDMJoin,
          leaveChannelIfOwned,
          removeDMParticipant,
          roomKind: 'channel',
        }),
    })
  ).resolves.toEqual({ access: { allowed: true }, status: 'canceled' });

  expect(leaveChannelIfOwned).toHaveBeenCalledOnce();
  expect(removeDMParticipant).not.toHaveBeenCalled();
  expect(cleanupDMJoin).not.toHaveBeenCalled();
});
