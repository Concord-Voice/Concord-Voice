import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';

import { logger } from '../src/lib/logger.js';
import {
  MEDIA_POLICER_CONSTANTS,
  MediaPolicer,
  MediaPolicyLedger,
  POLICER_INTERVAL_MS,
  type ObserveResult,
  type ParticipantCaps,
  type PolicerObservation,
  type PolicerTrip,
  type StalledParticipant,
} from '../src/lib/mediaPolicer.js';
import {
  createMediaPolicerTick,
  type MediaPolicerIO,
  type MediaPolicerRoomManager,
} from '../src/lib/mediaPolicerTick.js';
import type {
  LatchedProducer,
  PolicedPauseOutcome,
  PolicedProducerRef,
  ProducerIngressSample,
} from '../src/lib/roomManager.js';

const ROOM = 'room-1';
const ROOM_2 = 'room-2';
const USER = 'user-1';
const USER_2 = 'user-2';
const SOCKET = 'socket-owner';
const SOCKET_2 = 'socket-owner-2';
const MIC = 'producer-mic';
const MIC_2 = 'producer-mic-2';
/** Arbitrary monotonic origin; only differences between readings matter. */
const NOW = 1_000_000;
const SAMPLE: ProducerIngressSample = { participants: [] };
const EMPTY_OBSERVATION: PolicerObservation = { nowMs: NOW, readings: [], failed: [] };

const PAUSE_EVENT = {
  eventType: 'media_admission',
  outcome: 'denied',
  severity: 'medium',
  reasonCode: 'structural_limit_exceeded',
};
const EVICT_EVENT = { ...PAUSE_EVENT, severity: 'high', routeTemplate: 'socket.force_disconnect' };

function trip(overrides: Partial<PolicerTrip> = {}): PolicerTrip {
  return {
    roomId: ROOM,
    userId: USER,
    check: 'audio_bytes',
    ratioBucket: '2-4x',
    producerId: MIC,
    ...overrides,
  };
}

/** A captured ref. The entry is a bare latch flag: the fakes never touch mediasoup. */
function refFor(producerId: string, userId = USER, roomId = ROOM): PolicedProducerRef {
  return {
    roomId,
    userId,
    producerId,
    participant: { socketId: userId === USER ? SOCKET : SOCKET_2 },
    entry: { policed: false },
  } as unknown as PolicedProducerRef;
}

interface HarnessOptions {
  trips?: readonly PolicerTrip[];
  refs?: readonly PolicedProducerRef[];
  observe?: (observation: PolicerObservation) => ObserveResult;
  policer?: MediaPolicer;
  observation?: PolicerObservation;
  pauseOutcome?: PolicedPauseOutcome;
  fenceMisses?: boolean;
}

function makeHarness(options: HarnessOptions = {}) {
  const order: string[] = [];
  const policedAtPause: boolean[] = [];
  const ledger = new MediaPolicyLedger();
  const refs = new Map((options.refs ?? []).map((ref) => [ref.producerId, ref]));

  const collectProducerIngressSample = vi.fn(async () => SAMPLE);
  const settleIngressSample = vi.fn(() => ({
    observation: options.observation ?? EMPTY_OBSERVATION,
    refs,
  }));
  const latchPolicedProducer = vi.fn((ref: PolicedProducerRef): LatchedProducer | null => {
    order.push(`latch:${ref.producerId}`);
    // The real fence refuses an already-latched entry; the fake keeps that rule.
    if (options.fenceMisses || ref.entry.policed) return null;
    ref.entry.policed = true;
    return { kind: 'audio', source: 'mic', socketId: ref.userId === USER ? SOCKET : SOCKET_2 };
  });
  const pausePolicedProducer = vi.fn(async (ref: PolicedProducerRef) => {
    order.push(`pause:${ref.producerId}`);
    policedAtPause.push(ref.entry.policed);
    return options.pauseOutcome ?? 'paused';
  });
  const sockets: Record<string, Record<string, string>> = {
    [ROOM]: { [USER]: SOCKET, [USER_2]: SOCKET_2 },
    [ROOM_2]: { [USER]: SOCKET_2 },
  };
  const getParticipant = vi.fn((roomId: string, userId: string) => {
    const socketId = sockets[roomId]?.[userId];
    return socketId ? { socketId } : undefined;
  });
  const leaveRoomIfSocketOwned = vi.fn(async () => true);
  const roomManager = {
    collectProducerIngressSample,
    settleIngressSample,
    latchPolicedProducer,
    pausePolicedProducer,
    getParticipant,
    getProvisionalParticipantSocketId: vi.fn(() => undefined),
    leaveRoomIfSocketOwned,
    removeProvisionalParticipantForEnforcement: vi.fn(async () => false),
  } as unknown as MediaPolicerRoomManager;

  const peerEmit = vi.fn();
  const except = vi.fn(() => ({ emit: peerEmit }));
  const to = vi.fn(() => ({ except }));
  const ownerEmit = vi.fn();
  const ownerEmit2 = vi.fn();
  const io = {
    to,
    sockets: {
      sockets: new Map([
        [SOCKET, { emit: ownerEmit, disconnect: vi.fn() }],
        [SOCKET_2, { emit: ownerEmit2, disconnect: vi.fn() }],
      ]),
    },
  } as unknown as MediaPolicerIO;

  const observe =
    options.observe ??
    ((): ObserveResult => ({
      trips: options.trips ?? [],
      degradedTransition: null,
      stalled: [],
    }));
  const policer = options.policer ?? ({ observe: vi.fn(observe) } as unknown as MediaPolicer);
  const securityEmit = vi.fn((_event: unknown) => true);
  const now = vi.fn(() => NOW);
  const tick = createMediaPolicerTick({
    roomManager,
    io,
    policer,
    ledger,
    emit: securityEmit,
    now,
  });

  return {
    tick,
    ledger,
    order,
    policedAtPause,
    collectProducerIngressSample,
    settleIngressSample,
    latchPolicedProducer,
    pausePolicedProducer,
    getParticipant,
    leaveRoomIfSocketOwned,
    to,
    except,
    peerEmit,
    ownerEmit,
    ownerEmit2,
    securityEmit,
    now,
  };
}

describe('media policer tick lifecycle', () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it('starts one interval, logs the effective constants once, and stop() clears it', async () => {
    vi.useFakeTimers();
    const h = makeHarness();

    h.tick.start();
    h.tick.start();
    expect(vi.getTimerCount()).toBe(1);
    expect(vi.mocked(logger.info).mock.calls).toStrictEqual([
      ['Media policer started', { ...MEDIA_POLICER_CONSTANTS }],
    ]);

    await vi.advanceTimersByTimeAsync(POLICER_INTERVAL_MS);
    expect(h.collectProducerIngressSample).toHaveBeenCalledTimes(1);

    h.tick.stop();
    h.tick.stop();
    expect(vi.getTimerCount()).toBe(0);
    await vi.advanceTimersByTimeAsync(POLICER_INTERVAL_MS * 3);
    expect(h.collectProducerIngressSample).toHaveBeenCalledTimes(1);
  });

  it('skips a run while one is in flight and clears the flag when it settles', async () => {
    const h = makeHarness();
    let release: (sample: ProducerIngressSample) => void = () => undefined;
    h.collectProducerIngressSample.mockImplementationOnce(
      () =>
        new Promise<ProducerIngressSample>((resolve) => {
          release = resolve;
        })
    );

    const first = h.tick.runOnce();
    await h.tick.runOnce();
    expect(h.collectProducerIngressSample).toHaveBeenCalledTimes(1);
    expect(vi.mocked(logger.debug).mock.calls).toStrictEqual([
      ['Media policer tick skipped: previous tick still running'],
    ]);

    release(SAMPLE);
    await first;
    await h.tick.runOnce();
    expect(h.collectProducerIngressSample).toHaveBeenCalledTimes(2);
  });

  it('never rejects: a failed run is logged and the next run proceeds', async () => {
    const h = makeHarness();
    h.settleIngressSample.mockImplementationOnce(() => {
      throw new Error('settle exploded');
    });

    await expect(h.tick.runOnce()).resolves.toBeUndefined();
    expect(vi.mocked(logger.error).mock.calls).toStrictEqual([
      ['Media policer tick failed', { error: 'settle exploded' }],
    ]);

    await h.tick.runOnce();
    expect(h.settleIngressSample).toHaveBeenCalledTimes(2);
  });

  it('prunes the ledger last, with a clock read taken after the verdicts were applied', async () => {
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)] });
    const prune = vi.spyOn(h.ledger, 'prune').mockImplementation(() => {
      h.order.push('prune');
    });
    h.now.mockReturnValueOnce(NOW).mockReturnValue(NOW + 7);

    await h.tick.runOnce();

    expect(prune.mock.calls).toStrictEqual([[NOW + 7]]);
    expect(h.order).toStrictEqual([`latch:${MIC}`, `pause:${MIC}`, 'prune']);
  });
});

describe('media policer tick ordering (#2153 latch-before-await)', () => {
  it('sets the cooldown and every latch before the first await', async () => {
    const h = makeHarness({
      trips: [trip(), trip({ userId: USER_2, producerId: MIC_2 })],
      refs: [refFor(MIC), refFor(MIC_2, USER_2)],
    });
    h.ledger.strike(USER, NOW - 1_000); // USER's next strike evicts
    let atFirstYield: { cooling: boolean; latched: string[] } | null = null;
    h.settleIngressSample.mockImplementationOnce(() => {
      // Runs at the tick's first await after block B, whatever that await is.
      queueMicrotask(() => {
        atFirstYield = {
          cooling: h.ledger.cooldownRemainingMs(USER, NOW) > 0,
          latched: h.latchPolicedProducer.mock.calls.map(([ref]) => ref.producerId).sort(),
        };
      });
      return {
        observation: EMPTY_OBSERVATION,
        refs: new Map([
          [MIC, refFor(MIC)],
          [MIC_2, refFor(MIC_2, USER_2)],
        ]),
      };
    });

    await h.tick.runOnce();

    expect(atFirstYield).toStrictEqual({ cooling: true, latched: [MIC, MIC_2].sort() });
  });

  it('latches before it pauses, so the pause only ever sees a latched entry', async () => {
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)] });

    await h.tick.runOnce();

    expect(h.order).toStrictEqual([`latch:${MIC}`, `pause:${MIC}`]);
    expect(h.policedAtPause).toStrictEqual([true]);
  });

  it('latches an evicted producer, whose post-eviction pause is a no-op once torn down (spec R5)', async () => {
    // Production: a successful eviction has already torn the producer down,
    // so pausePolicedProducer sees it as 'gone' — the fake models that here
    // rather than the R5 test masking the F3 latch-pause step as unreached.
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)], pauseOutcome: 'gone' });
    h.ledger.strike(USER, NOW - 1_000);

    await h.tick.runOnce();

    expect(h.order).toStrictEqual([`latch:${MIC}`, `pause:${MIC}`]);
    expect(h.ownerEmit.mock.calls).toStrictEqual([
      ['force-disconnect', { channelId: ROOM, reason: 'media_policy', retryAfterSec: 900 }],
    ]);
    expect(h.leaveRoomIfSocketOwned.mock.calls).toStrictEqual([[ROOM, USER, SOCKET]]);
    expect(h.securityEmit.mock.calls).toStrictEqual([[EVICT_EVENT]]);
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer evicting participant',
        {
          userId: USER,
          roomIds: [ROOM],
          trips: [{ check: 'audio_bytes', ratioBucket: '2-4x' }],
          retryAfterSec: 900,
        },
      ],
      [
        'Media policer recorded strike',
        { userId: USER, roomId: ROOM, check: 'audio_bytes', ratioBucket: '2-4x' },
      ],
    ]);
  });

  it('isolates eviction per room: room 1 throwing does not skip room 2', async () => {
    const h = makeHarness({
      trips: [trip(), trip({ roomId: ROOM_2, producerId: MIC_2 })],
      refs: [refFor(MIC), refFor(MIC_2, USER, ROOM_2)],
      pauseOutcome: 'gone', // isolates this case to the per-room eviction split (F3)
    });
    h.ledger.strike(USER, NOW - 1_000);
    h.getParticipant.mockImplementation((roomId: string, userId: string) => {
      if (roomId === ROOM) throw new Error('room 1 walk failed');
      if (roomId === ROOM_2 && userId === USER) return { socketId: SOCKET_2 };
      return undefined;
    });

    await h.tick.runOnce();

    expect(h.ownerEmit).not.toHaveBeenCalled();
    expect(h.leaveRoomIfSocketOwned.mock.calls).toStrictEqual([[ROOM_2, USER, SOCKET_2]]);
    expect(h.ownerEmit2.mock.calls).toStrictEqual([
      ['force-disconnect', { channelId: ROOM_2, reason: 'media_policy', retryAfterSec: 900 }],
    ]);
    expect(h.securityEmit.mock.calls).toStrictEqual([[EVICT_EVENT]]);
    expect(vi.mocked(logger.error).mock.calls).toHaveLength(1);
    expect(vi.mocked(logger.error).mock.calls[0]).toMatchObject([
      'Media policer evict failed',
      { stage: 'evict', userId: USER, roomIds: [ROOM], error: 'room 1 walk failed' },
    ]);
  });

  it('evicts once per distinct room, however many checks tripped there', async () => {
    const h = makeHarness({
      trips: [
        trip(),
        trip({ check: 'aggregate', producerId: null }),
        trip({ roomId: ROOM_2, producerId: MIC_2 }),
      ],
      refs: [refFor(MIC), refFor(MIC_2, USER, ROOM_2)],
    });
    h.ledger.strike(USER, NOW - 1_000);

    await h.tick.runOnce();

    expect(h.leaveRoomIfSocketOwned.mock.calls).toStrictEqual([
      [ROOM, USER, SOCKET],
      [ROOM_2, USER, SOCKET_2],
    ]);
  });

  it('lands a pause for one user even when another user eviction fails, and still pauses that latch', async () => {
    const h = makeHarness({
      trips: [trip(), trip({ userId: USER_2, producerId: MIC_2 })],
      refs: [refFor(MIC), refFor(MIC_2, USER_2)],
    });
    h.ledger.strike(USER, NOW - 1_000);
    h.getParticipant.mockImplementation(() => {
      throw new Error('room walk failed');
    });
    const prune = vi.spyOn(h.ledger, 'prune');

    await h.tick.runOnce();

    // USER_2's pause verdict (unrelated to USER's eviction) still lands, and
    // USER's own latched producer is paused too (F3), even though the
    // eviction that would otherwise have torn it down failed.
    expect(h.pausePolicedProducer).toHaveBeenCalledTimes(2);
    expect(h.pausePolicedProducer.mock.calls.map(([ref]) => ref.producerId).sort()).toEqual(
      [MIC, MIC_2].sort()
    );
    expect(h.ownerEmit2.mock.calls).toStrictEqual([
      [
        'media-policy-notice',
        { producerId: MIC_2, kind: 'audio', source: 'mic', action: 'paused' },
      ],
    ]);
    expect(h.ownerEmit.mock.calls).toStrictEqual([
      ['media-policy-notice', { producerId: MIC, kind: 'audio', source: 'mic', action: 'paused' }],
    ]);
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer paused producer',
        {
          userId: USER_2,
          roomId: ROOM,
          producerId: MIC_2,
          kind: 'audio',
          source: 'mic',
          check: 'audio_bytes',
          ratioBucket: '2-4x',
          action: 'paused',
        },
      ],
      [
        'Media policer evicting participant',
        {
          userId: USER,
          roomIds: [ROOM],
          trips: [{ check: 'audio_bytes', ratioBucket: '2-4x' }],
          retryAfterSec: 900,
        },
      ],
      [
        'Media policer paused producer',
        {
          userId: USER,
          roomId: ROOM,
          producerId: MIC,
          kind: 'audio',
          source: 'mic',
          check: 'audio_bytes',
          ratioBucket: '2-4x',
          action: 'paused',
        },
      ],
    ]);
    expect(vi.mocked(logger.error).mock.calls).toHaveLength(1);
    expect(vi.mocked(logger.error).mock.calls[0]).toMatchObject([
      'Media policer evict failed',
      { stage: 'evict', userId: USER, roomIds: [ROOM], error: 'room walk failed' },
    ]);
    expect(prune).toHaveBeenCalledOnce();
  });

  it('pauses one user before evicting another: pause runs in the earlier loop, eviction in the later one', async () => {
    const h = makeHarness({
      trips: [trip(), trip({ userId: USER_2, producerId: MIC_2 })],
      refs: [refFor(MIC), refFor(MIC_2, USER_2)],
    });
    // USER's trip is a first strike (pause); USER_2's is a second strike
    // inside the window (evict) — one pause verdict, one evict verdict, in
    // the same tick.
    h.ledger.strike(USER_2, NOW - 1_000);
    h.leaveRoomIfSocketOwned.mockImplementation(async (_roomId: string, userId: string) => {
      h.order.push(`evict:${userId}`);
      return true;
    });

    await h.tick.runOnce();

    const pauseIndex = h.order.indexOf(`pause:${MIC}`);
    const evictIndex = h.order.indexOf(`evict:${USER_2}`);
    expect(pauseIndex).toBeGreaterThanOrEqual(0);
    expect(evictIndex).toBeGreaterThanOrEqual(0);
    expect(pauseIndex).toBeLessThan(evictIndex);
  });
});

describe('media policer tick outcomes', () => {
  it('pauses to peers only, notifies the owner, and reports one pause', async () => {
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)] });

    await h.tick.runOnce();

    expect(h.to.mock.calls).toStrictEqual([[ROOM]]);
    expect(h.except.mock.calls).toStrictEqual([[SOCKET]]);
    expect(h.peerEmit.mock.calls).toStrictEqual([
      ['producer-paused', { producerId: MIC, userId: USER, kind: 'audio', source: 'mic' }],
    ]);
    expect(h.ownerEmit.mock.calls).toStrictEqual([
      ['media-policy-notice', { producerId: MIC, kind: 'audio', source: 'mic', action: 'paused' }],
    ]);
    expect(h.securityEmit.mock.calls).toStrictEqual([[PAUSE_EVENT]]);
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer paused producer',
        {
          userId: USER,
          roomId: ROOM,
          producerId: MIC,
          kind: 'audio',
          source: 'mic',
          check: 'audio_bytes',
          ratioBucket: '2-4x',
          action: 'paused',
        },
      ],
    ]);
  });

  it('reports a pause-failure close without any owner notice or peer pause (spec R9)', async () => {
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)], pauseOutcome: 'closed' });

    await h.tick.runOnce();

    expect(h.peerEmit).not.toHaveBeenCalled();
    expect(h.ownerEmit).not.toHaveBeenCalled();
    expect(h.securityEmit.mock.calls).toStrictEqual([[PAUSE_EVENT]]);
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer closed producer',
        {
          userId: USER,
          roomId: ROOM,
          producerId: MIC,
          kind: 'audio',
          source: 'mic',
          check: 'audio_bytes',
          ratioBucket: '2-4x',
          action: 'closed',
        },
      ],
    ]);
  });

  it('logs a recorded strike, but says nothing else, when the producer is gone by the time the pause lands', async () => {
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)], pauseOutcome: 'gone' });

    await h.tick.runOnce();

    expect(h.peerEmit).not.toHaveBeenCalled();
    expect(h.ownerEmit).not.toHaveBeenCalled();
    expect(h.securityEmit).not.toHaveBeenCalled();
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer recorded strike',
        { userId: USER, roomId: ROOM, check: 'audio_bytes', ratioBucket: '2-4x' },
      ],
    ]);
  });

  it('guards the peer emit and owner notice separately: a failed emit still records the strike and does not skip the next pause (F4)', async () => {
    const MIC_3 = 'producer-mic-3';
    const h = makeHarness({
      trips: [trip(), trip({ check: 'aggregate', producerId: MIC_3, ratioBucket: '1-1.5x' })],
      refs: [refFor(MIC), refFor(MIC_3)],
    });
    h.to.mockImplementationOnce(() => {
      throw new Error('emit down');
    });

    await h.tick.runOnce();

    expect(h.pausePolicedProducer).toHaveBeenCalledTimes(2);
    expect(h.peerEmit.mock.calls).toStrictEqual([
      ['producer-paused', { producerId: MIC_3, userId: USER, kind: 'audio', source: 'mic' }],
    ]);
    expect(h.ownerEmit.mock.calls).toStrictEqual([
      [
        'media-policy-notice',
        { producerId: MIC_3, kind: 'audio', source: 'mic', action: 'paused' },
      ],
    ]);
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer pause notice failed',
        { userId: USER, roomId: ROOM, producerId: MIC, error: 'emit down' },
      ],
      [
        'Media policer paused producer',
        {
          userId: USER,
          roomId: ROOM,
          producerId: MIC,
          kind: 'audio',
          source: 'mic',
          check: 'audio_bytes',
          ratioBucket: '2-4x',
          action: 'paused',
        },
      ],
      [
        'Media policer paused producer',
        {
          userId: USER,
          roomId: ROOM,
          producerId: MIC_3,
          kind: 'audio',
          source: 'mic',
          check: 'aggregate',
          ratioBucket: '1-1.5x',
          action: 'paused',
        },
      ],
    ]);
    expect(vi.mocked(logger.error)).not.toHaveBeenCalled();
  });

  it('records a strike with nothing to pause, and when the identity fence misses', async () => {
    const strikeOnly = makeHarness({ trips: [trip({ check: 'aggregate', producerId: null })] });
    await strikeOnly.tick.runOnce();
    expect(strikeOnly.latchPolicedProducer).not.toHaveBeenCalled();

    const fenceMiss = makeHarness({ trips: [trip()], refs: [refFor(MIC)], fenceMisses: true });
    await fenceMiss.tick.runOnce();
    expect(fenceMiss.pausePolicedProducer).not.toHaveBeenCalled();

    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer recorded strike',
        { userId: USER, roomId: ROOM, check: 'aggregate', ratioBucket: '2-4x' },
      ],
      [
        'Media policer recorded strike',
        { userId: USER, roomId: ROOM, check: 'audio_bytes', ratioBucket: '2-4x' },
      ],
    ]);
    expect(strikeOnly.securityEmit).not.toHaveBeenCalled();
    expect(fenceMiss.securityEmit).not.toHaveBeenCalled();
  });

  it('latches and pauses once when a slot check and the aggregate name one producer', async () => {
    const h = makeHarness({
      trips: [trip(), trip({ check: 'aggregate', ratioBucket: '1-1.5x' })],
      refs: [refFor(MIC)],
    });

    await h.tick.runOnce();

    expect(h.order).toStrictEqual([`latch:${MIC}`, `pause:${MIC}`]);
    expect(vi.mocked(logger.warn)).toHaveBeenCalledOnce();
    expect(vi.mocked(logger.warn)).toHaveBeenCalledWith(
      'Media policer paused producer',
      expect.objectContaining({ check: 'audio_bytes' })
    );
  });

  it('isolates each pause: a throw pausing one producer does not skip the next one (F4)', async () => {
    const MIC_3 = 'producer-mic-3';
    const h = makeHarness({
      trips: [trip(), trip({ check: 'aggregate', producerId: MIC_3, ratioBucket: '1-1.5x' })],
      refs: [refFor(MIC), refFor(MIC_3)],
    });
    h.pausePolicedProducer.mockImplementationOnce(async () => {
      throw new Error('worker gone');
    });

    await h.tick.runOnce();

    expect(h.pausePolicedProducer.mock.calls.map(([ref]) => ref.producerId)).toStrictEqual([
      MIC,
      MIC_3,
    ]);
    expect(h.peerEmit).toHaveBeenCalledWith(
      'producer-paused',
      expect.objectContaining({ producerId: MIC_3 })
    );
  });

  it('latches every verdict synchronously, before the first pause awaits', async () => {
    const h = makeHarness({
      trips: [trip(), trip({ userId: USER_2, producerId: MIC_2 })],
      refs: [refFor(MIC), refFor(MIC_2, USER_2)],
    });

    await h.tick.runOnce();

    expect(h.order).toStrictEqual([
      `latch:${MIC}`,
      `latch:${MIC_2}`,
      `pause:${MIC}`,
      `pause:${MIC_2}`,
    ]);
  });

  it('keeps enforcing when the security-event observer throws, and logs the drop (F11)', async () => {
    const h = makeHarness({ trips: [trip()], refs: [refFor(MIC)] });
    h.securityEmit.mockImplementation(() => {
      throw new Error('sink down');
    });

    await h.tick.runOnce();

    expect(h.ownerEmit).toHaveBeenCalledWith('media-policy-notice', expect.anything());
    expect(vi.mocked(logger.warn)).toHaveBeenCalledWith(
      'Media policer paused producer',
      expect.objectContaining({ action: 'paused' })
    );
    expect(vi.mocked(logger.warn)).toHaveBeenCalledWith(
      'Media policer security event emit failed',
      {
        eventType: PAUSE_EVENT.eventType,
        error: 'sink down',
      }
    );
    expect(vi.mocked(logger.error)).not.toHaveBeenCalled();
  });

  it('emits one event per stats transition, with the exact tuples', async () => {
    const stalled: StalledParticipant[] = [{ roomId: ROOM, userId: USER, streak: 3 }];
    const results: ObserveResult[] = [
      { trips: [], degradedTransition: 'degraded', stalled },
      { trips: [], degradedTransition: null, stalled },
      { trips: [], degradedTransition: 'restored', stalled: [] },
    ];
    const h = makeHarness({ observe: () => results.shift() as ObserveResult });

    await h.tick.runOnce();
    await h.tick.runOnce();
    await h.tick.runOnce();

    expect(h.securityEmit.mock.calls).toStrictEqual([
      [
        {
          eventType: 'security_control',
          outcome: 'degraded',
          severity: 'medium',
          reasonCode: 'dependency_unavailable',
        },
      ],
      [
        {
          eventType: 'security_control',
          outcome: 'restored',
          severity: 'informational',
          reasonCode: 'dependency_recovered',
        },
      ],
    ]);
    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      ['Media policer stats degraded', { stalled }],
    ]);
    expect(vi.mocked(logger.info).mock.calls).toStrictEqual([['Media policer stats restored']]);
  });

  it('logs again when the stalled set changes while already degraded (F11)', async () => {
    const stalledA: StalledParticipant[] = [{ roomId: ROOM, userId: USER, streak: 3 }];
    const stalledB: StalledParticipant[] = [
      { roomId: ROOM, userId: USER, streak: 4 },
      { roomId: ROOM_2, userId: USER_2, streak: 3 },
    ];
    const results: ObserveResult[] = [
      { trips: [], degradedTransition: 'degraded', stalled: stalledA },
      { trips: [], degradedTransition: null, stalled: stalledA }, // unchanged set: no repeat log
      { trips: [], degradedTransition: null, stalled: stalledB }, // a participant joined: logs again
    ];
    const h = makeHarness({ observe: () => results.shift() as ObserveResult });

    await h.tick.runOnce();
    await h.tick.runOnce();
    await h.tick.runOnce();

    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      ['Media policer stats degraded', { stalled: stalledA }],
      ['Media policer stats degraded', { stalled: stalledB }],
    ]);
  });
});

describe('media policer tick logs (C8)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('logs a real trip with its bucket and never with a rate or counter', async () => {
    const caps: ParticipantCaps = {
      audioCeilingBps: 96_000,
      minPtimeMs: 20,
      maxManualBitrateBps: 5_000_000,
    };
    const BYTES = 1_000_003; // ~1.6 Mbps over 5 s against a 216 kbps free limit
    const observation: PolicerObservation = {
      nowMs: NOW,
      failed: [],
      readings: [
        {
          roomId: ROOM,
          userId: USER,
          caps,
          sendTransportId: 'send-1',
          sendTransportCreatedAtMs: NOW - 5_000,
          rtpBytesReceived: BYTES,
          rtxBytesReceived: 0,
          producers: [
            {
              producerId: MIC,
              kind: 'audio',
              source: 'mic',
              policed: false,
              createdAtMs: NOW - 5_000,
              streams: [{ ssrc: 1111, byteCount: BYTES, packetCount: 250 }],
            },
          ],
        },
      ],
    };
    const h = makeHarness({ policer: new MediaPolicer(), observation, refs: [refFor(MIC)] });

    await h.tick.runOnce();

    expect(vi.mocked(logger.warn).mock.calls).toStrictEqual([
      [
        'Media policer paused producer',
        {
          userId: USER,
          roomId: ROOM,
          producerId: MIC,
          kind: 'audio',
          source: 'mic',
          check: 'audio_bytes',
          ratioBucket: '>=4x',
          action: 'paused',
        },
      ],
    ]);
    const everyLogLine = JSON.stringify(
      (['info', 'warn', 'error', 'debug'] as const).flatMap(
        (level) => vi.mocked(logger[level]).mock.calls
      )
    );
    for (const counter of [BYTES, BYTES * 8, 250]) {
      expect(everyLogLine).not.toContain(String(counter));
    }
  });
});
