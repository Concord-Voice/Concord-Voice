/**
 * #2153 media-rate policer tick: the interval, the re-entrancy flag, and the
 * verdict applier. `mediaPolicer.ts` decides; RoomManager reads, latches and
 * pauses; this file only sequences them and says what happened. It receives
 * every dependency as a port (the `forceDisconnect.ts` shape) so the ordering
 * rules below are unit-tested rather than living in coverage-excluded index.ts.
 *
 * Ordering is the whole contract:
 *  - Everything between settling the sample and the last latch is ONE
 *    synchronous block. The identity fence is checked against the objects the
 *    settle captured, and an eviction's cooldown is set by ledger.strike()
 *    inside decideVerdicts(), so no join or produce can slip in between the
 *    verdict and its enforcement.
 *  - Evict verdicts latch too (spec R5): a latched producer's resume is
 *    refused while handleForceDisconnect tears the session down.
 *  - Pauses run before evictions, and every unit of work — each pauseLatched
 *    call, each room's eviction attempt — is isolated on its own, not merely
 *    each verdict. A latch is set before its pause, and a latched producer is
 *    excluded from every later reading, so a latch whose pause never ran
 *    would forward unmetered for the rest of its life: an evict verdict pauses
 *    its own latched producers too, after its per-room eviction attempts, so
 *    a failed eviction still stops the latch from forwarding unmetered
 *    (pausePolicedProducer returns 'gone' and is a no-op when the eviction
 *    already tore the producer down). One user's failed eviction must not
 *    cost another user their pause, and one room's failed eviction must not
 *    cost a sibling room its own.
 *
 * Logs carry the check and a ratio BUCKET, never a rate (C8: a VBR rate series
 * leaks speech activity).
 */
import {
  handleForceDisconnect,
  type ForceDisconnectIO,
  type ForceDisconnectRoomManager,
} from './forceDisconnect.js';
import { logger } from './logger.js';
import {
  MEDIA_POLICER_CONSTANTS,
  POLICER_INTERVAL_MS,
  decideVerdicts,
  type MediaPolicer,
  type MediaPolicyLedger,
  type ObserveResult,
  type PolicerTrip,
  type StalledParticipant,
  type UserVerdict,
} from './mediaPolicer.js';
import type { MediaPolicyNoticePayload, ProducerStateChangePayload } from './mediaPolicyWire.js';
import type {
  LatchedProducer,
  PolicedProducerRef,
  ProducerIngressSample,
  RoomManager,
} from './roomManager.js';
import type { EmitSecurityEvent, SecurityEventInput } from './securityEvent.js';

export interface MediaPolicerRoomManager extends ForceDisconnectRoomManager {
  collectProducerIngressSample: RoomManager['collectProducerIngressSample'];
  settleIngressSample: RoomManager['settleIngressSample'];
  latchPolicedProducer: RoomManager['latchPolicedProducer'];
  pausePolicedProducer: RoomManager['pausePolicedProducer'];
}

/** Verified: socket.io `Server` is assignable to this. */
export interface MediaPolicerIO extends ForceDisconnectIO {
  to(room: string): {
    except(socketId: string): { emit(event: string, ...args: unknown[]): unknown };
  };
}

export interface MediaPolicerTickPorts {
  readonly roomManager: MediaPolicerRoomManager;
  readonly io: MediaPolicerIO;
  readonly policer: MediaPolicer;
  readonly ledger: MediaPolicyLedger;
  readonly emit: EmitSecurityEvent | undefined;
  readonly now: () => number;
}

export interface MediaPolicerTick {
  start(): void;
  stop(): void;
  runOnce(): Promise<void>;
}

const PAUSE_EVENT: SecurityEventInput = {
  eventType: 'media_admission',
  outcome: 'denied',
  severity: 'medium',
  reasonCode: 'structural_limit_exceeded',
};
const STATS_DEGRADED_EVENT: SecurityEventInput = {
  eventType: 'security_control',
  outcome: 'degraded',
  severity: 'medium',
  reasonCode: 'dependency_unavailable',
};
const STATS_RESTORED_EVENT: SecurityEventInput = {
  eventType: 'security_control',
  outcome: 'restored',
  severity: 'informational',
  reasonCode: 'dependency_recovered',
};

/** The trip that latched a producer, kept so its pause is applied and logged once. */
interface LatchedTrip {
  readonly trip: PolicerTrip;
  readonly ref: PolicedProducerRef;
  readonly producer: LatchedProducer;
}

interface Decision {
  readonly verdicts: readonly UserVerdict[];
  readonly latched: ReadonlyMap<string, LatchedTrip>;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Which unit of isolated work failed, for a distinct log message per stage. */
interface IsolationContext {
  readonly stage: 'pause' | 'evict' | 'evict-latch-pause';
  readonly userId: string;
  readonly roomIds: readonly string[];
  readonly producerIds?: readonly string[];
}

/**
 * Runs one isolated unit of work so a failure in it cannot skip the work
 * after it (another pauseLatched call, another room's eviction, another
 * verdict). Each stage gets its own log message so a failure is traceable to
 * where it happened, not just that "a tick failed" somewhere.
 */
async function applyIsolated(apply: () => Promise<void>, context: IsolationContext): Promise<void> {
  try {
    await apply();
  } catch (error) {
    logger.error(`Media policer ${context.stage} failed`, {
      ...context,
      error: errorMessage(error),
      stack: error instanceof Error ? error.stack : undefined,
    });
  }
}

function stalledKey(s: StalledParticipant): string {
  return `${s.roomId}\u0000${s.userId}`;
}

function stalledSetsDiffer(a: ReadonlySet<string>, b: ReadonlySet<string>): boolean {
  if (a.size !== b.size) return true;
  for (const key of a) {
    if (!b.has(key)) return true;
  }
  return false;
}

export function createMediaPolicerTick(ports: MediaPolicerTickPorts): MediaPolicerTick {
  const { roomManager, io, policer, ledger } = ports;
  let timer: ReturnType<typeof setInterval> | null = null;
  let running = false;
  /** Keys of the currently-stalled set, while degraded; null when healthy. */
  let lastStalledKeys: ReadonlySet<string> | null = null;

  /** Telemetry is best-effort: an observer that throws cannot alter enforcement. */
  function observe(event: SecurityEventInput): void {
    try {
      ports.emit?.(event);
    } catch (error) {
      // Enforcement already happened; the audit trail is the only casualty,
      // but a silently-dropped security event should still be traceable.
      logger.warn('Media policer security event emit failed', {
        eventType: event.eventType,
        error: errorMessage(error),
      });
    }
  }

  function reportStatsTransition(result: ObserveResult): void {
    if (result.degradedTransition === 'degraded') {
      observe(STATS_DEGRADED_EVENT);
      logger.warn('Media policer stats degraded', { stalled: result.stalled });
      lastStalledKeys = new Set(result.stalled.map(stalledKey));
    } else if (result.degradedTransition === 'restored') {
      observe(STATS_RESTORED_EVENT);
      logger.info('Media policer stats restored');
      lastStalledKeys = null;
    } else if (lastStalledKeys !== null) {
      // Already degraded: log again only when the stalled SET changed
      // (a participant joined or left it), not on every tick it holds steady.
      const nextKeys = new Set(result.stalled.map(stalledKey));
      if (stalledSetsDiffer(lastStalledKeys, nextKeys)) {
        logger.warn('Media policer stats degraded', { stalled: result.stalled });
        lastStalledKeys = nextKeys;
      }
    }
  }

  /** Block B. Synchronous by construction: no await may appear in here. */
  function decide(sample: ProducerIngressSample): Decision {
    const now = ports.now();
    const settled = roomManager.settleIngressSample(sample, now);
    const result = policer.observe(settled.observation);
    reportStatsTransition(result);
    // One ledger.strike() per user; an 'evict' sets cooldownUntil right here.
    const verdicts = decideVerdicts(result.trips, ledger, now);
    const latched = new Map<string, LatchedTrip>();
    for (const verdict of verdicts) {
      for (const trip of verdict.trips) {
        // A slot trip and the aggregate can name the same producer: latch once.
        if (trip.producerId === null || latched.has(trip.producerId)) continue;
        const ref = settled.refs.get(trip.producerId);
        const producer = ref ? roomManager.latchPolicedProducer(ref) : null;
        if (ref && producer) latched.set(trip.producerId, { trip, ref, producer });
      }
    }
    return { verdicts, latched };
  }

  async function pauseLatched({ trip, ref, producer }: LatchedTrip): Promise<void> {
    const { roomId, userId, producerId } = ref;
    const outcome = await roomManager.pausePolicedProducer(ref);
    if (outcome === 'gone') {
      // Nothing to notify, but the strike still happened and must stay
      // traceable — same shape as the "nothing to pause" branch below.
      logger.warn('Media policer recorded strike', {
        userId,
        roomId,
        check: trip.check,
        ratioBucket: trip.ratioBucket,
      });
      return;
    }
    const { kind, source, socketId } = producer;
    if (outcome === 'paused') {
      try {
        const change: ProducerStateChangePayload = { producerId, userId, kind, source };
        io.to(roomId).except(socketId).emit('producer-paused', change);
        const notice: MediaPolicyNoticePayload = { producerId, kind, source, action: 'paused' };
        io.sockets.sockets.get(socketId)?.emit('media-policy-notice', notice);
      } catch (error) {
        // A failed emit must not skip the strike log below, nor the next
        // pause in this user's loop (F4): it is guarded on its own.
        logger.warn('Media policer pause notice failed', {
          userId,
          roomId,
          producerId,
          error: errorMessage(error),
        });
      }
    }
    observe(PAUSE_EVENT);
    // 'closed': pause() failed and RoomManager closed the producer instead. Its
    // producer-closed already reached the owner, so a notice would pin a row
    // for a producer that no longer exists (spec R9) — and it gets its own
    // log message rather than reusing the 'paused' one.
    logger.warn(
      outcome === 'closed' ? 'Media policer closed producer' : 'Media policer paused producer',
      {
        userId,
        roomId,
        producerId,
        kind,
        source,
        check: trip.check,
        ratioBucket: trip.ratioBucket,
        action: outcome,
      }
    );
  }

  async function applyPause(
    verdict: UserVerdict,
    latched: ReadonlyMap<string, LatchedTrip>
  ): Promise<void> {
    for (const trip of verdict.trips) {
      const hit = trip.producerId === null ? undefined : latched.get(trip.producerId);
      if (!hit) {
        // Nothing to pause (pending bits only, or the identity fence missed):
        // the strike stands and is recorded.
        logger.warn('Media policer recorded strike', {
          userId: trip.userId,
          roomId: trip.roomId,
          check: trip.check,
          ratioBucket: trip.ratioBucket,
        });
        continue;
      }
      if (hit.trip !== trip) continue; // a second check naming an already-latched producer
      // Isolated per call (F4): a throw pausing one producer must not skip
      // this user's remaining pauses.
      await applyIsolated(() => pauseLatched(hit), {
        stage: 'pause',
        userId: verdict.userId,
        roomIds: [hit.ref.roomId],
        producerIds: [hit.ref.producerId],
      });
    }
  }

  async function applyEvict(
    verdict: UserVerdict,
    latched: ReadonlyMap<string, LatchedTrip>
  ): Promise<void> {
    const retryAfterSec =
      verdict.retryAfterSec ?? ledger.retryAfterSec(verdict.userId, ports.now());
    const roomIds = [...new Set(verdict.trips.map((trip) => trip.roomId))];
    // handleForceDisconnect is silent when the user already left (e.g. a
    // drainUnreadSlots churn trip with producerId: null), so this is the
    // only trace a 15-minute cooldown left behind (F5).
    logger.warn('Media policer evicting participant', {
      userId: verdict.userId,
      roomIds,
      trips: verdict.trips.map((trip) => ({ check: trip.check, ratioBucket: trip.ratioBucket })),
      retryAfterSec,
    });
    // Eviction is room-scoped (spec Q1); the cooldown already covers the
    // node. Each room is isolated on its own (F3): a throw disconnecting one
    // room must not skip a sibling room.
    for (const roomId of roomIds) {
      await applyIsolated(
        () =>
          handleForceDisconnect(roomManager, io, roomId, verdict.userId, ports.emit, {
            reason: 'media_policy',
            retryAfterSec,
          }),
        { stage: 'evict', userId: verdict.userId, roomIds: [roomId] }
      );
    }
    // Evict verdicts latch too (spec R5): pause every latched producer this
    // verdict named, whether or not the eviction above succeeded (F3). A
    // successful eviction has already torn the producer down, so
    // pausePolicedProducer returns 'gone' and pauseLatched is a no-op; a
    // FAILED eviction must not leave the latch forwarding unmetered for the
    // rest of the producer's life.
    for (const trip of verdict.trips) {
      const hit = trip.producerId === null ? undefined : latched.get(trip.producerId);
      if (hit?.trip !== trip) continue;
      await applyIsolated(() => pauseLatched(hit), {
        stage: 'evict-latch-pause',
        userId: verdict.userId,
        roomIds: [hit.ref.roomId],
        producerIds: [hit.ref.producerId],
      });
    }
  }

  async function runOnce(): Promise<void> {
    if (running) {
      logger.debug('Media policer tick skipped: previous tick still running');
      return;
    }
    running = true;
    try {
      const sample = await roomManager.collectProducerIngressSample();
      const { verdicts, latched } = decide(sample);
      for (const verdict of verdicts) {
        if (verdict.action === 'pause') await applyPause(verdict, latched);
      }
      for (const verdict of verdicts) {
        if (verdict.action === 'evict') await applyEvict(verdict, latched);
      }
      ledger.prune(ports.now());
    } catch (error) {
      logger.error('Media policer tick failed', { error: errorMessage(error) });
    } finally {
      running = false;
    }
  }

  return {
    start(): void {
      if (timer) return;
      timer = setInterval(() => {
        void runOnce();
      }, POLICER_INTERVAL_MS);
      logger.info('Media policer started', { ...MEDIA_POLICER_CONSTANTS });
    },
    stop(): void {
      if (!timer) return;
      clearInterval(timer);
      timer = null;
    },
    runOnce,
  };
}
