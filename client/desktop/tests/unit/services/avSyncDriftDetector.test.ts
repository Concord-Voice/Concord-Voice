// @vitest-environment node
//
// Red-phase suite for #2941 (spec: [internal]specs/2026-09-11-2941-av-sync-drift-detector-design.md).
// `avSyncDriftDetector.ts` does not exist yet — this file is expected to fail on
// import resolution until Task 1 writes the module.
import { describe, expect, it } from 'vitest';
import {
  createAvSyncDriftDetector,
  mergeAvSyncStats,
  theilSenSlopeMsPerMs,
  DRIFT_SLOPE_MS_PER_SEC,
  FIELD_ABSENCE_STREAK_TO_REPORT,
  HISTORY_LIMIT,
  MAX_CLOCK_SKEW_MS,
  MAX_STEP_MS,
  MAX_STEP_RATE_MS_PER_SEC,
  MIN_EXCURSION_MS,
  MIN_SAMPLES,
  MIN_WINDOW_MS,
  SAMPLE_INTERVAL_MS,
  STALE_AFTER_MS,
  type AvSyncObservation,
} from '../../../src/renderer/services/voice/avSyncDriftDetector';

/**
 * Three `decoderBudgetSampler`-inherited union members are NOT exercised below:
 * `paused`, `no-progress`, `invalid-derived-metrics`.
 *
 * - `paused`: `observe(entries, observedAtMs)` (spec §7.1) carries no pause input at all —
 *   there is no boolean this module could read to produce it.
 * - `no-progress`: decoderBudgetSampler's version catches a raw counter that stalled between
 *   samples. This module's per-sample skew is a direct subtraction (`R.timestamp - R.remoteTimestamp`),
 *   not an interval delta of an accumulating counter, so there is no analogous "zero delta on an
 *   otherwise-healthy counter" case distinct from `sr-not-advanced` (guard #11), which already owns
 *   "remoteTimestamp did not advance".
 * - `invalid-derived-metrics`: this module's only derived arithmetic is subtraction (skew,
 *   jbSkewMs) and a median of pairwise slopes (Theil-Sen) over already-finite, already-validated
 *   inputs (guard #4 rejects non-finite raw fields before any arithmetic runs). No combination of
 *   finite doubles fed through subtraction and division-by-a-bounded-count is reachable to
 *   non-finite here, unlike decoderBudgetSampler's rho/fps computation.
 *
 * Do not fabricate a case for these. If the implementation genuinely can reach one, add a test;
 * otherwise this comment is the evidence for deleting the arm from the union (plan Task 2).
 */

const BASE_MS = 1_000_000;

interface KindFixture {
  ssrc: number;
  trackId: string;
  timestampMs: number;
  offsetMs: number;
  remoteTimestampMs?: number;
  jbDelaySec: number;
  jbCount: number;
  includeInbound?: boolean;
  includeOutbound?: boolean;
  remoteIdOverride?: string;
  outboundLocalIdOverride?: string;
}

function defaultKind(
  kind: 'audio' | 'video',
  timestampMs: number,
  offsetMs: number,
  overrides: Partial<KindFixture> = {}
): KindFixture {
  return {
    ssrc: kind === 'audio' ? 11 : 22,
    trackId: `track-${kind}`,
    timestampMs,
    offsetMs,
    jbDelaySec: 0.05,
    jbCount: 1_000,
    ...overrides,
  };
}

function kindEntries(kind: 'audio' | 'video', f: KindFixture): Array<[string, unknown]> {
  const inId = `IN-${kind}`;
  const roId = `RO-${kind}`;
  const entries: Array<[string, unknown]> = [];

  if (f.includeInbound !== false) {
    entries.push([
      inId,
      {
        id: inId,
        type: 'inbound-rtp',
        kind,
        ssrc: f.ssrc,
        trackIdentifier: f.trackId,
        remoteId: f.remoteIdOverride ?? roId,
        timestamp: f.timestampMs,
        packetsReceived: 500,
        jitterBufferDelay: f.jbDelaySec,
        jitterBufferEmittedCount: f.jbCount,
      },
    ]);
  }

  if (f.includeOutbound !== false) {
    const remoteTimestamp = f.remoteTimestampMs ?? f.timestampMs - f.offsetMs;
    entries.push([
      roId,
      {
        id: roId,
        type: 'remote-outbound-rtp',
        kind,
        localId: f.outboundLocalIdOverride ?? inId,
        timestamp: f.timestampMs,
        remoteTimestamp,
      },
    ]);
  }

  return entries;
}

/** Build one getStats()-shaped report from explicit per-kind fixtures. `null` omits the kind entirely. */
function frame(audio: KindFixture | null, video: KindFixture | null): Map<string, unknown> {
  const entries: Array<[string, unknown]> = [];
  if (audio) entries.push(...kindEntries('audio', audio));
  if (video) entries.push(...kindEntries('video', video));
  return new Map(entries);
}

/** A fully-formed, correctly-paired two-kind frame at one instant, with explicit per-kind offsets. */
function pair(
  timestampMs: number,
  audioOffsetMs: number,
  videoOffsetMs: number,
  jb: {
    audioDelaySec?: number;
    audioCount?: number;
    videoDelaySec?: number;
    videoCount?: number;
  } = {}
): Map<string, unknown> {
  const audio = defaultKind('audio', timestampMs, audioOffsetMs, {
    jbDelaySec: jb.audioDelaySec ?? 0.05,
    jbCount: jb.audioCount ?? 1_000,
  });
  const video = defaultKind('video', timestampMs, videoOffsetMs, {
    jbDelaySec: jb.videoDelaySec ?? 0.05,
    jbCount: jb.videoCount ?? 1_000,
  });
  return frame(audio, video);
}

/** Feed n frames at SAMPLE_INTERVAL_MS apart; audio offset grows by stepPerSample each frame,
 *  video offset stays fixed, so skew(i) = i * stepPerSample and the slope is exactly
 *  stepPerSample / (SAMPLE_INTERVAL_MS / 1000) ms/s. Returns the final observation. */
function runLinearSeries(n: number, stepPerSample: number): AvSyncObservation {
  const d = createAvSyncDriftDetector();
  let last!: AvSyncObservation;
  for (let i = 0; i < n; i++) {
    const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
    last = d.observe(pair(t, 100 + i * stepPerSample, 100).values(), t);
  }
  return last;
}

function expectDrift(
  observation: AvSyncObservation
): Extract<AvSyncObservation, { usable: true; verdict: 'drift' }> {
  if (!observation.usable || observation.verdict !== 'drift') {
    throw new Error(`expected a drift verdict, got ${JSON.stringify(observation)}`);
  }
  return observation;
}

function expectSteady(observation: AvSyncObservation): void {
  expect(observation).toEqual({ usable: true, verdict: 'steady' });
}

describe('avSyncDriftDetector', () => {
  describe('trip / no-trip surface', () => {
    it('AC-1: trips on a linear drift series and reports the constructed slope', () => {
      const stepPerSample = 2 * DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000); // 2x threshold
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / stepPerSample) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);

      const drift = expectDrift(runLinearSeries(n, stepPerSample));

      expect(drift.slopeMsPerSec).toBeCloseTo(stepPerSample / (SAMPLE_INTERVAL_MS / 1000), 1);
      // History spans ticks [1, n-1] (tick 0 is the first-sample baseline, never appended).
      // Assert the EXACT expected seconds, not just a floor -- `spanMs / 1000` dropping the
      // `/ 1000` (i.e. reporting milliseconds as seconds) survives a bare
      // `toBeGreaterThanOrEqual(MIN_WINDOW_MS / 1000)` check.
      const expectedSpanSec = ((n - 1 - 1) * SAMPLE_INTERVAL_MS) / 1000;
      expect(drift.spanSec).toBeCloseTo(expectedSpanSec, 5);
      expect(drift.samples).toBeGreaterThanOrEqual(MIN_SAMPLES);
    });

    it('AC-2: a constant non-zero offset (flat skew) does not trip', () => {
      const n = Math.max(MIN_SAMPLES + 1, Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2);
      // Both kinds' offset is 100ms (non-zero), difference is a constant 0 skew.
      expectSteady(runLinearSeries(n, 0));
    });

    it('AC-3: a bounded alternating-sign square-wave noise series stays steady', () => {
      // NOTE (correction): this test's original title claimed "(Theil-Sen median
      // survives; OLS would not)". That claim is FALSE for this fixture. A symmetric
      // alternating +-amplitude square wave has an OLS covariance of exactly zero --
      // the one noise shape OLS handles perfectly -- so this test would pass equally
      // against an OLS implementation, or even a `theilSenSlopeMsPerMs = () => 0`
      // stub. It genuinely locks "bounded symmetric noise stays steady", nothing more.
      // The robustness property is NOT falsifiable by observe() under the current
      // MAX_STEP_MS/MIN_EXCURSION_MS guards -- an asymmetric or clustered contamination
      // large enough to distinguish Theil-Sen from OLS also trips offset-step and wipes
      // history before a window can accumulate. See the direct
      // `theilSenSlopeMsPerMs` unit test below, which bypasses observe()'s guards
      // entirely to construct that contamination.
      const n = Math.max(MIN_SAMPLES + 1, Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2);
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      // Amplitude large enough to drag an OLS fit's endpoints, but the per-sample delta
      // (2x amplitude) stays comfortably under MAX_STEP_MS so no sample trips offset-step.
      const amplitude = MAX_STEP_MS / 4;
      for (let i = 0; i < n; i++) {
        const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
        const jitter = i % 2 === 0 ? amplitude : -amplitude;
        last = d.observe(pair(t, 100 + jitter, 100).values(), t);
      }
      expectSteady(last);
    });

    it('negative-direction drift: video-ahead-of-audio (a negative slope) trips drift exactly like a positive one', () => {
      // Every other fixture in this suite drifts positive (audio offset growing away from
      // video). skew = offset_audio - offset_video, so video's capture-time estimator
      // running ahead of audio's is a NEGATIVE slope and exactly as real a defect --
      // dropping Math.abs() from the drift check would survive every other test here.
      const stepPerSample = -2 * DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000); // negative, 2x threshold
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / Math.abs(stepPerSample)) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);

      const drift = expectDrift(runLinearSeries(n, stepPerSample));

      expect(drift.slopeMsPerSec).toBeLessThan(0);
      expect(drift.slopeMsPerSec).toBeCloseTo(stepPerSample / (SAMPLE_INTERVAL_MS / 1000), 1);
    });
  });

  describe('insufficient data (AC-4)', () => {
    it('AC-4a: fewer than MIN_SAMPLES history entries yields insufficient-samples even with an ample window', () => {
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      const interval = MIN_WINDOW_MS; // even one more sample would clear the window requirement
      const totalCalls = MIN_SAMPLES; // history.length === MIN_SAMPLES - 1
      for (let i = 0; i < totalCalls; i++) {
        const t = BASE_MS + i * interval;
        last = d.observe(pair(t, 100, 100).values(), t);
      }
      expect(last).toEqual({ usable: false, reason: 'insufficient-samples' });
    });

    it('AC-4b: MIN_SAMPLES history entries within too short a window yields insufficient-samples', () => {
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      const interval = Math.floor(MIN_WINDOW_MS / MIN_SAMPLES / 2); // count sufficient, span is not
      const totalCalls = MIN_SAMPLES + 1; // history.length === MIN_SAMPLES
      for (let i = 0; i < totalCalls; i++) {
        const t = BASE_MS + i * interval;
        last = d.observe(pair(t, 100, 100).values(), t);
      }
      expect(last).toEqual({ usable: false, reason: 'insufficient-samples' });
    });
  });

  describe('the false-positive guards', () => {
    it('AC-5: a frozen remoteTimestamp with an advancing local timestamp never trips (R1 regression lock)', () => {
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      const frozenRemote = BASE_MS - 100; // pinned SR-receipt instant for both kinds
      for (let i = 0; i < 20; i++) {
        const t = BASE_MS + i * SAMPLE_INTERVAL_MS; // only the local clock advances
        const audio = defaultKind('audio', t, 0, { remoteTimestampMs: frozenRemote });
        const video = defaultKind('video', t, 0, { remoteTimestampMs: frozenRemote });
        last = d.observe(frame(audio, video).values(), t);
      }
      expect(last).toEqual({ usable: false, reason: 'sr-not-advanced' });
    });

    it('AC-6: a single mid-window ~100ms step reports offset-step and does not trip', () => {
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      for (let i = 0; i < 3; i++) {
        const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
        last = d.observe(pair(t, 100, 100).values(), t);
      }
      const stepMs = 100;
      expect(stepMs).toBeGreaterThan(MAX_STEP_MS); // sanity: the step genuinely exceeds the guard
      const t = BASE_MS + 3 * SAMPLE_INTERVAL_MS;
      last = d.observe(pair(t, 100 + stepMs, 100).values(), t);

      expect(last).toEqual({ usable: false, reason: 'offset-step' });
    });

    it('constant boundary: MAX_STEP_MS - 1 is absorbed, MAX_STEP_MS clears history (offset-step)', () => {
      const absorbed = (() => {
        const d = createAvSyncDriftDetector();
        d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
        const t = BASE_MS + SAMPLE_INTERVAL_MS;
        return d.observe(pair(t, 100 + (MAX_STEP_MS - 1), 100).values(), t);
      })();
      // Absorbed (not classified as offset-step). Only 2 total calls exist, so the only other
      // possible outcome at this point is insufficient-samples.
      expect(absorbed).toEqual({ usable: false, reason: 'insufficient-samples' });

      const cleared = (() => {
        const d = createAvSyncDriftDetector();
        d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
        const t = BASE_MS + SAMPLE_INTERVAL_MS;
        return d.observe(pair(t, 100 + MAX_STEP_MS, 100).values(), t);
      })();
      expect(cleared).toEqual({ usable: false, reason: 'offset-step' });
    });
  });

  describe('units (AC-7)', () => {
    it('AC-7: jitterBufferDelay (seconds) surfaces as jbSkewMs in milliseconds', () => {
      const stepPerSample = 2 * DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000);
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / stepPerSample) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);

      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      // Per-interval: audio accrues 100ms of buffer delay per 10 emitted frames => 10 ms/frame;
      // video accrues 50ms per 10 frames => 5 ms/frame. Expected jbSkewMs = 10 - 5 = 5.
      for (let i = 0; i < n; i++) {
        const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
        const audio = defaultKind('audio', t, 100 + i * stepPerSample, {
          jbDelaySec: 0.05 + i * 0.1,
          jbCount: 1_000 + i * 10,
        });
        const video = defaultKind('video', t, 100, {
          jbDelaySec: 0.05 + i * 0.05,
          jbCount: 1_000 + i * 10,
        });
        last = d.observe(frame(audio, video).values(), t);
      }

      const drift = expectDrift(last);
      expect(drift.jbSkewMs).not.toBeNull();
      expect(drift.jbSkewMs as number).toBeCloseTo(5, 1);
    });
  });

  describe('reported scalars (AC-9)', () => {
    // NOTE (correction): the original AC-9 built its own payload object out of three
    // numbers pulled from `drift` and then asserted that JSON has no IPv4/UUID/long-token
    // shape. That is a tautology -- a payload constructed from finite numbers and null
    // cannot ever match an IP/UUID/40+-char-token regex, so the assertion cannot fail
    // regardless of what the module does. The real lock on the ACTUAL logged
    // `console.warn` argument lives in the sibling sampler suite. What this test can
    // genuinely pin is narrower: that the scalars `observe()` reports are finite numbers
    // (never NaN/Infinity), which is a real regression class (e.g. a division introduced
    // in a future slope/jbSkew computation).
    it('AC-9: the reported drift scalars (slope, span, samples, jbSkew) are finite numbers', () => {
      const stepPerSample = 2 * DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000);
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / stepPerSample) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);

      const drift = expectDrift(runLinearSeries(n, stepPerSample));

      expect(Number.isFinite(drift.slopeMsPerSec)).toBe(true);
      expect(Number.isFinite(drift.spanSec)).toBe(true);
      expect(Number.isFinite(drift.samples)).toBe(true);
      expect(drift.jbSkewMs === null || Number.isFinite(drift.jbSkewMs)).toBe(true);
    });
  });

  describe('every remaining unusable reason', () => {
    it('first-sample: the very first observation establishes a baseline only', () => {
      const d = createAvSyncDriftDetector();
      const result = d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'first-sample' });
    });

    it('missing-stats: no inbound-rtp entry of any kind is present', () => {
      const d = createAvSyncDriftDetector();
      const entries = new Map<string, unknown>([
        ['cp-1', { id: 'cp-1', type: 'candidate-pair', currentRoundTripTime: 0.02 }],
      ]);
      const result = d.observe(entries.values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'missing-stats' });
    });

    it('kind-incomplete: only one of {audio, video} is present (camera-off peer)', () => {
      const d = createAvSyncDriftDetector();
      const audio = defaultKind('audio', BASE_MS, 100);
      const result = d.observe(frame(audio, null).values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'kind-incomplete' });
    });

    it('unpaired: the remote-outbound-rtp back-reference disagrees with inbound-rtp.remoteId', () => {
      const d = createAvSyncDriftDetector();
      const audio = defaultKind('audio', BASE_MS, 100);
      const video = defaultKind('video', BASE_MS, 100, {
        outboundLocalIdOverride: 'IN-audio', // forward reference agrees; backward reference does not
      });
      const result = d.observe(frame(audio, video).values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'unpaired' });
    });

    it('field-absent: the remote-outbound-rtp report is missing for one kind', () => {
      const d = createAvSyncDriftDetector();
      const audio = defaultKind('audio', BASE_MS, 100);
      const video = defaultKind('video', BASE_MS, 100, { includeOutbound: false });
      const result = d.observe(frame(audio, video).values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'field-absent' });
    });

    it('field-absent: an inbound-rtp entry missing jitterBufferEmittedCount is refused (buildSnapshot null branch)', () => {
      const d = createAvSyncDriftDetector();
      const audio = defaultKind('audio', BASE_MS, 100);
      const video = defaultKind('video', BASE_MS, 100);
      const frameEntries = frame(audio, video);
      // Deleting the field entirely (not just leaving it at a default) exercises
      // buildSnapshot's own `asFiniteNumber(...) === null` guard for jbCount -- a
      // DIFFERENT code path than anyFieldAbsent's outbound-only check above, since
      // anyFieldAbsent never inspects inbound-rtp fields at all.
      const videoIn = frameEntries.get('IN-video') as Record<string, unknown>;
      delete videoIn.jitterBufferEmittedCount;
      const result = d.observe(frameEntries.values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'field-absent' });
    });

    it('field-absent: a remote-outbound-rtp entry with remoteTimestamp undefined is refused (anyFieldAbsent branch)', () => {
      const d = createAvSyncDriftDetector();
      const audio = defaultKind('audio', BASE_MS, 100);
      const video = defaultKind('video', BASE_MS, 100);
      const frameEntries = frame(audio, video);
      const videoOut = frameEntries.get('RO-video') as Record<string, unknown>;
      videoOut.remoteTimestamp = undefined;
      const result = d.observe(frameEntries.values(), BASE_MS);
      expect(result).toEqual({ usable: false, reason: 'field-absent' });
    });

    it('future-stats: a report timestamp beyond MAX_CLOCK_SKEW_MS ahead of observedAtMs is refused', () => {
      const d = createAvSyncDriftDetector();
      const t = BASE_MS;
      const observedAtMs = t - MAX_CLOCK_SKEW_MS - 1;
      const result = d.observe(pair(t, 100, 100).values(), observedAtMs);
      expect(result).toEqual({ usable: false, reason: 'future-stats' });
    });

    it('stale-stats: a report timestamp older than STALE_AFTER_MS is refused', () => {
      const d = createAvSyncDriftDetector();
      const t = BASE_MS;
      const observedAtMs = t + STALE_AFTER_MS + 1;
      const result = d.observe(pair(t, 100, 100).values(), observedAtMs);
      expect(result).toEqual({ usable: false, reason: 'stale-stats' });
    });

    it('out-of-order: observedAtMs does not advance past the previous call', () => {
      const d = createAvSyncDriftDetector();
      d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
      const result = d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS - 1);
      expect(result).toEqual({ usable: false, reason: 'out-of-order' });
    });

    it('clock-regression: a report timestamp moves backwards even though observedAtMs advances', () => {
      const d = createAvSyncDriftDetector();
      d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
      const regressedT = BASE_MS - 1_000;
      const observedAtMs = BASE_MS + SAMPLE_INTERVAL_MS;
      const result = d.observe(pair(regressedT, 100, 100).values(), observedAtMs);
      expect(result).toEqual({ usable: false, reason: 'clock-regression' });
    });

    it('stream-changed: the selected stream identity changes for one kind', () => {
      const d = createAvSyncDriftDetector();
      d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
      const t = BASE_MS + SAMPLE_INTERVAL_MS;
      const audio = defaultKind('audio', t, 100, { ssrc: 999 }); // ssrc changed from the baseline's 11
      const video = defaultKind('video', t, 100);
      const result = d.observe(frame(audio, video).values(), t);
      expect(result).toEqual({ usable: false, reason: 'stream-changed' });
    });

    it('counter-reset: a jitter-buffer counter decreases for one kind', () => {
      const d = createAvSyncDriftDetector();
      d.observe(pair(BASE_MS, 100, 100, { audioCount: 1_000 }).values(), BASE_MS);
      const t = BASE_MS + SAMPLE_INTERVAL_MS;
      const audio = defaultKind('audio', t, 100, { jbCount: 500 }); // decreased from 1,000
      const video = defaultKind('video', t, 100);
      const result = d.observe(frame(audio, video).values(), t);
      expect(result).toEqual({ usable: false, reason: 'counter-reset' });
    });
  });

  describe('constant boundaries through the export', () => {
    it('MIN_SAMPLES boundary: exactly MIN_SAMPLES history entries with a window at MIN_WINDOW_MS clears insufficient-samples', () => {
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      const interval = MIN_WINDOW_MS / (MIN_SAMPLES - 1);
      const totalCalls = MIN_SAMPLES + 1; // history.length === MIN_SAMPLES
      for (let i = 0; i < totalCalls; i++) {
        const t = BASE_MS + i * interval;
        last = d.observe(pair(t, 100, 100).values(), t);
      }
      expectSteady(last);
    });

    it('bounds history to the newest HISTORY_LIMIT samples', () => {
      const stepPerSample = 2 * DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000);
      const extra = 5;
      const totalCalls = HISTORY_LIMIT + extra + 1; // +1 for the baseline first-sample call
      const drift = expectDrift(runLinearSeries(totalCalls, stepPerSample));
      expect(drift.samples).toBe(HISTORY_LIMIT);
    });

    it('slope threshold: just under DRIFT_SLOPE_MS_PER_SEC does not trip', () => {
      const stepPerSample = (DRIFT_SLOPE_MS_PER_SEC - 0.1) * (SAMPLE_INTERVAL_MS / 1000);
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / stepPerSample) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);
      expectSteady(runLinearSeries(n, stepPerSample));
    });

    it('slope threshold: exactly DRIFT_SLOPE_MS_PER_SEC trips', () => {
      const stepPerSample = DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000);
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / stepPerSample) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);
      const drift = expectDrift(runLinearSeries(n, stepPerSample));
      expect(drift.slopeMsPerSec).toBeCloseTo(DRIFT_SLOPE_MS_PER_SEC, 5);
    });
  });

  describe('x-axis provenance', () => {
    it('history.x is observedAtMs, not the report timestamp -- a growing clock offset detects a wrong-field regression', () => {
      // Every other fixture in this suite passes the SAME value as both the report
      // timestamp (via pair(t, ...)) and observedAtMs (d.observe(..., t)), so
      // `x: observedAtMs` -> `x: current.audio.timestamp` is invisible everywhere else:
      // a CONSTANT offset between the two clocks would also be invisible here, because
      // it cancels out of every difference. Growing the offset LINEARLY per tick makes
      // the two candidate x-axes diverge in spanSec, which is what this test pins.
      const d = createAvSyncDriftDetector();
      let last!: AvSyncObservation;
      const clockOffsetPerTick = 37;
      const stepPerSample = 2 * DRIFT_SLOPE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000);
      const minNForWindow = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2;
      const minNForExcursion = Math.ceil(MIN_EXCURSION_MS / stepPerSample) + 2;
      const n = Math.max(MIN_SAMPLES + 1, minNForWindow, minNForExcursion);

      for (let i = 0; i < n; i++) {
        const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
        const observedAtMs = t + i * clockOffsetPerTick;
        last = d.observe(pair(t, 100 + i * stepPerSample, 100).values(), observedAtMs);
      }

      const drift = expectDrift(last);

      // History spans ticks [1, n-1]. Correct: x = observedAtMs = t + i*clockOffsetPerTick.
      const firstObservedAtMs = BASE_MS + 1 * SAMPLE_INTERVAL_MS + 1 * clockOffsetPerTick;
      const lastObservedAtMs =
        BASE_MS + (n - 1) * SAMPLE_INTERVAL_MS + (n - 1) * clockOffsetPerTick;
      const expectedSpanSec = (lastObservedAtMs - firstObservedAtMs) / 1000;
      expect(drift.spanSec).toBeCloseTo(expectedSpanSec, 5);

      // The mutant (`x: current.audio.timestamp`) would instead span only the report
      // timestamps, which never include clockOffsetPerTick -- a strictly smaller span.
      const mutantSpanSec = ((n - 1 - 1) * SAMPLE_INTERVAL_MS) / 1000;
      expect(mutantSpanSec).toBeLessThan(expectedSpanSec); // sanity: the two really do diverge
      expect(drift.spanSec).not.toBeCloseTo(mutantSpanSec, 5);
    });
  });

  describe('reset()', () => {
    it('clears history so the next observation is first-sample again', () => {
      const d = createAvSyncDriftDetector();
      d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
      const t = BASE_MS + SAMPLE_INTERVAL_MS;
      const midway = d.observe(pair(t, 100, 100).values(), t);
      expect(midway).not.toEqual({ usable: false, reason: 'first-sample' }); // history was building

      d.reset();

      const afterReset = d.observe(pair(BASE_MS, 100, 100).values(), BASE_MS);
      expect(afterReset).toEqual({ usable: false, reason: 'first-sample' });
    });
  });
});

/** Builds a frame where BOTH kinds carry two distinct remote-sender entries -- exactly
 *  what a 3+-party call yields for one kind's recv transport. Distinct ids and ssrc per
 *  peer so `classifyEntries`' per-kind counters (not just its last-write-wins maps) see
 *  more than one entry. */
function ambiguousFrame(timestampMs: number): Map<string, unknown> {
  const entries: Array<[string, unknown]> = [];
  for (const kind of ['audio', 'video'] as const) {
    for (let peer = 0; peer < 2; peer++) {
      const inId = `IN-${kind}-${peer}`;
      const roId = `RO-${kind}-${peer}`;
      entries.push([
        inId,
        {
          id: inId,
          type: 'inbound-rtp',
          kind,
          ssrc: (kind === 'audio' ? 100 : 200) + peer,
          trackIdentifier: `track-${kind}-${peer}`,
          remoteId: roId,
          timestamp: timestampMs,
          jitterBufferDelay: 0.05,
          jitterBufferEmittedCount: 1_000,
        },
      ]);
      entries.push([
        roId,
        {
          id: roId,
          type: 'remote-outbound-rtp',
          kind,
          localId: inId,
          timestamp: timestampMs,
          remoteTimestamp: timestampMs - 100,
        },
      ]);
    }
  }
  return new Map(entries);
}

describe('multi-peer refusal: ambiguous-streams (#2941 new guard)', () => {
  it('two remote senders of each kind refuse as ambiguous-streams and do NOT clear or fall back to insufficient-samples on resume', () => {
    const d = createAvSyncDriftDetector();
    let last!: AvSyncObservation;

    // Build a healthy single-peer window first.
    const n = Math.max(MIN_SAMPLES + 1, Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2);
    for (let i = 0; i < n; i++) {
      const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      last = d.observe(pair(t, 100, 100).values(), t);
    }
    expectSteady(last);

    // One ambiguous (multi-peer) tick.
    const ambiguousT = BASE_MS + n * SAMPLE_INTERVAL_MS;
    const ambiguousResult = d.observe(ambiguousFrame(ambiguousT).values(), ambiguousT);
    expect(ambiguousResult).toEqual({ usable: false, reason: 'ambiguous-streams' });

    // Resume single-peer. Reaching `steady` immediately (rather than
    // `insufficient-samples`) IS the proof that the ambiguous tick did not clear
    // history and did not update `previousState`.
    const resumeT = ambiguousT + SAMPLE_INTERVAL_MS;
    const resumeResult = d.observe(pair(resumeT, 100, 100).values(), resumeT);
    expectSteady(resumeResult);
  });
});

describe('sustained drift steeper than the step ceiling is measured, not discarded (CodeRabbit)', () => {
  // MEASURED BEFORE THE FIX: a sustained 10 ms/s ramp produced `offset-step` on all 17 ticks
  // and never once reached MIN_SAMPLES, so the detector stayed silent while accumulating
  // 600 ms of skew per minute. The severity response was inverted -- the worse the drift, the
  // more certainly it went unreported -- because `isOffsetStep` fired every tick and each
  // firing cleared the window before it could close.
  const ticks = Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 6;

  const runRate = (msPerSec: number) => {
    const d = createAvSyncDriftDetector();
    const perSample = msPerSec * (SAMPLE_INTERVAL_MS / 1000);
    const seen: AvSyncObservation[] = [];
    for (let i = 0; i < ticks; i++) {
      const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      seen.push(d.observe(pair(t, 100 + i * perSample, 100).values(), t));
    }
    return seen;
  };

  it.each([
    ['at the ceiling', MAX_STEP_RATE_MS_PER_SEC],
    ['well above it', MAX_STEP_RATE_MS_PER_SEC * 2],
  ])('a sustained ramp %s still reports drift', (_label, rate) => {
    const seen = runRate(rate);
    const steps = seen.filter((o) => !o.usable && o.reason === 'offset-step');
    const drifts = seen.filter((o) => o.usable && o.verdict === 'drift');

    // Exactly ONE step: the first crossing is indistinguishable from a genuine
    // discontinuity and is still treated as one. The repeats are the ramp.
    expect(steps).toHaveLength(1);
    expect(drifts.length).toBeGreaterThan(0);
    const last = drifts[drifts.length - 1];
    if (!last.usable || last.verdict !== 'drift') throw new Error('unreachable');
    expect(Math.abs(last.slopeMsPerSec)).toBeGreaterThanOrEqual(MAX_STEP_RATE_MS_PER_SEC);
  });

  it('CONTROL: a ONE-OFF step is still a discontinuity that clears the window', () => {
    // Without this, the fix above could not be distinguished from deleting the guard.
    const d = createAvSyncDriftDetector();
    let last!: AvSyncObservation;
    const n = Math.max(MIN_SAMPLES + 1, Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2);
    for (let i = 0; i < n; i++) {
      const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      last = d.observe(pair(t, 100, 100).values(), t);
    }
    expectSteady(last);

    // One isolated jump far beyond the ceiling, then the offset HOLDS at its new value.
    const stepT = BASE_MS + n * SAMPLE_INTERVAL_MS;
    const jumped = 100 + MAX_STEP_RATE_MS_PER_SEC * (SAMPLE_INTERVAL_MS / 1000) * 10;
    expect(d.observe(pair(stepT, jumped, 100).values(), stepT)).toEqual({
      usable: false,
      reason: 'offset-step',
    });

    // History was cleared, so the very next sample cannot already be a full window.
    const afterT = stepT + SAMPLE_INTERVAL_MS;
    const after = d.observe(pair(afterT, jumped, 100).values(), afterT);
    expect(after).toEqual({ usable: false, reason: 'insufficient-samples' });
  });
});

describe("mediasoup's probator consumer is not a second publisher (#2941 AC-11 regression)", () => {
  // FOUND ON A LIVE CALL, not by review. mediasoup-client creates a bandwidth
  // `probator` consumer in EVERY call: an `inbound-rtp` of kind video, ssrc 1234,
  // `mid: 'probator'`, with NO `remoteId` and no `remote-outbound-rtp` partner.
  // Counting raw inbound entries per kind made `ambiguous-streams` fire on an
  // ORDINARY TWO-PARTY CALL -- 22/22 ticks refused, the feature inert in
  // production, with all 80 unit tests green. That is the #3117 failure this
  // PR's own register row exists to record, so it gets a test.
  const probator = (timestampMs: number) => ({
    type: 'inbound-rtp',
    kind: 'video',
    id: 'v:IT01V1234',
    ssrc: 1234,
    mid: 'probator',
    timestamp: timestampMs,
    packetsReceived: 360,
    jitterBufferDelay: 0.05,
    jitterBufferEmittedCount: 1_000,
    // no remoteId -- this is the whole point
  });

  it('an inbound-rtp with no remoteId does not make a two-party call ambiguous', () => {
    const d = createAvSyncDriftDetector();
    let last!: AvSyncObservation;
    const n = Math.max(MIN_SAMPLES + 1, Math.ceil(MIN_WINDOW_MS / SAMPLE_INTERVAL_MS) + 2);
    for (let i = 0; i < n; i++) {
      const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      const f = pair(t, 100, 100);
      f.set('v:IT01V1234', probator(t));
      last = d.observe(f.values(), t);
    }
    // Before the fix this was { usable: false, reason: 'ambiguous-streams' } on
    // every single tick.
    expectSteady(last);
  });

  it('CONTROL: a second SR-LINKED sender of a kind still refuses as ambiguous-streams', () => {
    // Without this, the test above would also pass against a build that simply
    // deleted the guard. The discriminator is SR linkage, not entry count.
    const d = createAvSyncDriftDetector();
    const t = BASE_MS;
    const f = pair(t, 100, 100);
    f.set('v:IT01V999', {
      type: 'inbound-rtp',
      kind: 'video',
      id: 'v:IT01V999',
      ssrc: 999,
      remoteId: 'v:ROV999', // <- SR-linked: a genuine second publisher
      timestamp: t,
      jitterBufferDelay: 0.05,
      jitterBufferEmittedCount: 1_000,
    });
    f.set('v:ROV999', {
      type: 'remote-outbound-rtp',
      kind: 'video',
      id: 'v:ROV999',
      localId: 'v:IT01V999',
      timestamp: t,
      remoteTimestamp: t - 100,
    });
    expect(d.observe(f.values(), t)).toEqual({ usable: false, reason: 'ambiguous-streams' });
  });
});

/** Builds a frame for the stall-then-resume scenario: one kind's WHOLE remote-outbound-rtp
 *  entry (both `timestamp` and `remoteTimestamp`) is pinned at a frozen snapshot -- modeling
 *  "no new RTCP SR arrived" -- while that kind's inbound-rtp `timestamp` (produced locally on
 *  every getStats() call regardless of SR arrival) keeps advancing with the real clock. The
 *  other kind behaves normally throughout. */
function srStallFrame(
  t: number,
  frozenKind: 'audio' | 'video',
  frozenSnapshot: { outTimestamp: number; remoteTimestamp: number },
  offsets: { audio: number; video: number }
): Map<string, unknown> {
  const entries: Array<[string, unknown]> = [];
  for (const kind of ['audio', 'video'] as const) {
    const inId = `IN-${kind}`;
    const roId = `RO-${kind}`;
    const isFrozen = kind === frozenKind;
    const outTimestamp = isFrozen ? frozenSnapshot.outTimestamp : t;
    const remoteTimestamp = isFrozen ? frozenSnapshot.remoteTimestamp : t - offsets[kind];
    entries.push([
      inId,
      {
        id: inId,
        type: 'inbound-rtp',
        kind,
        ssrc: kind === 'audio' ? 11 : 22,
        trackIdentifier: `track-${kind}`,
        remoteId: roId,
        timestamp: t, // inbound-rtp's own timestamp always advances, frozen kind included
        jitterBufferDelay: 0.05,
        jitterBufferEmittedCount: 1_000,
      },
    ]);
    entries.push([
      roId,
      {
        id: roId,
        type: 'remote-outbound-rtp',
        kind,
        localId: inId,
        timestamp: outTimestamp,
        remoteTimestamp,
      },
    ]);
  }
  return new Map(entries);
}

describe('stall-then-resume: offset-step is a RATE, not an absolute delta (#2941 fix)', () => {
  it('a step of exactly MAX_STEP_MS across a long SR-stall gap does NOT trip, but the same gap with a proportionally larger step DOES (control)', () => {
    // Phase A: 8 healthy ticks establish previousState + history.
    const d = createAvSyncDriftDetector();
    let last!: AvSyncObservation;
    let t = BASE_MS;
    for (let i = 0; i < 8; i++) {
      t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      last = d.observe(pair(t, 100, 100).values(), t);
    }
    const T0 = t; // last successful tick's observedAtMs -- previousState is pinned here
    const frozen = { outTimestamp: T0, remoteTimestamp: T0 - 100 };

    // Phase B: 6 ticks where video's remote-outbound-rtp entry is frozen at T0's snapshot.
    // previousState does NOT advance on sr-not-advanced, so it stays pinned at T0 through
    // all 6 -- which is what lets the resume tick's elapsed gap be exactly 30s below.
    const freezeOffsetsMs = [5_000, 10_000, 15_000, 20_000, 25_000, 29_000];
    for (const dt of freezeOffsetsMs) {
      const tickT = T0 + dt;
      const result = d.observe(
        srStallFrame(tickT, 'video', frozen, { audio: 100, video: 100 }).values(),
        tickT
      );
      expect(result).toEqual({ usable: false, reason: 'sr-not-advanced' });
    }

    // Resume: exactly a 30s gap since T0 (the sanity table's documented "30s+30ms -> 1
    // ms/s NO TRIP" row). skew(T0) was 0 (both offsets 100); moving video's offset by
    // MAX_STEP_MS moves skew by exactly MAX_STEP_MS across that 30s gap = 1 ms/s, derived
    // from MAX_STEP_RATE_MS_PER_SEC rather than hardcoded, and well under the threshold.
    const gapSec = 30;
    const gapMs = gapSec * 1_000;
    const noTripStepMs = MAX_STEP_MS;
    const noTripRate = noTripStepMs / gapSec;
    expect(noTripRate).toBeLessThan(MAX_STEP_RATE_MS_PER_SEC); // sanity: genuinely a no-trip construction

    const resumeT = T0 + gapMs;
    const resumeResult = d.observe(pair(resumeT, 100, 100 - noTripStepMs).values(), resumeT);
    expect(resumeResult).not.toEqual({ usable: false, reason: 'offset-step' });

    // CONTROL: same 30s gap, a step proportionally larger than the rate threshold. Without
    // this control the prior assertion alone cannot distinguish "guard correctly computes
    // a rate" from "guard is simply disabled" -- both would let the small step through.
    const controlStepMs = MAX_STEP_RATE_MS_PER_SEC * gapSec * 1.5; // well above threshold rate
    const controlRate = controlStepMs / gapSec;
    expect(controlRate).toBeGreaterThan(MAX_STEP_RATE_MS_PER_SEC); // sanity: genuinely a trip construction

    const controlD = createAvSyncDriftDetector();
    let controlLast!: AvSyncObservation;
    for (let i = 0; i < 8; i++) {
      const tickT = BASE_MS + i * SAMPLE_INTERVAL_MS;
      controlLast = controlD.observe(pair(tickT, 100, 100).values(), tickT);
    }
    for (const dt of freezeOffsetsMs) {
      const tickT = T0 + dt;
      controlLast = controlD.observe(
        srStallFrame(tickT, 'video', frozen, { audio: 100, video: 100 }).values(),
        tickT
      );
    }
    const controlResumeT = T0 + gapMs;
    const controlResult = controlD.observe(
      pair(controlResumeT, 100, 100 - controlStepMs).values(),
      controlResumeT
    );
    expect(controlResult).toEqual({ usable: false, reason: 'offset-step' });
  });
});

describe('single-kind SR freeze (highest-value single test, #2941 review)', () => {
  it('freezing only VIDEO remoteTimestamp while audio advances trips sr-not-advanced every tick -- proving it is ordered BEFORE offset-step', () => {
    // AC-5 above freezes BOTH kinds' remoteTimestamp, which produces a CONSTANT skew of 0
    // (both offsets grow at the same rate and cancel) -- it proves the guard fires but NOT
    // the module's own claim that a frozen SR manufactures a perfect +1 ms/ms line, which
    // is a statement about ONE kind freezing. Here only video's remoteTimestamp is pinned
    // while its own remote-outbound-rtp `timestamp` keeps advancing with the real clock, so
    // video.offsetMs = outTimestamp(advancing) - remoteTimestamp(frozen) grows by
    // SAMPLE_INTERVAL_MS every tick. Audio stays flat at offset 100, so skew swings by a
    // huge amount each tick -- an amount that would trip offset-step (and clear history) if
    // reached. Every tick reporting sr-not-advanced instead proves guard #12 is evaluated
    // BEFORE guard #13.
    const d = createAvSyncDriftDetector();
    const frozenVideoRemote = BASE_MS - 100; // video's remoteTimestamp pinned from tick 0
    const n = 8;
    for (let i = 0; i < n; i++) {
      const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      const audio = defaultKind('audio', t, 100);
      const video = defaultKind('video', t, 100, { remoteTimestampMs: frozenVideoRemote });
      const result = d.observe(frame(audio, video).values(), t);
      if (i === 0) {
        expect(result).toEqual({ usable: false, reason: 'first-sample' });
      } else {
        expect(result).toEqual({ usable: false, reason: 'sr-not-advanced' });
      }
    }
  });
});

describe('theilSenSlopeMsPerMs (exported for direct testing, #2941 item 6)', () => {
  it('pins the median-of-slopes result against 30% clustered outliers, where endpoint-difference and OLS both diverge', () => {
    // Replacing Theil-Sen with endpoint-difference or plain OLS survives observe()'s
    // ENTIRE suite, because every drift fixture in this file is exactly linear -- all
    // three estimators agree on a clean line. Freed from observe()'s MAX_STEP_MS /
    // MIN_EXCURSION_MS guards (which would refuse or wipe history before this contamination
    // could ever accumulate through observe()), this series is directly constructible: 7
    // points lie exactly on y = x (slope 1); the last 3 (30%) are a clustered outlier group
    // pinned to y = 100 regardless of x.
    const points = [
      { x: 0, y: 0, jbSkewMs: null },
      { x: 1, y: 1, jbSkewMs: null },
      { x: 2, y: 2, jbSkewMs: null },
      { x: 3, y: 3, jbSkewMs: null },
      { x: 4, y: 4, jbSkewMs: null },
      { x: 5, y: 5, jbSkewMs: null },
      { x: 6, y: 6, jbSkewMs: null },
      { x: 7, y: 100, jbSkewMs: null },
      { x: 8, y: 100, jbSkewMs: null },
      { x: 9, y: 100, jbSkewMs: null },
    ];

    // Of the C(10,2)=45 pairwise slopes: 21 clean-clean pairs are exactly 1; 3
    // outlier-outlier pairs are exactly 0 (equal y); the remaining 21 clean-outlier pairs
    // all land between ~11.1 and 94 (strictly above 1). Sorted ascending that is
    // [0,0,0, 1x21, then 21 large values] -- the median (index 22 of 45) falls inside the
    // block of 21 values equal to 1, so Theil-Sen recovers the clean-segment slope EXACTLY
    // despite 30% of the points (53% of the pairs) being contaminated.
    const slopeMsPerMs = theilSenSlopeMsPerMs(points);
    expect(slopeMsPerMs).toBe(1);

    // Prove the alternates really do diverge from the pinned value, computed (not
    // hand-derived) so an implementation swapped to either one would fail this test.
    const endpointDifference =
      (points[points.length - 1].y - points[0].y) / (points[points.length - 1].x - points[0].x);
    expect(Math.abs(endpointDifference - slopeMsPerMs)).toBeGreaterThan(5);

    const n = points.length;
    const meanX = points.reduce((a, p) => a + p.x, 0) / n;
    const meanY = points.reduce((a, p) => a + p.y, 0) / n;
    const sxy = points.reduce((a, p) => a + (p.x - meanX) * (p.y - meanY), 0);
    const sxx = points.reduce((a, p) => a + (p.x - meanX) ** 2, 0);
    const olsSlope = sxy / sxx;
    expect(Math.abs(olsSlope - slopeMsPerMs)).toBeGreaterThan(5);
  });
});

describe('startup regression lock: field-absent is the only reason before the first RTCP SR (#2941 R1)', () => {
  it('a realistic startup streak (both kinds negotiated, neither remote-outbound-rtp built yet) reports field-absent every time, never a different reason', () => {
    const d = createAvSyncDriftDetector();
    // libwebrtc cannot construct remote-outbound-rtp before the first RTCP SR
    // arrives, so a healthy call's opening window has inbound-rtp for both
    // kinds but remote-outbound-rtp for neither. Import the constant rather
    // than hardcoding a streak length -- this proves the REASON the sampler's
    // streak counter (voiceService.ts) depends on is what the module actually
    // produces, for at least as many consecutive ticks as the sampler waits.
    for (let i = 0; i < FIELD_ABSENCE_STREAK_TO_REPORT; i++) {
      const t = BASE_MS + i * SAMPLE_INTERVAL_MS;
      const audio = defaultKind('audio', t, 100, { includeOutbound: false });
      const video = defaultKind('video', t, 100, { includeOutbound: false });
      const result = d.observe(frame(audio, video).values(), t);
      expect(result).toEqual({ usable: false, reason: 'field-absent' });
    }
  });
});

describe('mergeAvSyncStats — AC-17 id namespacing', () => {
  const T = BASE_MS;

  /** Two independently-numbered reports that reuse the SAME literal ids, exactly
   *  as two separate RTCPeerConnections (#291) routinely do -- Chromium derives
   *  several stats ids from a per-PC index counter starting near zero. */
  function collidingReports(): {
    audioReport: Map<string, unknown>;
    videoReport: Map<string, unknown>;
  } {
    const audioIn = {
      id: 'IT01',
      type: 'inbound-rtp',
      kind: 'audio',
      remoteId: 'RO01',
      timestamp: T,
      ssrc: 11,
      trackIdentifier: 'track-audio',
      jitterBufferDelay: 0.05,
      jitterBufferEmittedCount: 1_000,
    };
    const audioOut = {
      id: 'RO01',
      type: 'remote-outbound-rtp',
      kind: 'audio',
      localId: 'IT01',
      timestamp: T,
      remoteTimestamp: T - 100, // audio offset 100ms
    };
    // SAME literal ids as audio's, on a DIFFERENT kind/report.
    const videoIn = {
      id: 'IT01',
      type: 'inbound-rtp',
      kind: 'video',
      remoteId: 'RO01',
      timestamp: T,
      ssrc: 22,
      trackIdentifier: 'track-video',
      jitterBufferDelay: 0.05,
      jitterBufferEmittedCount: 1_000,
    };
    const videoOut = {
      id: 'RO01',
      type: 'remote-outbound-rtp',
      kind: 'video',
      localId: 'IT01',
      timestamp: T,
      remoteTimestamp: T - 300, // video offset 300ms -- clearly different from audio's
    };
    return {
      audioReport: new Map<string, unknown>([
        ['IT01', audioIn],
        ['RO01', audioOut],
      ]),
      videoReport: new Map<string, unknown>([
        ['IT01', videoIn],
        ['RO01', videoOut],
      ]),
    };
  }

  it('VACUITY CONTROL: the naive way to merge two RTCStatsReports (spreading them together as Maps, since RTCStatsReport IS Map-shaped) silently drops one side on id collision and misclassifies', () => {
    const { audioReport, videoReport } = collidingReports();
    // The obvious, wrong first attempt at "merging two reports": both are
    // Map-shaped, so a caller might reach for `new Map([...a, ...b])`. Because
    // every id collides, video's entries (inserted second) evict audio's from
    // the SAME id-keyed slots -- audio's inbound-rtp and remote-outbound-rtp
    // objects are gone from the iterable entirely, not merely misread.
    const naivelyMerged = new Map<string, unknown>([...audioReport, ...videoReport]);
    expect(naivelyMerged.size).toBe(2); // proof of the data loss: 4 distinct entries went in

    const d = createAvSyncDriftDetector();
    const result = d.observe(naivelyMerged.values(), T);
    // Real, valid pairing data existed for BOTH kinds a moment ago; the
    // id-collision data loss reports a kind as entirely absent instead.
    expect(result).toEqual({ usable: false, reason: 'kind-incomplete' });
  });

  it('AC-17: mergeAvSyncStats avoids the collision entirely -- no data loss, correct classification', () => {
    const { audioReport, videoReport } = collidingReports();
    const merged = mergeAvSyncStats(audioReport.values(), videoReport.values());
    expect(merged).toHaveLength(4); // nothing lost, unlike the naive Map merge above

    const d = createAvSyncDriftDetector();
    const result = d.observe(merged, T);
    // Reaching `first-sample` (rather than kind-incomplete/field-absent) on a
    // fresh detector IS the proof both kinds' entries survived and paired.
    expect(result).toEqual({ usable: false, reason: 'first-sample' });
  });

  it('AC-17: prefixes id, remoteId, and localId with the kind-scoped prefix', () => {
    const entry = { id: 'X1', remoteId: 'X2', localId: 'X3', type: 'inbound-rtp', kind: 'audio' };
    const merged = mergeAvSyncStats([entry], []);
    expect(merged).toEqual([
      { id: 'a:X1', remoteId: 'a:X2', localId: 'a:X3', type: 'inbound-rtp', kind: 'audio' },
    ]);
  });

  it('AC-17: uses the "v:" prefix for the second (video) argument', () => {
    const entry = {
      id: 'Y1',
      remoteId: 'Y2',
      localId: 'Y3',
      type: 'remote-outbound-rtp',
      kind: 'video',
    };
    const merged = mergeAvSyncStats([], [entry]);
    expect(merged).toEqual([
      { id: 'v:Y1', remoteId: 'v:Y2', localId: 'v:Y3', type: 'remote-outbound-rtp', kind: 'video' },
    ]);
  });

  it('AC-17: leaves non-string id-bearing fields untouched', () => {
    const entry = { id: 42, remoteId: null, localId: undefined, kind: 'audio' };
    const merged = mergeAvSyncStats([entry], []);
    expect(merged).toEqual([{ id: 42, remoteId: null, localId: undefined, kind: 'audio' }]);
  });

  it('AC-17: passes non-object entries through unchanged', () => {
    const merged = mergeAvSyncStats([null, 'not-an-object', 42, undefined], []);
    expect(merged).toEqual([null, 'not-an-object', 42, undefined]);
  });

  it('AC-17: namespaces every entry regardless of its `type`, so a non-RTP entry is not silently skipped', () => {
    const codecEntry = { id: 'C1', type: 'codec', mimeType: 'audio/opus' };
    const transportEntry = { id: 'T1', type: 'transport', selectedCandidatePairId: 'cp-1' };
    const merged = mergeAvSyncStats([codecEntry], [transportEntry]);
    expect(merged).toEqual([
      { id: 'a:C1', type: 'codec', mimeType: 'audio/opus' },
      { id: 'v:T1', type: 'transport', selectedCandidatePairId: 'cp-1' },
    ]);
  });
});
