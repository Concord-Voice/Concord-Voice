/**
 * Hard media-rate policer (#2153) — the PURE half.
 *
 * RoomManager reads cumulative RTP counters from mediasoup and hands them here
 * as plain numbers; this module turns them into verdicts. It has no runtime
 * mediasoup import and no input type carries `rtpParameters` or fmtp, so it
 * structurally cannot depend on anything the client DECLARES. That is the
 * point: CV-CAN-018 (the bitrate cap is a BWE advisory) and CV-CAN-020 (the
 * audio tier gate reads optional fmtp) both trusted the client.
 *
 * Every limit is derived from server-authoritative Participant caps, read live
 * on each tick. Every constant is a PR-only change: there are deliberately no
 * env vars (a media-plane knob reaches no container, and a cooldown knob set
 * to 1 silently disables repeat-offender handling).
 *
 * Design: [internal]specs/2026-09-23-2153-media-rate-policer-design.md §5.
 */
import type { MediaSource } from './roomManager.js';

// ---------------------------------------------------------------------------
// Constants, fixed by the T0 measurement (spec §9). The evidence and the reason for
// each value: [internal]reports/2026-09-23-2153-media-policer-t0.md. Each τ is
// hard-capped at TAU_HARD_CAP.
// ---------------------------------------------------------------------------

/** Tick period. */
export const POLICER_INTERVAL_MS = 5_000;
/** Bound on each getStats() read; mediasoup 3.26's Channel has no request timeout. */
export const POLICER_READ_TIMEOUT_MS = 1_000;
/** Flat FEC allowance. Mirrors the client's FEC_MAX_HEADROOM_PERCENT (+50%); parity-tested. */
export const FEC_HEADROOM = 1.5;
/**
 * Per-packet bytes beyond the encoder's bitrate: the RTP header and extensions (28) plus the
 * E2EE GCM tag and v5 trailer (16 + 22). Measured 67; rounded up to a multiple of 8.
 */
export const PER_PACKET_OVERHEAD_BYTES = 72;
export const TAU_AUDIO = 1.25;
export const TAU_PPS = 1.5;
export const TAU_AGGREGATE = 1.5;
/** No τ may exceed this; raising one past it is a design change, not a tuning change. */
export const TAU_HARD_CAP = 1.5;
/** Seconds of sustained excess a bucket may bank before it trips. */
export const TRIP_BUDGET_S = 10;
/** Consecutive failed ticks for one participant before the process reports degraded. */
export const FAILURE_STREAK_K = 3;
export const COOLDOWN_MS = 900_000;
export const STRIKE_WINDOW_MS = 3_600_000;

/** The effective constants, for the `Media policer started` log line. */
export const MEDIA_POLICER_CONSTANTS = Object.freeze({
  POLICER_INTERVAL_MS,
  POLICER_READ_TIMEOUT_MS,
  FEC_HEADROOM,
  PER_PACKET_OVERHEAD_BYTES,
  TAU_AUDIO,
  TAU_PPS,
  TAU_AGGREGATE,
  TRIP_BUDGET_S,
  FAILURE_STREAK_K,
  COOLDOWN_MS,
  STRIKE_WINDOW_MS,
});

// ponytail: fail-closed stand-ins for a malformed Participant cap. The
// entitlement parser already floors every field, so these are reachable only
// through a bug; they exist so a 0/NaN cap can never become an Infinity limit,
// which would be a silent fail-OPEN. Free-floor values, so they only err strict.
const FAIL_CLOSED_AUDIO_CEILING_BPS = 96_000;
const FAIL_CLOSED_MIN_PTIME_MS = 20;
const FAIL_CLOSED_MAX_MANUAL_BITRATE_BPS = 5_000_000;

// ---------------------------------------------------------------------------
// Limits and verdict vocabulary
// ---------------------------------------------------------------------------

/** Server-authoritative caps, read live from the Participant on every tick. */
export interface ParticipantCaps {
  /** `resolveAllowedOpusBitrateCeiling(participant.allowedAudioTiers)`. */
  readonly audioCeilingBps: number;
  readonly minPtimeMs: number;
  readonly maxManualBitrateBps: number;
}

export interface PolicerLimits {
  /** Per audio slot, RTP-layer bits per second after SRTP decryption (excludes IP/UDP headers and the SRTP tag). */
  readonly audioBps: number;
  /** Per audio slot, packets per second. Enforces minPtimeMs without reading fmtp. */
  readonly audioPps: number;
  /** Per send transport, RTP-layer bits per second after SRTP decryption (excludes IP/UDP headers and the SRTP tag). */
  readonly aggregateBps: number;
}

export type PolicedSlot = 'mic' | 'screen-audio';
export type PolicerCheck = 'audio_bytes' | 'audio_pps' | 'aggregate';
export type RatioBucket = '1-1.5x' | '1.5-2x' | '2-4x' | '>=4x';

export interface PolicerTrip {
  readonly roomId: string;
  readonly userId: string;
  readonly check: PolicerCheck;
  readonly ratioBucket: RatioBucket;
  /** The non-latched producer to pause, or null when none carried bytes (a strike only). */
  readonly producerId: string | null;
}

function positiveFinite(value: number, fallback: number): number {
  return Number.isFinite(value) && value > 0 ? value : fallback;
}

/**
 * The flat overhead allowance is sized at the NOMINAL packet rate, so a sender
 * that raises its packet rate pays for its own extra headers. FEC headroom is
 * flat rather than loss-aware because the sender controls the loss we observe.
 */
export function deriveLimits(caps: ParticipantCaps): PolicerLimits {
  const ceiling = positiveFinite(caps.audioCeilingBps, FAIL_CLOSED_AUDIO_CEILING_BPS);
  const ptimeMs = positiveFinite(caps.minPtimeMs, FAIL_CLOSED_MIN_PTIME_MS);
  const aggregate = positiveFinite(caps.maxManualBitrateBps, FAIL_CLOSED_MAX_MANUAL_BITRATE_BPS);
  const nominalPps = 1000 / ptimeMs;
  return {
    audioBps: (ceiling * FEC_HEADROOM + nominalPps * PER_PACKET_OVERHEAD_BYTES * 8) * TAU_AUDIO,
    audioPps: nominalPps * TAU_PPS,
    aggregateBps: aggregate * TAU_AGGREGATE,
  };
}

/** Coarse ratio of the tripping interval's rate to its limit. Never a rate series (C8). */
export function ratioBucketFor(ratio: number): RatioBucket {
  if (ratio >= 4) return '>=4x';
  if (ratio >= 2) return '2-4x';
  if (ratio >= 1.5) return '1.5-2x';
  return '1-1.5x';
}

/**
 * Every audio producer lands in a slot whatever source it declares; anything
 * not labelled `screen-audio` is metered as `mic`, the strictest reading.
 */
export function slotFor(source: MediaSource): PolicedSlot {
  return source === 'screen-audio' ? 'screen-audio' : 'mic';
}

const KEY_SEPARATOR = '\u0000';

/** Slot buckets outlive producer churn and same-room rejoin: the key has no producer id. */
export function slotKey(roomId: string, userId: string, slot: PolicedSlot): string {
  return [roomId, userId, slot].join(KEY_SEPARATOR);
}

function participantKey(roomId: string, userId: string): string {
  return [roomId, userId].join(KEY_SEPARATOR);
}

// ---------------------------------------------------------------------------
// Strikes and cooldown
// ---------------------------------------------------------------------------

export type StrikeAction = 'pause' | 'evict';

/** Test seam only. Production constructs `new MediaPolicyLedger()`. */
export interface MediaPolicyLedgerOptions {
  readonly cooldownMs?: number;
  readonly strikeWindowMs?: number;
}

interface LedgerEntry {
  strikes: number;
  lastStrikeAtMs: number;
  cooldownUntilMs: number;
}

/**
 * userId → strikes and cooldown, in process memory. SINGLE-NODE ASSUMPTION:
 * lost on every media-plane recreate (deploy, healthwatch restart). A second
 * media node would need a fail-closed shared store — Redis is not one, since
 * the media plane serves while Redis is down.
 */
export class MediaPolicyLedger {
  private readonly entries = new Map<string, LedgerEntry>();
  private readonly cooldownMs: number;
  private readonly strikeWindowMs: number;

  constructor(options: MediaPolicyLedgerOptions = {}) {
    this.cooldownMs = options.cooldownMs ?? COOLDOWN_MS;
    this.strikeWindowMs = options.strikeWindowMs ?? STRIKE_WINDOW_MS;
  }

  /**
   * Record one strike. A strike inside the window of the previous one evicts
   * and (re)arms the cooldown synchronously, before the caller can await
   * anything. Strikes survive the cooldown, so a trip after it ends but inside
   * the window evicts again.
   */
  strike(userId: string, nowMs: number): StrikeAction {
    const entry = this.entries.get(userId);
    if (!entry || nowMs - entry.lastStrikeAtMs > this.strikeWindowMs) {
      this.entries.set(userId, {
        strikes: 1,
        lastStrikeAtMs: nowMs,
        cooldownUntilMs: entry?.cooldownUntilMs ?? 0,
      });
      return 'pause';
    }
    entry.strikes += 1;
    entry.lastStrikeAtMs = nowMs;
    // The clock is monotonic, so a later strike never shortens a cooldown.
    entry.cooldownUntilMs = nowMs + this.cooldownMs;
    return 'evict';
  }

  cooldownRemainingMs(userId: string, nowMs: number): number {
    const entry = this.entries.get(userId);
    return entry ? Math.max(0, entry.cooldownUntilMs - nowMs) : 0;
  }

  /** Whole seconds until the cooldown ends, rounded up; 0 when none is active. */
  retryAfterSec(userId: string, nowMs: number): number {
    return Math.ceil(this.cooldownRemainingMs(userId, nowMs) / 1000);
  }

  /** Deletes an entry only once its cooldown has ended AND its last strike left the window. */
  prune(nowMs: number): void {
    for (const [userId, entry] of this.entries) {
      if (entry.cooldownUntilMs <= nowMs && nowMs - entry.lastStrikeAtMs > this.strikeWindowMs) {
        this.entries.delete(userId);
      }
    }
  }

  get size(): number {
    return this.entries.size;
  }
}

export interface UserVerdict {
  readonly userId: string;
  readonly action: StrikeAction;
  /** Every trip this user produced this tick; the tick latches each one's producer on 'pause'. */
  readonly trips: readonly PolicerTrip[];
  /** Set only for 'evict': whole seconds of cooldown, always >= 1. */
  readonly retryAfterSec: number | null;
}

/**
 * One strike per user per tick, however many checks tripped: a mic slot and
 * the aggregate tripping together is one offence, not a pause and an eviction.
 */
export function decideVerdicts(
  trips: readonly PolicerTrip[],
  ledger: MediaPolicyLedger,
  nowMs: number
): UserVerdict[] {
  const byUser = new Map<string, PolicerTrip[]>();
  for (const trip of trips) {
    const userTrips = byUser.get(trip.userId);
    if (userTrips) userTrips.push(trip);
    else byUser.set(trip.userId, [trip]);
  }
  const verdicts: UserVerdict[] = [];
  for (const [userId, userTrips] of byUser) {
    const action = ledger.strike(userId, nowMs);
    verdicts.push({
      userId,
      action,
      trips: userTrips,
      retryAfterSec: action === 'evict' ? ledger.retryAfterSec(userId, nowMs) : null,
    });
  }
  return verdicts;
}

// ---------------------------------------------------------------------------
// Readings and buckets
// ---------------------------------------------------------------------------

/** One RTP stream's cumulative counters. Simulcast layers are separate streams. */
export interface StreamCounter {
  readonly ssrc: number;
  readonly byteCount: number;
  readonly packetCount: number;
}

export interface ProducerReading {
  readonly producerId: string;
  readonly kind: 'audio' | 'video';
  readonly source: MediaSource;
  /** Latched by the policer. Excluded from attribution and subtracted from the aggregate. */
  readonly policed: boolean;
  /** Shared monotonic clock at creation: a new producer's baseline is zero bytes at this instant. */
  readonly createdAtMs: number;
  readonly streams: readonly StreamCounter[];
}

/** A participant whose every read succeeded. Only producers still open at settle time appear. */
export interface ParticipantReading {
  readonly roomId: string;
  readonly userId: string;
  readonly caps: ParticipantCaps;
  readonly sendTransportId: string;
  /** A new transport's baseline is zero bytes at this instant. */
  readonly sendTransportCreatedAtMs: number;
  readonly rtpBytesReceived: number;
  readonly rtxBytesReceived: number;
  readonly producers: readonly ProducerReading[];
}

/** A participant with any failed read this tick. Its whole state is held untouched. */
export interface FailedParticipant {
  readonly roomId: string;
  readonly userId: string;
  readonly sendTransportId: string;
  readonly producerIds: readonly string[];
}

export interface PolicerObservation {
  readonly nowMs: number;
  readonly readings: readonly ParticipantReading[];
  readonly failed: readonly FailedParticipant[];
}

export interface StalledParticipant {
  readonly roomId: string;
  readonly userId: string;
  readonly streak: number;
}

export type DegradedTransition = 'degraded' | 'restored' | null;

export interface ObserveResult {
  readonly trips: readonly PolicerTrip[];
  /** Process-level; each transition is reported exactly once. */
  readonly degradedTransition: DegradedTransition;
  /** Participants at or past FAILURE_STREAK_K, for the one aggregated warn line. */
  readonly stalled: readonly StalledParticipant[];
}

/** Last counters of an audio producer the client closed (RoomManager.closeProducerFromClient). */
export interface FinalProducerSample {
  readonly roomId: string;
  readonly userId: string;
  readonly producerId: string;
  readonly kind: 'audio' | 'video';
  readonly source: MediaSource;
  readonly createdAtMs: number;
  readonly caps: ParticipantCaps;
  /** null when the bounded final read failed or timed out: tolerated, nothing is added. */
  readonly streams: readonly StreamCounter[] | null;
}

interface StreamBase {
  bytes: number;
  pkts: number;
}

interface Delta {
  bytes: number;
  pkts: number;
}

export interface SlotBucketState {
  /** Who owns the bucket. Carried, never parsed back out of the slot key (red-team W1). */
  readonly roomId: string;
  readonly userId: string;
  lastEvalAtMs: number;
  debtBits: number;
  debtPkts: number;
  pendingBits: number;
  pendingPkts: number;
  /** Last-known limits, so a bucket whose participant left still drains at its own rate. */
  limits: PolicerLimits;
}

export interface AggregateBucketState {
  lastEvalAtMs: number;
  debtBits: number;
  /** rtp + rtx bytes at the last evaluation. */
  baseTransportBytes: number;
}

export interface PolicerStateSnapshot {
  readonly slots: ReadonlyMap<string, SlotBucketState>;
  readonly aggregates: ReadonlyMap<string, AggregateBucketState>;
  readonly baselines: ReadonlyMap<string, ReadonlyMap<number, Readonly<StreamBase>>>;
  readonly failStreaks: ReadonlyMap<string, number>;
  readonly degraded: boolean;
}

interface LiveSets {
  readonly producers: Set<string>;
  readonly transports: Set<string>;
}

const POLICED_SLOTS: readonly PolicedSlot[] = ['mic', 'screen-audio'];
const NO_DELTA: Delta = { bytes: 0, pkts: 0 };

/** A counter below its baseline means the stream was recreated: count it from zero. */
function counterDelta(current: number, base: number): number {
  return current >= base ? current - base : current;
}

function sumStreamDeltas(
  streams: readonly StreamCounter[],
  prior: ReadonlyMap<number, StreamBase> | undefined
): Delta {
  let bytes = 0;
  let pkts = 0;
  for (const stream of streams) {
    const base = prior?.get(stream.ssrc);
    bytes += counterDelta(stream.byteCount, base?.bytes ?? 0);
    pkts += counterDelta(stream.packetCount, base?.pkts ?? 0);
  }
  return { bytes, pkts };
}

function isCounter(value: number): boolean {
  return Number.isFinite(value) && value >= 0;
}

function hasWellFormedStreams(streams: readonly StreamCounter[]): boolean {
  return streams.every((s) => isCounter(s.byteCount) && isCounter(s.packetCount));
}

/** A NaN counter would poison debt and never trip; such a reading is treated as failed. */
function isWellFormed(reading: ParticipantReading): boolean {
  return (
    isCounter(reading.rtpBytesReceived) &&
    isCounter(reading.rtxBytesReceived) &&
    Number.isFinite(reading.sendTransportCreatedAtMs) &&
    reading.producers.every(
      (p) => Number.isFinite(p.createdAtMs) && hasWellFormedStreams(p.streams)
    )
  );
}

function asFailed(reading: ParticipantReading): FailedParticipant {
  return {
    roomId: reading.roomId,
    userId: reading.userId,
    sendTransportId: reading.sendTransportId,
    producerIds: reading.producers.map((p) => p.producerId),
  };
}

function rate(amount: number, dtS: number): number {
  return dtS > 0 ? amount / dtS : Number.POSITIVE_INFINITY;
}

function newSlotBucket(
  roomId: string,
  userId: string,
  lastEvalAtMs: number,
  limits: PolicerLimits
): SlotBucketState {
  return {
    roomId,
    userId,
    lastEvalAtMs,
    debtBits: 0,
    debtPkts: 0,
    pendingBits: 0,
    pendingPkts: 0,
    limits,
  };
}

/**
 * Sum the non-latched producers' deltas. The offender is the largest non-zero
 * one by bytes, and `pktOffender` the largest by packets: an `audio_pps` trip
 * names the producer that sent the packets, not a byte-heavier sibling. A slot
 * can briefly hold two producers while a client-closed one takes its final
 * read beside its replacement (F8).
 */
function sumUnlatched(
  producers: readonly ProducerReading[],
  deltas: ReadonlyMap<string, Delta>
): { bits: number; pkts: number; offender: string | null; pktOffender: string | null } {
  let bits = 0;
  let pkts = 0;
  let offender: string | null = null;
  let offenderBytes = 0;
  let pktOffender: string | null = null;
  let offenderPkts = 0;
  for (const producer of producers) {
    if (producer.policed) continue;
    const delta = deltas.get(producer.producerId) ?? NO_DELTA;
    bits += delta.bytes * 8;
    pkts += delta.pkts;
    if (delta.bytes > offenderBytes) {
      offenderBytes = delta.bytes;
      offender = producer.producerId;
    }
    if (delta.pkts > offenderPkts) {
      offenderPkts = delta.pkts;
      pktOffender = producer.producerId;
    }
  }
  return { bits, pkts, offender, pktOffender };
}

/**
 * Leaky bucket: debt grows by what arrived and drains at the limit over the
 * real elapsed time since the last GOOD evaluation, so a read after a gap
 * averages over the whole gap. Debt resets on a trip.
 */
function settleSlot(
  bucket: SlotBucketState,
  bits: number,
  pkts: number,
  nowMs: number
): { check: PolicerCheck; ratioBucket: RatioBucket } | null {
  const dtS = Math.max(0, nowMs - bucket.lastEvalAtMs) / 1000;
  const intervalBits = bits + bucket.pendingBits;
  const intervalPkts = pkts + bucket.pendingPkts;
  const { audioBps, audioPps } = bucket.limits;
  bucket.debtBits = Math.max(0, bucket.debtBits + intervalBits - audioBps * dtS);
  bucket.debtPkts = Math.max(0, bucket.debtPkts + intervalPkts - audioPps * dtS);
  bucket.pendingBits = 0;
  bucket.pendingPkts = 0;
  bucket.lastEvalAtMs = nowMs;

  let verdict: { check: PolicerCheck; ratioBucket: RatioBucket } | null = null;
  if (bucket.debtBits >= audioBps * TRIP_BUDGET_S) {
    verdict = {
      check: 'audio_bytes',
      ratioBucket: ratioBucketFor(rate(intervalBits, dtS) / audioBps),
    };
  } else if (bucket.debtPkts >= audioPps * TRIP_BUDGET_S) {
    verdict = {
      check: 'audio_pps',
      ratioBucket: ratioBucketFor(rate(intervalPkts, dtS) / audioPps),
    };
  }
  if (verdict) {
    bucket.debtBits = 0;
    bucket.debtPkts = 0;
  }
  return verdict;
}

/**
 * Stateful policer. One instance per process, driven only by the tick.
 *
 * - Slot buckets, keyed (roomId, userId, slot), hold every audio producer to
 *   the audio limit whatever source it declares. Self-paused producers count;
 *   latched ones do not.
 * - Aggregate buckets, keyed by send-transport id, bound the transport's
 *   producer-attributed RTP+RTX traffic, minus the latched producers' bytes.
 * - A failed read is a no-op for that participant: no baseline moves, no debt
 *   drains, no strike. Enforcement is delayed, not bypassed.
 */
export class MediaPolicer {
  private readonly baselines = new Map<string, Map<number, StreamBase>>();
  private readonly slots = new Map<string, SlotBucketState>();
  private readonly aggregates = new Map<string, AggregateBucketState>();
  private failStreaks = new Map<string, number>();
  private degraded = false;

  observe(input: PolicerObservation): ObserveResult {
    const failed = [...input.failed];
    const readings: ParticipantReading[] = [];
    for (const reading of input.readings) {
      if (isWellFormed(reading)) readings.push(reading);
      else failed.push(asFailed(reading));
    }
    const failedKeys = new Set(failed.map((f) => participantKey(f.roomId, f.userId)));
    const live: LiveSets = { producers: new Set(), transports: new Set() };
    for (const f of failed) {
      for (const id of f.producerIds) live.producers.add(id);
      live.transports.add(f.sendTransportId);
    }

    const trips: PolicerTrip[] = [];
    const evaluatedSlots = new Set<string>();
    for (const reading of readings) {
      for (const p of reading.producers) live.producers.add(p.producerId);
      live.transports.add(reading.sendTransportId);
      if (failedKeys.has(participantKey(reading.roomId, reading.userId))) continue;
      trips.push(...this.evaluateParticipant(reading, input.nowMs, evaluatedSlots));
    }
    trips.push(...this.drainUnreadSlots(input.nowMs, evaluatedSlots, failedKeys));
    this.prune(live);
    const { transition, stalled } = this.updateFailStreaks(failed);
    return { trips, degradedTransition: transition, stalled };
  }

  /**
   * Final sample for a client-closed audio producer: the delta since its
   * stored baseline becomes pending bits on its slot, and the baseline is
   * dropped. This is what makes close-and-re-produce churn visible: a producer
   * that lives between two ticks is never in any reading.
   */
  recordFinal(sample: FinalProducerSample): void {
    const prior = this.baselines.get(sample.producerId);
    this.baselines.delete(sample.producerId);
    if (sample.kind !== 'audio' || sample.streams === null) return;
    if (!hasWellFormedStreams(sample.streams)) return;
    const delta = sumStreamDeltas(sample.streams, prior);
    const key = slotKey(sample.roomId, sample.userId, slotFor(sample.source));
    let bucket = this.slots.get(key);
    if (!bucket) {
      bucket = newSlotBucket(
        sample.roomId,
        sample.userId,
        sample.createdAtMs,
        deriveLimits(sample.caps)
      );
      this.slots.set(key, bucket);
    }
    bucket.pendingBits += delta.bytes * 8;
    bucket.pendingPkts += delta.pkts;
  }

  /** Deep copy of internal state, for tests and debugging. */
  snapshot(): PolicerStateSnapshot {
    return {
      slots: new Map([...this.slots].map(([k, b]) => [k, { ...b, limits: { ...b.limits } }])),
      aggregates: new Map([...this.aggregates].map(([k, b]) => [k, { ...b }])),
      baselines: new Map(
        [...this.baselines].map(([id, streams]) => [
          id,
          new Map([...streams].map(([ssrc, base]) => [ssrc, { ...base }])),
        ])
      ),
      failStreaks: new Map(this.failStreaks),
      degraded: this.degraded,
    };
  }

  private evaluateParticipant(
    reading: ParticipantReading,
    nowMs: number,
    evaluatedSlots: Set<string>
  ): PolicerTrip[] {
    const limits = deriveLimits(reading.caps);
    const deltas = this.advanceBaselines(reading.producers);
    const trips: PolicerTrip[] = [];
    for (const slot of POLICED_SLOTS) {
      const key = slotKey(reading.roomId, reading.userId, slot);
      evaluatedSlots.add(key);
      const inSlot = reading.producers.filter(
        (p) => p.kind === 'audio' && slotFor(p.source) === slot
      );
      const trip = this.evaluateSlot(key, reading, inSlot, deltas, limits, nowMs);
      if (trip) trips.push(trip);
    }
    const aggregateTrip = this.evaluateAggregate(reading, deltas, limits, nowMs);
    if (aggregateTrip) trips.push(aggregateTrip);
    return trips;
  }

  private advanceBaselines(producers: readonly ProducerReading[]): Map<string, Delta> {
    const deltas = new Map<string, Delta>();
    for (const producer of producers) {
      deltas.set(
        producer.producerId,
        sumStreamDeltas(producer.streams, this.baselines.get(producer.producerId))
      );
      this.baselines.set(
        producer.producerId,
        new Map(producer.streams.map((s) => [s.ssrc, { bytes: s.byteCount, pkts: s.packetCount }]))
      );
    }
    return deltas;
  }

  private evaluateSlot(
    key: string,
    reading: ParticipantReading,
    inSlot: readonly ProducerReading[],
    deltas: ReadonlyMap<string, Delta>,
    limits: PolicerLimits,
    nowMs: number
  ): PolicerTrip | null {
    let bucket = this.slots.get(key);
    if (!bucket) {
      if (inSlot.length === 0) return null;
      bucket = newSlotBucket(
        reading.roomId,
        reading.userId,
        Math.min(...inSlot.map((p) => p.createdAtMs)),
        limits
      );
      this.slots.set(key, bucket);
    }
    bucket.limits = limits;
    const { bits, pkts, offender, pktOffender } = sumUnlatched(inSlot, deltas);
    const verdict = settleSlot(bucket, bits, pkts, nowMs);
    if (inSlot.length === 0 && bucket.debtBits === 0 && bucket.debtPkts === 0) {
      this.slots.delete(key);
    }
    if (!verdict) return null;
    const producerId = verdict.check === 'audio_pps' ? pktOffender : offender;
    return { roomId: reading.roomId, userId: reading.userId, ...verdict, producerId };
  }

  private evaluateAggregate(
    reading: ParticipantReading,
    deltas: ReadonlyMap<string, Delta>,
    limits: PolicerLimits,
    nowMs: number
  ): PolicerTrip | null {
    let bucket = this.aggregates.get(reading.sendTransportId);
    if (!bucket) {
      bucket = {
        lastEvalAtMs: reading.sendTransportCreatedAtMs,
        debtBits: 0,
        baseTransportBytes: 0,
      };
      this.aggregates.set(reading.sendTransportId, bucket);
    }
    const transportBytes = reading.rtpBytesReceived + reading.rtxBytesReceived;
    const transportDelta = counterDelta(transportBytes, bucket.baseTransportBytes);
    let latchedBytes = 0;
    for (const producer of reading.producers) {
      if (producer.policed) latchedBytes += (deltas.get(producer.producerId) ?? NO_DELTA).bytes;
    }
    // Latched bytes are a LOWER bound on the latched producer's true share of
    // transportDelta: the transport numerator also counts bytes no producer
    // counter ever sees (padding-only packets, unsolicited RTX, rejected
    // packets). Those bytes stay in `bits` after this subtraction, so it errs
    // STRICT, not lenient — the aggregate can over-attribute debt to
    // non-latched traffic, and `sumUnlatched(...).offender` below can name
    // the wrong producer when the true excess is unattributed ingress rather
    // than that producer's own bytes. The floor only guards a negative value;
    // it does not correct this bias.
    const bits = Math.max(0, transportDelta - latchedBytes) * 8;
    const dtS = Math.max(0, nowMs - bucket.lastEvalAtMs) / 1000;
    bucket.debtBits = Math.max(0, bucket.debtBits + bits - limits.aggregateBps * dtS);
    bucket.baseTransportBytes = transportBytes;
    bucket.lastEvalAtMs = nowMs;
    if (bucket.debtBits < limits.aggregateBps * TRIP_BUDGET_S) return null;
    bucket.debtBits = 0;
    return {
      roomId: reading.roomId,
      userId: reading.userId,
      check: 'aggregate',
      ratioBucket: ratioBucketFor(rate(bits, dtS) / limits.aggregateBps),
      producerId: sumUnlatched(reading.producers, deltas).offender,
    };
  }

  /**
   * Slots whose participant was neither read nor failed this tick (left the
   * room, or is between leave and rejoin) keep draining at their last-known
   * limit, and their pending final samples still count. They are deleted only
   * once drained, so a rejoin cannot reset an in-debt bucket.
   */
  private drainUnreadSlots(
    nowMs: number,
    evaluatedSlots: ReadonlySet<string>,
    failedKeys: ReadonlySet<string>
  ): PolicerTrip[] {
    const trips: PolicerTrip[] = [];
    for (const [key, bucket] of this.slots) {
      if (evaluatedSlots.has(key)) continue;
      const { roomId, userId } = bucket;
      if (failedKeys.has(participantKey(roomId, userId))) continue;
      const verdict = settleSlot(bucket, 0, 0, nowMs);
      if (bucket.debtBits === 0 && bucket.debtPkts === 0) this.slots.delete(key);
      if (verdict) trips.push({ roomId, userId, ...verdict, producerId: null });
    }
    return trips;
  }

  private prune(live: LiveSets): void {
    for (const id of this.baselines.keys()) {
      if (!live.producers.has(id)) this.baselines.delete(id);
    }
    for (const id of this.aggregates.keys()) {
      if (!live.transports.has(id)) this.aggregates.delete(id);
    }
  }

  private updateFailStreaks(failed: readonly FailedParticipant[]): {
    transition: DegradedTransition;
    stalled: StalledParticipant[];
  } {
    const next = new Map<string, number>();
    const stalled: StalledParticipant[] = [];
    for (const f of failed) {
      const key = participantKey(f.roomId, f.userId);
      if (next.has(key)) continue;
      const streak = (this.failStreaks.get(key) ?? 0) + 1;
      next.set(key, streak);
      if (streak >= FAILURE_STREAK_K) stalled.push({ roomId: f.roomId, userId: f.userId, streak });
    }
    this.failStreaks = next;
    const degraded = stalled.length > 0;
    let transition: DegradedTransition = null;
    if (degraded !== this.degraded) {
      transition = degraded ? 'degraded' : 'restored';
      this.degraded = degraded;
    }
    return { transition, stalled };
  }
}
