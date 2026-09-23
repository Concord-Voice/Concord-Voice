import { beforeEach, describe, expect, it, vi } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const { release } = vi.hoisted(() => ({ release: vi.fn() }));
vi.mock('../src/lib/voiceEnforcementSession.js', () => ({
  releaseVoiceEnforcementSession: release,
}));

import {
  releaseVoiceEnforcementSessionsAfterClose,
  snapshotVoiceEnforcementSessions,
  VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY,
  VoiceEnforcementSessionReleaseQueue,
} from '../src/lib/voiceEnforcementShutdown.js';
import {
  VoiceEnforcementExpiryFence,
  VoiceEnforcementLease,
} from '../src/lib/voiceEnforcementLease.js';

describe('voice-enforcement shutdown release', () => {
  beforeEach(() => {
    release.mockReset();
  });

  it('attempts every terminally closed exact session and retains failures', async () => {
    const first = { sessionGeneration: 'first' };
    const second = { sessionGeneration: 'second' };
    release
      .mockResolvedValueOnce(undefined)
      .mockRejectedValue(new Error('control plane unavailable'));

    const retained = await releaseVoiceEnforcementSessionsAfterClose([
      { data: { voiceEnforcementSession: first } },
      { data: { voiceEnforcementSession: second } },
      { data: {} },
    ] as never);

    expect(release).toHaveBeenCalledTimes(3);
    expect(release).toHaveBeenCalledWith(first);
    expect(release).toHaveBeenCalledWith(second);
    expect(retained).toBe(1);
  });

  it('deduplicates pending and held exact sessions without consuming queue capacity', async () => {
    const session = {
      sessionGeneration: 'duplicate-session',
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: 'user-a',
      credentialEpoch: 'epoch-a',
      socketId: 'socket-a',
    };
    const queue = new VoiceEnforcementSessionReleaseQueue();

    expect(queue.enqueue(session)).toBe(true);
    expect(queue.enqueue({ ...session, socketId: 'replacement-socket' })).toBe(true);
    release.mockResolvedValue(undefined);
    await queue.retryPending();

    expect(release).toHaveBeenCalledOnce();
    expect(release).toHaveBeenCalledWith({ ...session, socketId: 'replacement-socket' });
    expect(queue.outstandingCount()).toBe(0);

    const heldQueue = new VoiceEnforcementSessionReleaseQueue();
    heldQueue.holdAll([session]);
    heldQueue.holdAll([{ ...session, socketId: 'ignored-socket' }]);
    heldQueue.promoteHeld();
    await heldQueue.retryPending();
    expect(release).toHaveBeenCalledTimes(2);
    expect(release).toHaveBeenLastCalledWith(session);
    expect(heldQueue.outstandingCount()).toBe(0);
  });

  it('keeps a held session out of the pending retry when its release fails', async () => {
    const session = {
      sessionGeneration: 'held-failed-session',
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: 'user-a',
      credentialEpoch: 'epoch-a',
      socketId: 'socket-a',
    };
    const queue = new VoiceEnforcementSessionReleaseQueue();
    queue.enqueue(session);
    let rejectRelease!: (error: Error) => void;
    const releaseAttempt = new Promise<void>((_, reject) => {
      rejectRelease = reject;
    });
    release.mockReturnValueOnce(releaseAttempt);

    const retry = queue.retryPending();
    await Promise.resolve();
    queue.holdAll([session]);
    rejectRelease(new Error('control plane unavailable'));
    await retry;

    expect(queue.pendingCount()).toBe(0);
    expect(queue.heldCount()).toBe(1);
  });

  it('queues an exact terminal session through partition expiry and retries after recovery', async () => {
    const session = { sessionGeneration: 'session-a', nodeBootId: 'boot-a' };
    let now = 0;
    const lease = new VoiceEnforcementLease(() => now, 30_000);
    lease.renew();
    now = 30_001;
    const queue = new VoiceEnforcementSessionReleaseQueue();
    let terminallyClosed = false;
    const fence = new VoiceEnforcementExpiryFence(lease, async () => {
      terminallyClosed = true;
      queue.holdAll(
        snapshotVoiceEnforcementSessions([{ data: { voiceEnforcementSession: session } }] as never)
      );
      await queue.retryPending();
    });

    await fence.enforce();
    expect(terminallyClosed).toBe(true);
    expect(release).not.toHaveBeenCalled();
    expect(queue.pendingCount()).toBe(0);
    expect(queue.heldCount()).toBe(1);

    // A later socket/session object cannot retarget the immutable snapshot.
    session.nodeBootId = 'successor-boot';
    release.mockResolvedValueOnce(undefined);
    queue.promoteHeld();
    await queue.retryPending();
    expect(release).toHaveBeenCalledTimes(1);
    expect(release).toHaveBeenLastCalledWith({
      sessionGeneration: 'session-a',
      nodeBootId: 'boot-a',
    });
    expect(queue.pendingCount()).toBe(0);
  });

  it('keeps held expiry sessions out of an in-flight multi-batch retry', async () => {
    const sessions = Array.from({ length: 65 }, (_, index) => ({
      sessionGeneration: `held-session-${index}`,
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: `user-${index}`,
      credentialEpoch: 'epoch-a',
      socketId: `socket-${index}`,
    }));
    const queue = new VoiceEnforcementSessionReleaseQueue();
    let releaseBatch!: () => void;
    const batchReleased = new Promise<void>((resolve) => {
      releaseBatch = resolve;
    });
    release.mockImplementation(() => batchReleased);
    queue.enqueueAll(sessions);

    const retry = queue.retryPending();
    await Promise.resolve();
    expect(release).toHaveBeenCalledTimes(64);
    queue.holdAll([sessions[0]]);
    releaseBatch();
    await retry;

    expect(release).toHaveBeenCalledTimes(65);
    expect(queue.heldCount()).toBe(1);
    expect(queue.pendingCount()).toBe(0);
  });

  it('releases held expiry sessions only after explicit promotion', async () => {
    const session = {
      sessionGeneration: 'held-session',
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: 'user-a',
      credentialEpoch: 'epoch-a',
      socketId: 'socket-a',
    };
    const queue = new VoiceEnforcementSessionReleaseQueue();
    queue.holdAll([session]);
    release.mockResolvedValue(undefined);

    await queue.retryPending();
    expect(release).not.toHaveBeenCalled();

    queue.promoteHeld();
    await queue.retryPending();
    expect(release).toHaveBeenCalledWith(session);
    expect(queue.heldCount()).toBe(0);
    expect(queue.pendingCount()).toBe(0);
  });

  it('reports held-only terminal sessions as outstanding during shutdown', async () => {
    const session = {
      sessionGeneration: 'held-shutdown-session',
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: 'user-a',
      credentialEpoch: 'epoch-a',
      socketId: 'socket-a',
    };
    const queue = new VoiceEnforcementSessionReleaseQueue();
    queue.holdAll([session]);

    const outstanding = await releaseVoiceEnforcementSessionsAfterClose([], queue);

    expect(release).not.toHaveBeenCalled();
    expect(outstanding).toBe(1);
    expect(queue.heldCount()).toBe(1);
  });

  it('quarantines expiry sessions before disconnect cleanup can bypass the hold', () => {
    const source = readFileSync(resolve(import.meta.dirname, '../src/index.ts'), 'utf8');
    const start = source.indexOf('const voiceEnforcementExpiryFence =');
    const end = source.indexOf('// Durable authority outboxes', start);
    const expiryCallback = source.slice(start, end);

    expect(expiryCallback).toMatch(
      /voiceEnforcementReleaseQueue\.holdAll\(terminalSessions\)[\s\S]*?socket\.data\.voiceEnforcementSession = undefined;[\s\S]*?socket\.disconnect\(true\)/
    );
    expect(expiryCallback).not.toContain('retryPending');
  });

  it('retains a terminal join rollback release failure for exact-session retry', async () => {
    const session = {
      sessionGeneration: 'rollback-session',
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: 'user-a',
      credentialEpoch: 'epoch-a',
      socketId: 'socket-a',
    };
    const queue = new VoiceEnforcementSessionReleaseQueue();
    release.mockRejectedValueOnce(new Error('control plane unavailable'));

    await queue.releaseAfterTerminalClose(session);
    expect(queue.pendingCount()).toBe(1);

    session.nodeBootId = 'successor-boot';
    release.mockResolvedValueOnce(undefined);
    await queue.retryPending();

    expect(release).toHaveBeenLastCalledWith({
      sessionGeneration: 'rollback-session',
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel',
      userId: 'user-a',
      credentialEpoch: 'epoch-a',
      socketId: 'socket-a',
    });
    expect(queue.pendingCount()).toBe(0);
  });

  it('drains more than one retry batch when every release succeeds', async () => {
    const sessions = Array.from({ length: 65 }, (_, index) => ({
      sessionGeneration: `session-${index}`,
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: `user-${index}`,
      credentialEpoch: 'epoch-a',
      socketId: `socket-${index}`,
    }));
    const queue = new VoiceEnforcementSessionReleaseQueue();
    release.mockResolvedValue(undefined);
    queue.enqueueAll(sessions);

    await queue.retryPending();

    expect(release).toHaveBeenCalledTimes(65);
    expect(queue.pendingCount()).toBe(0);
  });

  it('stops after one fully failed batch instead of spinning retries', async () => {
    const sessions = Array.from({ length: 64 }, (_, index) => ({
      sessionGeneration: `failed-session-${index}`,
      nodeBootId: 'boot-a',
      roomId: 'room-a',
      roomKind: 'channel' as const,
      userId: `user-${index}`,
      credentialEpoch: 'epoch-a',
      socketId: `socket-${index}`,
    }));
    const queue = new VoiceEnforcementSessionReleaseQueue();
    release.mockRejectedValue(new Error('control plane unavailable'));
    queue.enqueueAll(sessions);

    await queue.retryPending();

    expect(release).toHaveBeenCalledTimes(64);
    expect(queue.pendingCount()).toBe(64);
  });

  it('latches capacity across pending and held rows without allowing shutdown success', async () => {
    const sessions = Array.from(
      { length: VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY },
      (_, index) => ({
        sessionGeneration: `capacity-session-${index}`,
        nodeBootId: 'boot-a',
        roomId: 'room-a',
        roomKind: 'channel' as const,
        userId: `user-${index}`,
        credentialEpoch: 'epoch-a',
        socketId: `socket-${index}`,
      })
    );
    const onSaturated = vi.fn();
    const queue = new VoiceEnforcementSessionReleaseQueue(onSaturated);
    queue.enqueueAll(sessions);
    queue.holdAll([sessions[0]]);

    const overflow = {
      ...sessions[0],
      sessionGeneration: 'overflow-session',
      socketId: 'overflow-socket',
    };
    release.mockRejectedValueOnce(new Error('control plane unavailable'));
    await queue.releaseAfterTerminalClose(overflow);

    expect(queue.pendingCount()).toBe(VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY - 1);
    expect(queue.heldCount()).toBe(1);
    expect(queue.saturated()).toBe(true);
    expect(queue.outstandingCount()).toBe(VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY + 1);
    expect(onSaturated).toHaveBeenCalledOnce();
    expect(onSaturated).toHaveBeenCalledWith(VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY, 1);

    release.mockResolvedValue(undefined);
    queue.promoteHeld();
    await queue.retryPending();

    expect(queue.pendingCount()).toBe(0);
    expect(queue.heldCount()).toBe(0);
    expect(queue.outstandingCount()).toBe(1);
    expect(queue.saturated()).toBe(true);
  });

  it('wires saturation to readiness and later join admission', () => {
    const source = readFileSync(resolve(import.meta.dirname, '../src/index.ts'), 'utf8');

    expect(source).toContain('voiceEnforcementReleaseQueue.saturated() ||');
    expect(source).toMatch(
      /\(\)\s*=>\s*shuttingDown\s*\|\|\s*voiceEnforcementReleaseQueue\.saturated\(\)\s*\|\|\s*!voiceEnforcementLease\.valid\(\)/
    );
  });
});
