export type SocketBoundJoinOutcome<Access, Value> =
  | { access: Access; status: 'denied' }
  | { access: Access; status: 'canceled' }
  | { access: Access; status: 'joined'; value: Value };

interface SocketBoundJoinOptions<Access, Value> {
  authorize: () => Promise<Access>;
  isAllowed: (access: Access) => boolean;
  isConnected: () => boolean;
  join: (access: Access) => Promise<Value>;
  rollback: (access: Access) => Promise<void>;
}

type EmptyDMJoinRoom = {
  callId?: string;
  participantCount: number;
};

interface EmptyDMJoinCleanupOptions {
  authorizedCallId?: string;
  closeEmptyRoom: (expectedCallId: string) => Promise<'terminal' | 'discarded' | undefined>;
  queuedJoinCount: number;
  releaseAuthorization: (callId: string) => Promise<void>;
  room?: EmptyDMJoinRoom;
}

interface RollbackSocketRoomJoinOptions {
  cleanupDMJoin: () => Promise<void>;
  leaveChannelIfOwned: () => Promise<unknown>;
  removeDMParticipant: () => unknown;
  roomKind: 'channel' | 'dm';
}

export type EmptyDMJoinCleanupResult = 'active' | 'deferred' | 'noop' | 'released' | 'terminal';

/**
 * Resolves one empty DM admission while its per-room fence is held. A queued
 * successor owns the lifecycle decision; the final holder closes locally
 * before any potentially slow control-plane release.
 */
export async function cleanupEmptyDMJoin({
  authorizedCallId,
  closeEmptyRoom,
  queuedJoinCount,
  releaseAuthorization,
  room,
}: EmptyDMJoinCleanupOptions): Promise<EmptyDMJoinCleanupResult> {
  if (queuedJoinCount > 1) return 'deferred';
  if (room && room.participantCount > 0) return 'active';

  const closeResult = room?.callId ? await closeEmptyRoom(room.callId) : undefined;
  if (closeResult === 'terminal') return 'terminal';

  const releaseCallId = authorizedCallId ?? room?.callId;
  if (!releaseCallId) return 'noop';
  await releaseAuthorization(releaseCallId);
  return 'released';
}

/** Keep DM queue ownership separate from ordinary channel empty-room teardown. */
export async function rollbackSocketRoomJoin({
  cleanupDMJoin,
  leaveChannelIfOwned,
  removeDMParticipant,
  roomKind,
}: RollbackSocketRoomJoinOptions): Promise<void> {
  if (roomKind === 'channel') {
    await leaveChannelIfOwned();
    return;
  }

  removeDMParticipant();
  await cleanupDMJoin();
}

/** Synchronously reserves one in-flight room until socket.data records admission. */
export class SocketRoomClaim {
  private pendingRoomId?: string;

  claim(joinedRoomId: string | undefined, requestedRoomId: string): (() => void) | undefined {
    if (joinedRoomId !== undefined || this.pendingRoomId !== undefined) return undefined;

    this.pendingRoomId = requestedRoomId;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      if (this.pendingRoomId === requestedRoomId) this.pendingRoomId = undefined;
    };
  }
}

export class KeyedJoinFence {
  private readonly tails = new Map<string, Promise<void>>();
  private readonly pendingCounts = new Map<string, number>();

  async acquire(roomId: string): Promise<() => void> {
    const previous = this.tails.get(roomId) ?? Promise.resolve();
    let releaseCurrent!: () => void;
    const current = new Promise<void>((resolve) => {
      releaseCurrent = resolve;
    });
    this.tails.set(roomId, current);
    this.pendingCounts.set(roomId, this.pending(roomId) + 1);
    await previous;

    let released = false;
    return () => {
      if (released) return;
      released = true;
      releaseCurrent();
      if (this.tails.get(roomId) === current) this.tails.delete(roomId);
      const remaining = this.pending(roomId) - 1;
      if (remaining > 0) this.pendingCounts.set(roomId, remaining);
      else this.pendingCounts.delete(roomId);
    };
  }

  pending(roomId: string): number {
    return this.pendingCounts.get(roomId) ?? 0;
  }
}

/** Always releases the per-room fence, including authorization aborts. */
export async function withKeyedJoinFence<Value>(
  fence: KeyedJoinFence,
  roomId: string,
  operation: () => Promise<Value>
): Promise<Value> {
  const release = await fence.acquire(roomId);
  try {
    return await operation();
  } finally {
    release();
  }
}

export async function runSocketBoundJoin<Access, Value>({
  authorize,
  isAllowed,
  isConnected,
  join,
  rollback,
}: SocketBoundJoinOptions<Access, Value>): Promise<SocketBoundJoinOutcome<Access, Value>> {
  const access = await authorize();
  if (!isAllowed(access)) return { access, status: 'denied' };

  if (!isConnected()) {
    await rollback(access);
    return { access, status: 'canceled' };
  }

  try {
    const value = await join(access);
    if (isConnected()) return { access, status: 'joined', value };

    await rollback(access);
    return { access, status: 'canceled' };
  } catch (error) {
    await rollback(access);
    throw error;
  }
}
