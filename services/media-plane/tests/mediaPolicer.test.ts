import { describe, expect, it } from 'vitest';
import {
  COOLDOWN_MS,
  FAILURE_STREAK_K,
  FEC_HEADROOM,
  MEDIA_POLICER_CONSTANTS,
  MediaPolicer,
  MediaPolicyLedger,
  PER_PACKET_OVERHEAD_BYTES,
  POLICER_INTERVAL_MS,
  POLICER_READ_TIMEOUT_MS,
  STRIKE_WINDOW_MS,
  TAU_AGGREGATE,
  TAU_AUDIO,
  TAU_HARD_CAP,
  TAU_PPS,
  TRIP_BUDGET_S,
  decideVerdicts,
  deriveLimits,
  ratioBucketFor,
  slotFor,
  slotKey,
  type ObserveResult,
  type ParticipantCaps,
  type ParticipantReading,
  type PolicerTrip,
} from '../src/lib/mediaPolicer.js';
import type { MediaSource } from '../src/lib/roomManager.js';

const ROOM = 'room-1';
const USER = 'user-1';
const TRANSPORT = 'send-1';
/** Arbitrary monotonic origin; the policer only ever subtracts clock readings. */
const T0 = 1_000_000;

const FREE_CAPS: ParticipantCaps = {
  audioCeilingBps: 96_000,
  minPtimeMs: 20,
  maxManualBitrateBps: 5_000_000,
};
const PREMIUM_STUDIO_CAPS: ParticipantCaps = {
  audioCeilingBps: 510_000,
  minPtimeMs: 10,
  maxManualBitrateBps: 10_000_000,
};
/** Free: 216 kbps and 75 pps per audio slot, 7.5 Mbps per send transport. */
const FREE = deriveLimits(FREE_CAPS);

describe('media policer limits', () => {
  it('derives the free-tier limits from the Participant caps', () => {
    expect(deriveLimits(FREE_CAPS)).toEqual({
      audioBps: 216_000,
      audioPps: 75,
      aggregateBps: 7_500_000,
    });
  });

  it('derives the premium studio limits', () => {
    expect(deriveLimits(PREMIUM_STUDIO_CAPS)).toEqual({
      audioBps: 1_028_250,
      audioPps: 150,
      aggregateBps: 15_000_000,
    });
  });

  it('fails closed to the free floor when a cap is zero, negative or not finite', () => {
    for (const bad of [0, -1, Number.NaN, Number.POSITIVE_INFINITY]) {
      expect(
        deriveLimits({ audioCeilingBps: bad, minPtimeMs: bad, maxManualBitrateBps: bad })
      ).toEqual(FREE);
    }
  });

  it('buckets the tripping ratio without ever carrying the rate itself', () => {
    expect(ratioBucketFor(1)).toBe('1-1.5x');
    expect(ratioBucketFor(1.49)).toBe('1-1.5x');
    expect(ratioBucketFor(1.5)).toBe('1.5-2x');
    expect(ratioBucketFor(1.99)).toBe('1.5-2x');
    expect(ratioBucketFor(2)).toBe('2-4x');
    expect(ratioBucketFor(3.99)).toBe('2-4x');
    expect(ratioBucketFor(4)).toBe('>=4x');
    expect(ratioBucketFor(Number.POSITIVE_INFINITY)).toBe('>=4x');
  });

  it('maps every audio source onto a slot, so a relabelled source cannot escape', () => {
    expect(slotFor('mic')).toBe('mic');
    expect(slotFor('screen-audio')).toBe('screen-audio');
    expect(slotFor('camera')).toBe('mic');
    expect(slotFor('screen')).toBe('mic');
    expect(slotKey(ROOM, USER, 'mic')).not.toBe(slotKey(ROOM, USER, 'screen-audio'));
  });

  it('keeps every tau under the hard cap and the read timeout inside the tick', () => {
    expect(TAU_HARD_CAP).toBe(1.5);
    for (const tau of [TAU_AUDIO, TAU_PPS, TAU_AGGREGATE]) {
      expect(tau).toBeGreaterThan(1);
      expect(tau).toBeLessThanOrEqual(TAU_HARD_CAP);
    }
    expect(POLICER_READ_TIMEOUT_MS).toBeGreaterThanOrEqual(1_000);
    expect(POLICER_READ_TIMEOUT_MS).toBeLessThan(POLICER_INTERVAL_MS);
  });

  it('exposes the effective constants, frozen, for the start-up log', () => {
    expect(Object.isFrozen(MEDIA_POLICER_CONSTANTS)).toBe(true);
    expect(MEDIA_POLICER_CONSTANTS).toMatchObject({
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
  });
});

function trip(check: PolicerTrip['check'], producerId: string | null, userId = USER): PolicerTrip {
  return { roomId: ROOM, userId, check, ratioBucket: '2-4x', producerId };
}

describe('MediaPolicyLedger', () => {
  it('pauses on a first strike and evicts on a second inside the strike window', () => {
    const ledger = new MediaPolicyLedger();
    expect(ledger.strike(USER, T0)).toBe('pause');
    expect(ledger.cooldownRemainingMs(USER, T0)).toBe(0);

    const second = T0 + STRIKE_WINDOW_MS; // exactly W is still inside the window
    expect(ledger.strike(USER, second)).toBe('evict');
    expect(ledger.cooldownRemainingMs(USER, second)).toBe(COOLDOWN_MS);
    expect(ledger.retryAfterSec(USER, second)).toBe(COOLDOWN_MS / 1000);
  });

  it('treats a strike after the window has passed as a first strike again', () => {
    const ledger = new MediaPolicyLedger();
    ledger.strike(USER, T0);
    const afterWindow = T0 + STRIKE_WINDOW_MS + 1;
    expect(ledger.strike(USER, afterWindow)).toBe('pause');
    // Observably a fresh first strike, not a second one: the very next strike,
    // still inside THIS new window, evicts rather than pausing again.
    expect(ledger.strike(USER, afterWindow + 1)).toBe('evict');
  });

  it('ends the cooldown exactly at its deadline and rounds retryAfterSec up', () => {
    const ledger = new MediaPolicyLedger();
    ledger.strike(USER, T0);
    ledger.strike(USER, T0 + 1_000);
    const until = T0 + 1_000 + COOLDOWN_MS;
    expect(ledger.cooldownRemainingMs(USER, until - 1)).toBeGreaterThan(0);
    expect(ledger.retryAfterSec(USER, until - 1)).toBe(1);
    expect(ledger.cooldownRemainingMs(USER, until)).toBe(0);
    expect(ledger.retryAfterSec(USER, until)).toBe(0);
  });

  it('keeps strikes through the cooldown, so the next trip inside the window evicts again', () => {
    const ledger = new MediaPolicyLedger();
    ledger.strike(USER, T0);
    ledger.strike(USER, T0 + 1);
    const afterCooldown = T0 + 1 + COOLDOWN_MS + 1;
    expect(ledger.cooldownRemainingMs(USER, afterCooldown)).toBe(0);
    expect(ledger.strike(USER, afterCooldown)).toBe('evict');
  });

  it('prunes an entry only after its last strike leaves the window', () => {
    const ledger = new MediaPolicyLedger();
    ledger.strike(USER, T0);
    ledger.prune(T0 + STRIKE_WINDOW_MS);
    expect(ledger.size).toBe(1);
    ledger.prune(T0 + STRIKE_WINDOW_MS + 1);
    expect(ledger.size).toBe(0);
  });

  it('never prunes an active cooldown, even once the strike window has passed', () => {
    // A cooldown longer than the window is reachable only through the test seam;
    // it is what makes the cooldown clause of prune() observable at all.
    const ledger = new MediaPolicyLedger({ cooldownMs: 2 * STRIKE_WINDOW_MS });
    ledger.strike(USER, T0);
    ledger.strike(USER, T0 + 1);
    ledger.prune(T0 + 1 + STRIKE_WINDOW_MS + 1);
    expect(ledger.size).toBe(1);
    expect(ledger.cooldownRemainingMs(USER, T0 + 1 + STRIKE_WINDOW_MS + 1)).toBeGreaterThan(0);
    ledger.prune(T0 + 1 + 2 * STRIKE_WINDOW_MS);
    expect(ledger.size).toBe(0);
  });

  it('reports no cooldown for a user it has never seen', () => {
    const ledger = new MediaPolicyLedger();
    expect(ledger.cooldownRemainingMs('stranger', T0)).toBe(0);
    expect(ledger.retryAfterSec('stranger', T0)).toBe(0);
  });
});

describe('decideVerdicts', () => {
  it('records one strike per user per tick however many checks tripped', () => {
    const ledger = new MediaPolicyLedger();
    const trips = [trip('audio_bytes', 'mic-1'), trip('aggregate', 'cam-1')];
    expect(decideVerdicts(trips, ledger, T0)).toEqual([
      { userId: USER, action: 'pause', trips, retryAfterSec: null },
    ]);
  });

  it('evicts on the next tick that trips, carrying the cooldown as retryAfterSec', () => {
    const ledger = new MediaPolicyLedger();
    decideVerdicts([trip('audio_bytes', 'mic-1')], ledger, T0);
    const [verdict] = decideVerdicts([trip('audio_bytes', null)], ledger, T0 + POLICER_INTERVAL_MS);
    expect(verdict).toMatchObject({ action: 'evict', retryAfterSec: COOLDOWN_MS / 1000 });
    expect(ledger.cooldownRemainingMs(USER, T0 + POLICER_INTERVAL_MS)).toBeGreaterThan(0);
  });

  it('gives each user their own strike', () => {
    const ledger = new MediaPolicyLedger();
    const verdicts = decideVerdicts(
      [trip('audio_bytes', 'mic-a', 'user-a'), trip('audio_bytes', 'mic-b', 'user-b')],
      ledger,
      T0
    );
    expect(verdicts.map((v) => [v.userId, v.action])).toEqual([
      ['user-a', 'pause'],
      ['user-b', 'pause'],
    ]);
  });
});

// ---------------------------------------------------------------------------
// Simulation harness: one participant's send side, advanced in virtual time.
// ---------------------------------------------------------------------------

interface SimStream {
  ssrc: number;
  byteCount: number;
  packetCount: number;
}

interface SimProducer {
  id: string;
  kind: 'audio' | 'video';
  source: MediaSource;
  policed: boolean;
  createdAtMs: number;
  streams: SimStream[];
}

interface Flow {
  id: string;
  /** Wire bits per second on this stream. */
  bps: number;
  pps?: number;
  /** Simulcast layer index into the producer's streams; default 0. */
  layer?: number;
  /** Count these bytes as transport RTX (a repaired retransmission) rather than RTP. */
  rtx?: boolean;
}

interface TimedTrip {
  /** Elapsed virtual time since T0 when the tick that produced it ran. */
  atMs: number;
  trip: PolicerTrip;
}

class Sim {
  readonly policer = new MediaPolicer();
  nowMs = T0;
  rtpBytes = 0;
  rtxBytes = 0;
  readonly producers = new Map<string, SimProducer>();

  constructor(readonly caps: ParticipantCaps = FREE_CAPS) {}

  add(
    id: string,
    kind: 'audio' | 'video',
    source: MediaSource,
    opts: { ssrcs?: number[]; policed?: boolean } = {}
  ): void {
    this.producers.set(id, {
      id,
      kind,
      source,
      policed: opts.policed ?? false,
      createdAtMs: this.nowMs,
      streams: (opts.ssrcs ?? [1]).map((ssrc) => ({ ssrc, byteCount: 0, packetCount: 0 })),
    });
  }

  producer(id: string): SimProducer {
    const producer = this.producers.get(id);
    if (!producer) throw new Error(`no producer ${id}`);
    return producer;
  }

  advance(ms: number, flows: Flow[] = []): void {
    for (const flow of flows) {
      const stream = this.producer(flow.id).streams[flow.layer ?? 0];
      const bytes = (flow.bps * ms) / 8000;
      stream.byteCount += bytes;
      stream.packetCount += ((flow.pps ?? 0) * ms) / 1000;
      if (flow.rtx) this.rtxBytes += bytes;
      else this.rtpBytes += bytes;
    }
    this.nowMs += ms;
  }

  /** The stream was recreated by the worker: its cumulative counters restart from zero. */
  resetStream(id: string): void {
    const stream = this.producer(id).streams[0];
    stream.byteCount = 0;
    stream.packetCount = 0;
  }

  /** Raise a producer's own counter by bytes the transport never received. */
  inflate(id: string, bytes: number): void {
    this.producer(id).streams[0].byteCount += bytes;
  }

  reading(): ParticipantReading {
    return {
      roomId: ROOM,
      userId: USER,
      caps: this.caps,
      sendTransportId: TRANSPORT,
      sendTransportCreatedAtMs: T0,
      rtpBytesReceived: this.rtpBytes,
      rtxBytesReceived: this.rtxBytes,
      producers: [...this.producers.values()].map((p) => ({
        producerId: p.id,
        kind: p.kind,
        source: p.source,
        policed: p.policed,
        createdAtMs: p.createdAtMs,
        streams: p.streams.map((s) => ({ ...s })),
      })),
    };
  }

  tick(): ObserveResult {
    return this.policer.observe({ nowMs: this.nowMs, readings: [this.reading()], failed: [] });
  }

  tickFailed(): ObserveResult {
    return this.policer.observe({
      nowMs: this.nowMs,
      readings: [],
      failed: [
        {
          roomId: ROOM,
          userId: USER,
          sendTransportId: TRANSPORT,
          producerIds: [...this.producers.keys()],
        },
      ],
    });
  }

  /** The participant is in no room this tick (left, or between leave and rejoin). */
  tickAbsent(): ObserveResult {
    return this.policer.observe({ nowMs: this.nowMs, readings: [], failed: [] });
  }

  /** Client-initiated close: RoomManager.closeProducerFromClient hands over the last counters. */
  closeFromClient(id: string): void {
    const p = this.producer(id);
    this.policer.recordFinal({
      roomId: ROOM,
      userId: USER,
      producerId: id,
      kind: p.kind,
      source: p.source,
      createdAtMs: p.createdAtMs,
      caps: this.caps,
      streams: p.streams.map((s) => ({ ...s })),
    });
    this.producers.delete(id);
  }

  /** Runs `durationMs` in tick-sized steps under constant flows; returns every trip. */
  drive(durationMs: number, flows: Flow[]): TimedTrip[] {
    const trips: TimedTrip[] = [];
    for (let elapsed = 0; elapsed < durationMs; elapsed += POLICER_INTERVAL_MS) {
      this.advance(POLICER_INTERVAL_MS, flows);
      for (const t of this.tick().trips) trips.push({ atMs: this.nowMs - T0, trip: t });
    }
    return trips;
  }

  slotDebt(slot: 'mic' | 'screen-audio' = 'mic'): number | undefined {
    return this.policer.snapshot().slots.get(slotKey(ROOM, USER, slot))?.debtBits;
  }
}

const TEN_MINUTES_MS = 600_000;
const STOCK_MIC_PPS = 50;

describe('MediaPolicer — audio slots', () => {
  it('polices a mic that declares no fmtp at all on its observed rate (omitted params)', () => {
    // The reading has no rtpParameters field to omit: only observed counters exist.
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    const trips = sim.drive(30_000, [{ id: 'mic-1', bps: 510_000, pps: STOCK_MIC_PPS }]);
    // 510 kbps on a free account: limit·S/(rate−limit) ≈ 7.1 s, so the 10 s tick trips.
    expect(trips[0]).toEqual({
      atMs: 10_000,
      trip: {
        roomId: ROOM,
        userId: USER,
        check: 'audio_bytes',
        ratioBucket: '2-4x',
        producerId: 'mic-1',
      },
    });
    // Debt resets on a trip, so the next one needs a full budget of new excess.
    expect(trips.map((t) => t.atMs)).toEqual([10_000, 20_000, 30_000]);
  });

  it('polices a mic relabelled as screen-audio in its own slot (no exemption)', () => {
    const sim = new Sim();
    sim.add('sa-1', 'audio', 'screen-audio');
    const trips = sim.drive(30_000, [{ id: 'sa-1', bps: 510_000, pps: STOCK_MIC_PPS }]);
    expect(trips[0]).toMatchObject({ atMs: 10_000, trip: { producerId: 'sa-1' } });
    expect(sim.slotDebt('screen-audio')).toBeDefined();
    expect(sim.slotDebt('mic')).toBeUndefined();
  });

  it('never trips a stock mic at the free ceiling with full FEC headroom for ten minutes', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    const stockWireBps = 96_000 * FEC_HEADROOM + STOCK_MIC_PPS * PER_PACKET_OVERHEAD_BYTES * 8;
    expect(
      sim.drive(TEN_MINUTES_MS, [{ id: 'mic-1', bps: stockWireBps, pps: STOCK_MIC_PPS }])
    ).toEqual([]);
  });

  // T0-measured peak 10-s windows, server-side ([internal]reports/2026-09-23-2153-
  // media-policer-t0.md): the stock mic held at full FEC headroom, and stereo noise screen
  // audio at the tier cap. Sustained for ten minutes, neither may trip.
  it.each([
    ['mic', 'mic', 170_820, 58.1],
    ['screen-audio', 'screen-audio', 123_012, 50.1],
  ] as const)(
    'never trips the T0-measured stock %s peak for ten minutes',
    (_label, source, bps, pps) => {
      const sim = new Sim();
      sim.add('a-1', 'audio', source);
      expect(sim.drive(TEN_MINUTES_MS, [{ id: 'a-1', bps, pps }])).toEqual([]);
    }
  );

  it('never trips genuine system audio at the fit bound (0.8 x the limit) for ten minutes', () => {
    // Spec §9 fit criterion: every stock scenario's peak must sit at or under
    // 0.8 x its limit. The T0-measured peaks are the cases above.
    const sim = new Sim();
    sim.add('sa-1', 'audio', 'screen-audio');
    expect(
      sim.drive(TEN_MINUTES_MS, [{ id: 'sa-1', bps: 0.8 * FREE.audioBps, pps: STOCK_MIC_PPS }])
    ).toEqual([]);
  });

  it('does not trip on a 3 s burst at 3x followed by compliant audio', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.advance(3_000, [{ id: 'mic-1', bps: 3 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    sim.advance(2_000, [{ id: 'mic-1', bps: 0.8 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    expect(sim.tick().trips).toEqual([]);
    expect(
      sim.drive(TEN_MINUTES_MS, [{ id: 'mic-1', bps: 0.8 * FREE.audioBps, pps: STOCK_MIC_PPS }])
    ).toEqual([]);
  });

  it('does not trip a 50% duty cycle at 1.8x (average 0.9x)', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    for (let cycle = 0; cycle < 30; cycle++) {
      expect(
        sim.drive(10_000, [{ id: 'mic-1', bps: 1.8 * FREE.audioBps, pps: STOCK_MIC_PPS }])
      ).toEqual([]);
      expect(sim.drive(10_000, [])).toEqual([]);
    }
  });

  it('trips a sustained 1.3x at the predicted time, within one interval', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    const trips = sim.drive(120_000, [
      { id: 'mic-1', bps: 1.3 * FREE.audioBps, pps: STOCK_MIC_PPS },
    ]);
    const predictedMs = (TRIP_BUDGET_S * 1000) / (1.3 - 1); // limit·S/(rate−limit) ≈ 33.3 s
    expect(trips[0].atMs).toBeGreaterThanOrEqual(predictedMs);
    expect(trips[0].atMs).toBeLessThan(predictedMs + POLICER_INTERVAL_MS);
  });

  it('trips a free sender that raises its packet rate above minPtime allows', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    // 110 pps of small packets: 88 kbps is well under the byte limit; 75 pps is not.
    const trips = sim.drive(60_000, [{ id: 'mic-1', bps: 88_000, pps: 110 }]);
    expect(trips[0]).toMatchObject({
      atMs: 25_000,
      trip: { check: 'audio_pps', ratioBucket: '1-1.5x', producerId: 'mic-1' },
    });
  });

  it('names the packet sender, not a byte-heavier sibling, on an audio_pps trip', () => {
    // A client-closed producer takes its final read beside its admitted replacement
    // (F8), so a slot can briefly hold two. Bytes stay under the limit; packets do not.
    const sim = new Sim();
    sim.add('mic-heavy', 'audio', 'mic');
    sim.add('mic-flood', 'audio', 'mic');
    const trips = sim.drive(60_000, [
      { id: 'mic-heavy', bps: 100_000, pps: 20 },
      { id: 'mic-flood', bps: 40_000, pps: 90 },
    ]);
    expect(trips[0]).toMatchObject({ trip: { check: 'audio_pps', producerId: 'mic-flood' } });
  });

  it('lets a premium studio sender packetize at 10 ms without tripping', () => {
    const sim = new Sim(PREMIUM_STUDIO_CAPS);
    sim.add('mic-1', 'audio', 'mic');
    const studioWireBps = 510_000 + 100 * PER_PACKET_OVERHEAD_BYTES * 8;
    expect(sim.drive(TEN_MINUTES_MS, [{ id: 'mic-1', bps: studioWireBps, pps: 100 }])).toEqual([]);
  });

  it('counts a recreated stream from zero when its counter goes backwards', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    // A minute of compliant audio builds a baseline (1.27 MB) that the recreated
    // stream's first interval (530 kB) stays below — the only case a backwards
    // counter can be recognised at all.
    expect(
      sim.drive(60_000, [{ id: 'mic-1', bps: 0.8 * FREE.audioBps, pps: STOCK_MIC_PPS }])
    ).toEqual([]);
    sim.resetStream('mic-1');
    const trips = sim.drive(5_000, [{ id: 'mic-1', bps: 4 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    expect(trips).toHaveLength(1);
    expect(trips[0]).toMatchObject({ atMs: 65_000, trip: { check: 'audio_bytes' } });
  });

  it('meters a mic closed and re-produced every 2 s at 3x (slot continuity + final sample)', () => {
    const sim = new Sim();
    let n = 0;
    sim.add(`mic-${n}`, 'audio', 'mic');
    const trips: PolicerTrip[] = [];
    for (let second = 1; second <= 10; second++) {
      sim.advance(1_000, [{ id: `mic-${n}`, bps: 3 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
      if (second % 2 === 0) {
        sim.closeFromClient(`mic-${n}`);
        n += 1;
        sim.add(`mic-${n}`, 'audio', 'mic');
      }
      if (second % 5 === 0) trips.push(...sim.tick().trips);
    }
    expect(trips.length).toBeGreaterThan(0);
    expect(trips[0]).toMatchObject({ roomId: ROOM, userId: USER, check: 'audio_bytes' });
  });

  it('stops metering a latched mic that keeps sending, so one trip cannot become an eviction', () => {
    // Spec R13: a latched producer is paused, never escalated on persistence.
    // Bundled renderers without the notice keep sending; counting that traffic
    // would strike them again ~10 s later and evict a likely false positive.
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic', { policed: true });
    expect(sim.drive(TEN_MINUTES_MS, [{ id: 'mic-1', bps: 510_000, pps: STOCK_MIC_PPS }])).toEqual(
      []
    );
  });

  it('ignores a final sample whose read failed, and one for a video producer', () => {
    const sim = new Sim();
    sim.policer.recordFinal({
      roomId: ROOM,
      userId: USER,
      producerId: 'mic-x',
      kind: 'audio',
      source: 'mic',
      createdAtMs: T0,
      caps: FREE_CAPS,
      streams: null,
    });
    sim.policer.recordFinal({
      roomId: ROOM,
      userId: USER,
      producerId: 'cam-x',
      kind: 'video',
      source: 'camera',
      createdAtMs: T0,
      caps: FREE_CAPS,
      streams: [{ ssrc: 1, byteCount: 10_000_000, packetCount: 1_000 }],
    });
    expect(sim.policer.snapshot().slots.size).toBe(0);
  });

  it('evaluates a live cap upgrade against the NEW caps on the very next tick (free → premium)', () => {
    // 510 kbps is ~2.36x the free 216 kbps limit and trips it on the second
    // interval (1470 kbit of debt per interval against a 2160 kbit budget), but
    // sits well under the premium studio 1,028,250 bps limit. Held for three
    // intervals after the upgrade, it trips only if the bucket kept its stale
    // free limits.
    const policer = new MediaPolicer();
    const producerId = 'mic-1';
    const compliantBytes = (0.5 * FREE.audioBps * POLICER_INTERVAL_MS) / 8_000;
    const abusiveBytes = (510_000 * POLICER_INTERVAL_MS) / 8_000;

    function reading(caps: ParticipantCaps, byteCount: number): ParticipantReading {
      return {
        roomId: ROOM,
        userId: USER,
        caps,
        sendTransportId: TRANSPORT,
        sendTransportCreatedAtMs: T0,
        rtpBytesReceived: byteCount,
        rtxBytesReceived: 0,
        producers: [
          {
            producerId,
            kind: 'audio',
            source: 'mic',
            policed: false,
            createdAtMs: T0,
            streams: [{ ssrc: 1, byteCount, packetCount: 0 }],
          },
        ],
      };
    }

    // Tick 1, still free: compliant, establishes the baseline without tripping.
    const tick1 = policer.observe({
      nowMs: T0 + POLICER_INTERVAL_MS,
      readings: [reading(FREE_CAPS, compliantBytes)],
      failed: [],
    });
    expect(tick1.trips).toEqual([]);

    // Ticks 2-4: upgraded to premium studio caps between ticks, then 510 kbps
    // for three intervals, each measured against the premium limit.
    for (let interval = 1; interval <= 3; interval++) {
      const tick = policer.observe({
        nowMs: T0 + (1 + interval) * POLICER_INTERVAL_MS,
        readings: [reading(PREMIUM_STUDIO_CAPS, compliantBytes + interval * abusiveBytes)],
        failed: [],
      });
      expect(tick.trips).toEqual([]);
    }
  });
});

describe('MediaPolicer — failed reads', () => {
  it('holds all state untouched on a failed read: no strike, no drain, no baseline move', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.drive(10_000, [{ id: 'mic-1', bps: 1.8 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    const before = sim.policer.snapshot();
    expect(before.slots.get(slotKey(ROOM, USER, 'mic'))?.debtBits).toBeCloseTo(
      0.8 * FREE.audioBps * 10
    );

    sim.advance(POLICER_INTERVAL_MS);
    expect(sim.tickFailed().trips).toEqual([]);
    const after = sim.policer.snapshot();
    expect(after.slots).toEqual(before.slots);
    expect(after.aggregates).toEqual(before.aggregates);
    expect(after.baselines).toEqual(before.baselines);
  });

  it('averages the next good read over the whole gap', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.drive(5_000, [{ id: 'mic-1', bps: 0.8 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    // A 5 s burst at 3.5x, then silence, all across three failed ticks.
    sim.advance(5_000, [{ id: 'mic-1', bps: 3.5 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    expect(sim.tickFailed().trips).toEqual([]);
    sim.advance(5_000);
    sim.tickFailed();
    sim.advance(5_000);
    sim.tickFailed();
    sim.advance(5_000);
    // Averaged over 20 s the burst is under the limit; over 5 s it would trip.
    expect(sim.tick().trips).toEqual([]);
  });

  it('reports degraded once at the K-th consecutive failure and restored once on recovery', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    expect(sim.tick().degradedTransition).toBeNull();
    for (let i = 1; i < FAILURE_STREAK_K; i++) {
      sim.advance(POLICER_INTERVAL_MS);
      expect(sim.tickFailed().degradedTransition).toBeNull();
    }
    sim.advance(POLICER_INTERVAL_MS);
    const kth = sim.tickFailed();
    expect(kth.degradedTransition).toBe('degraded');
    expect(kth.stalled).toEqual([{ roomId: ROOM, userId: USER, streak: FAILURE_STREAK_K }]);
    sim.advance(POLICER_INTERVAL_MS);
    expect(sim.tickFailed().degradedTransition).toBeNull();
    sim.advance(POLICER_INTERVAL_MS);
    expect(sim.tick().degradedTransition).toBe('restored');
    sim.advance(POLICER_INTERVAL_MS);
    expect(sim.tick().degradedTransition).toBeNull();
  });

  it('treats a reading with a non-finite counter as failed rather than trusting it', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    const good = sim.reading();
    const bad: ParticipantReading = {
      ...good,
      producers: [
        { ...good.producers[0], streams: [{ ssrc: 1, byteCount: Number.NaN, packetCount: 0 }] },
      ],
    };
    let last: ObserveResult | undefined;
    for (let i = 0; i < FAILURE_STREAK_K; i++) {
      sim.advance(POLICER_INTERVAL_MS);
      last = sim.policer.observe({ nowMs: sim.nowMs, readings: [bad], failed: [] });
      expect(last.trips).toEqual([]);
    }
    expect(last?.degradedTransition).toBe('degraded');
  });
});

describe('MediaPolicer — lifecycle', () => {
  it('prunes a departed producer baseline and deletes its drained slot', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.drive(5_000, [{ id: 'mic-1', bps: 0.5 * FREE.audioBps, pps: 25 }]);
    expect(sim.policer.snapshot().baselines.has('mic-1')).toBe(true);
    expect(sim.slotDebt()).toBe(0);

    sim.producers.delete('mic-1'); // transport-close path: no final sample
    sim.advance(POLICER_INTERVAL_MS);
    sim.tick();
    const state = sim.policer.snapshot();
    expect(state.baselines.has('mic-1')).toBe(false);
    expect(state.slots.has(slotKey(ROOM, USER, 'mic'))).toBe(false);
  });

  it('keeps an in-debt slot across a leave so a rejoin cannot reset it, then deletes it drained', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.drive(10_000, [{ id: 'mic-1', bps: 1.8 * FREE.audioBps, pps: STOCK_MIC_PPS }]);
    expect(sim.slotDebt()).toBeCloseTo(0.8 * FREE.audioBps * 10);

    sim.advance(POLICER_INTERVAL_MS);
    sim.tickAbsent();
    expect(sim.slotDebt()).toBeCloseTo(0.8 * FREE.audioBps * 10 - FREE.audioBps * 5); // drained at its last-known limit
    sim.advance(POLICER_INTERVAL_MS);
    sim.tickAbsent();
    expect(sim.slotDebt()).toBeUndefined();
  });

  it('a drained slot names its OWN participant, never one re-derived from the key (red-team W1)', () => {
    // The slot key joins roomId and userId with U+0000. Re-deriving them by splitting the key
    // lets a roomId that contains the separator shift the trip, and so the strike and the
    // cooldown, onto another user. Unreachable today (both ids are validated UUIDs upstream),
    // but identity must come from the bucket, not from a parse of its key.
    const victim = '00000000-0000-4000-8000-000000000001';
    const hostileRoom = `room-1\u0000${victim}`;
    const policer = new MediaPolicer();
    policer.recordFinal({
      roomId: hostileRoom,
      userId: USER,
      producerId: 'mic-1',
      kind: 'audio',
      source: 'mic',
      createdAtMs: T0,
      caps: FREE_CAPS,
      streams: [{ ssrc: 1, byteCount: 10_000_000, packetCount: 50 }],
    });
    const { trips } = policer.observe({
      nowMs: T0 + POLICER_INTERVAL_MS,
      readings: [],
      failed: [],
    });
    expect(trips).toHaveLength(1); // positive control: the drain did trip
    expect(trips[0]).toMatchObject({ roomId: hostileRoom, userId: USER, producerId: null });
  });

  it('forgets a failure streak once the participant is gone', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.advance(POLICER_INTERVAL_MS);
    sim.tickFailed();
    expect(sim.policer.snapshot().failStreaks.size).toBe(1);
    sim.advance(POLICER_INTERVAL_MS);
    sim.tickAbsent();
    expect(sim.policer.snapshot().failStreaks.size).toBe(0);
  });
});

describe('MediaPolicer — aggregate', () => {
  const compliantMic: Flow = { id: 'mic-1', bps: 0.8 * FREE.audioBps, pps: STOCK_MIC_PPS };

  it('does not cascade an 8 Mbps latched producer into the aggregate', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.add('cam-1', 'video', 'camera', { policed: true });
    expect(sim.drive(TEN_MINUTES_MS, [compliantMic, { id: 'cam-1', bps: 8_000_000 }])).toEqual([]);
  });

  it('subtracts every simulcast layer of a latched producer', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.add('cam-1', 'video', 'camera', { policed: true, ssrcs: [11, 12, 13] });
    expect(
      sim.drive(TEN_MINUTES_MS, [
        compliantMic,
        { id: 'cam-1', layer: 0, bps: 1_000_000 },
        { id: 'cam-1', layer: 1, bps: 2_000_000 },
        { id: 'cam-1', layer: 2, bps: 5_000_000 },
      ])
    ).toEqual([]);
  });

  it('targets the non-latched producer with the largest delta', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.add('cam-1', 'video', 'camera');
    sim.add('screen-1', 'video', 'screen');
    const trips = sim.drive(60_000, [
      compliantMic,
      { id: 'cam-1', bps: 6_000_000 },
      { id: 'screen-1', bps: 4_800_000 },
    ]);
    expect(trips[0]).toEqual({
      atMs: 25_000,
      trip: {
        roomId: ROOM,
        userId: USER,
        check: 'aggregate',
        ratioBucket: '1-1.5x',
        producerId: 'cam-1',
      },
    });
    expect(trips.map((t) => t.atMs)).toEqual([25_000, 50_000]);
  });

  it('gives no headroom for repaired RTX inflating a latched producer', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.add('cam-1', 'video', 'camera', { policed: true });
    sim.add('screen-1', 'video', 'screen');
    const trips = sim.drive(60_000, [
      compliantMic,
      { id: 'cam-1', bps: 1_200_000 },
      { id: 'cam-1', bps: 2_400_000, rtx: true },
      { id: 'screen-1', bps: 9_600_000 },
    ]);
    expect(trips[0]).toMatchObject({
      atMs: 35_000,
      trip: { check: 'aggregate', producerId: 'screen-1' },
    });
  });

  it('never lets a latched delta larger than the transport delta pay down debt', () => {
    const sim = new Sim();
    sim.add('cam-1', 'video', 'camera', { policed: true });
    sim.add('screen-1', 'video', 'screen');
    sim.drive(15_000, [{ id: 'screen-1', bps: 10_800_000 }]);
    const aggregateDebt = () => sim.policer.snapshot().aggregates.get(TRANSPORT)?.debtBits;
    expect(aggregateDebt()).toBeCloseTo(49_500_000);

    sim.inflate('cam-1', 15_000_000); // +120 Mbit the transport never received
    sim.drive(5_000, [{ id: 'screen-1', bps: 10_800_000 }]);
    expect(aggregateDebt()).toBeCloseTo(12_000_000); // 49.5 + 0 − 37.5
  });

  it('records a strike with nothing to pause when no live producer carried the bytes', () => {
    const sim = new Sim();
    let trips: PolicerTrip[] = [];
    for (let i = 0; i < 6 && trips.length === 0; i++) {
      sim.rtpBytes += (10_800_000 * POLICER_INTERVAL_MS) / 8000;
      sim.advance(POLICER_INTERVAL_MS);
      trips = [...sim.tick().trips];
    }
    expect(trips).toEqual([
      { roomId: ROOM, userId: USER, check: 'aggregate', ratioBucket: '1-1.5x', producerId: null },
    ]);
  });

  it('drops the aggregate bucket when its transport leaves', () => {
    const sim = new Sim();
    sim.add('mic-1', 'audio', 'mic');
    sim.drive(5_000, [compliantMic]);
    expect(sim.policer.snapshot().aggregates.has(TRANSPORT)).toBe(true);
    sim.advance(POLICER_INTERVAL_MS);
    sim.tickAbsent();
    expect(sim.policer.snapshot().aggregates.has(TRANSPORT)).toBe(false);
  });
});
