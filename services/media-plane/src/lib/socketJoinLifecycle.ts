export type SocketBoundJoinOutcome<Access, Value> =
  | { access: Access; status: 'denied' }
  | { access: Access; status: 'revoked' }
  | { access: Access; status: 'canceled' }
  | { access: Access; status: 'joined'; value: Value };

interface SocketBoundJoinOptions<Access, Value> {
  authorize: () => Promise<Access>;
  isAllowed: (access: Access, authorizedAccess?: Access) => boolean;
  isConnected: () => boolean;
  join: (access: Access) => Promise<Value>;
  reauthorize?: (access: Access) => Promise<Access>;
  finalize?: (access: Access, value: Value) => Promise<void>;
  commit?: (access: Access, value: Value) => Value;
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
  removeDMParticipant: () => void | Promise<unknown>;
  roomKind: 'channel' | 'dm';
}

export type EmptyDMJoinCleanupResult = 'active' | 'deferred' | 'noop' | 'released' | 'terminal';

/** Keeps cleanup bound to the A1 call generation, even when A2 reports rotation. */
export class DMJoinCallIdTracker {
  private authorizedCallId?: string;

  constructor(private readonly requestedCallId?: string) {}

  observe(callId?: string): void {
    if (callId && !this.authorizedCallId) this.authorizedCallId = callId;
  }

  current(): string | undefined {
    return this.authorizedCallId ?? this.requestedCallId;
  }
}

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

  await removeDMParticipant();
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
  private readonly deferredCleanupCallIds = new Map<string, string>();
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
      else {
        this.pendingCounts.delete(roomId);
        this.deferredCleanupCallIds.delete(roomId);
      }
    };
  }

  callIdForCleanup(roomId: string, callId?: string): string | undefined {
    return callId ?? this.deferredCleanupCallIds.get(roomId);
  }

  deferCleanupCallId(roomId: string, callId?: string): void {
    if (callId && !this.deferredCleanupCallIds.has(roomId)) {
      this.deferredCleanupCallIds.set(roomId, callId);
    }
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

/** Re-check a provisional DM join against the exact call ID returned by A1. */
export async function reauthorizeDMAdmission<Access extends { callId?: string }>(
  roomKind: 'channel' | 'dm',
  access: Access,
  requestedCallId: string | undefined,
  authorize: (exactCallId: string | undefined) => Promise<Access>
): Promise<Access> {
  if (roomKind !== 'dm') return access;
  return authorize(access.callId ?? requestedCallId);
}

export async function runSocketBoundJoin<Access, Value>({
  authorize,
  isAllowed,
  isConnected,
  join,
  reauthorize,
  finalize,
  commit,
  rollback,
}: SocketBoundJoinOptions<Access, Value>): Promise<SocketBoundJoinOutcome<Access, Value>> {
  const authorizedAccess = await authorize();
  if (!isAllowed(authorizedAccess, authorizedAccess)) {
    return { access: authorizedAccess, status: 'denied' };
  }

  if (!isConnected()) {
    await rollback(authorizedAccess);
    return { access: authorizedAccess, status: 'canceled' };
  }

  try {
    const value = await join(authorizedAccess);
    const access = reauthorize ? await reauthorize(authorizedAccess) : authorizedAccess;
    if (!isAllowed(access, authorizedAccess)) {
      await rollback(authorizedAccess);
      return { access, status: 'revoked' };
    }
    await finalize?.(access, value);
    if (!isConnected()) {
      await rollback(authorizedAccess);
      return { access, status: 'canceled' };
    }

    return { access, status: 'joined', value: commit?.(access, value) ?? value };
  } catch (error) {
    await rollback(authorizedAccess);
    throw error;
  }
}
