/**
 * A/V sync-drift decision module (#2941).
 *
 * mediasoup 3.25.0 (PR #2917) changed how the worker derives RTCP Sender
 * Reports for `remote-outbound-rtp` stats -- from RTP-arrival-time derivation
 * to capture-instant estimation (`RemoteClockOffsetEstimator` /
 * `RemoteCaptureTimeEstimator`) -- and changed `abs-capture-time` rewriting in
 * `SimulcastProducerStreamManager`. Nothing in this repository measures
 * whether that rewrite preserves A/V lip-sync. Engine baseline for this
 * module: mediasoup 3.26.0, Electron 44.1.1 / Chromium 152.
 *
 * The estimator differences two offsets rather than reading either alone:
 * `offset_k = remote-outbound-rtp.timestamp - remote-outbound-rtp.remoteTimestamp`
 * per kind k in {audio, video}. Neither offset is meaningful by itself -- it
 * carries the receiver/sender clock delta plus one-way delay plus per-stream
 * estimator noise -- but audio and video share one sender clock basis and one
 * BUNDLE'd transport path, so `skew = offset_audio - offset_video` cancels the
 * shared clock delta and leaves only the divergence between the two streams'
 * capture-time estimators. A sustained slope in `skew` is therefore evidence
 * of a timestamping defect, not a network property.
 *
 * Pure module: no `Date.now()`, no `performance.now()`, no timer, no I/O. The
 * caller supplies the clock reading (`observedAtMs`) so tests stay
 * deterministic. Mirrors `decoderBudgetSampler.ts`'s `unknown`-typed local
 * interface + runtime narrowing and its `{usable:false;reason} |
 * {usable:true;...}` result shape.
 *
 * The design spec lists 17 not-usable reasons (inherited from
 * `decoderBudgetSampler.ts` for vocabulary consistency, plus additions for
 * this problem). Three are unreachable from this module's signature and are
 * NOT implemented -- do not "restore" them from the spec:
 *   - `paused`: `observe(entries, observedAtMs)` carries no pause input at all.
 *   - `no-progress`: decoderBudgetSampler's version catches a stalled raw
 *     counter between samples. This module's per-sample skew is a direct
 *     subtraction, not an interval delta of an accumulating counter, so there
 *     is no case distinct from `sr-not-advanced` (guard #12), which already
 *     owns "remoteTimestamp did not advance".
 *   - `invalid-derived-metrics`: the only derived arithmetic here is
 *     subtraction (skew, jbSkewMs) and a median of pairwise slopes over
 *     already-finite, already-validated inputs -- `field-absent` rejects
 *     non-finite raw fields before any arithmetic runs, so no combination of
 *     finite doubles fed through subtraction and bounded-count division is
 *     reachable to non-finite here.
 *
 * `ambiguous-streams` is an 18th reason, added beyond the design spec's 17
 * after multi-peer testing surfaced a real defect: there is ONE recv
 * transport per kind for the whole call, so a 3+-party call yields multiple
 * `inbound-rtp` (and/or `remote-outbound-rtp`) entries of one kind, and
 * `classifyEntries`' Map-based last-write-wins would otherwise silently pick
 * two different peers' entries for the two directions -- possibly from
 * DIFFERENT publishers, voiding the shared-sender-clock-basis premise this
 * whole module rests on. See guard #3 below.
 */

/** Sampling cadence. Must exceed the RTCP SR interval (~1-5 s) or most samples hit `sr-not-advanced`. */
export const SAMPLE_INTERVAL_MS = 5_000;
/** Theil-Sen over 15 pairs; below this the median is not meaningfully robust. */
export const MIN_SAMPLES = 6;
/** Mirrors decoderBudgetSampler's 20; at 5 s this is a 2-minute window. */
export const HISTORY_LIMIT = 24;
/** One minute of baseline; at 1 ms/s that is a 60 ms rise, well above per-sample jitter. */
export const MIN_WINDOW_MS = 60_000;
/** 1 ms/s = 1000 ppm. Consumer crystals run +/-20-100 ppm and both streams share one sender
 *  clock basis, so the nominal differential is 0 ppm. 1000 ppm is a logic defect, not tolerance. */
export const DRIFT_SLOPE_MS_PER_SEC = 1;
/** Consistent with the slope threshold; independently binding only on longer windows. */
export const MIN_EXCURSION_MS = 60;
/** A server-commanded spatial-layer switch steps the capture-time basis. This is the RATE
 *  NUMERATOR at exactly one `SAMPLE_INTERVAL_MS` gap, not an absolute cap on the step itself --
 *  see `MAX_STEP_RATE_MS_PER_SEC` below, which normalizes it to ms/s so the same headroom holds
 *  at any inter-sample gap, including the arbitrarily long gap after an SR stall. Bounded BELOW
 *  by the signal (<= 5 ms per sample at threshold = 6x headroom); NOT bounded above by
 *  measurement -- the loopback harness cannot produce a real-network per-sample delta
 *  distribution. Revisit if any field `drift-detected` report shows a pre-trip sample sequence
 *  with steps in the 10-30 ms band. */
export const MAX_STEP_MS = 30;
/** `MAX_STEP_MS` expressed as a rate (ms/s), so the offset-step guard trips on the same 6x
 *  headroom regardless of how long the gap between samples was. An ABSOLUTE step threshold
 *  reads a genuine 1 ms/s drift across a long post-SR-stall gap as one large step and wipes the
 *  window -- the bug this constant exists to fix. Sanity table (gap, step -> rate, verdict):
 *  5s+30ms -> 6 ms/s TRIPS; 5s+5ms -> 1 ms/s no trip; 30s+30ms -> 1 ms/s NO TRIP (the case the
 *  absolute form got wrong); 30s+200ms -> 6.7 ms/s TRIPS. */
export const MAX_STEP_RATE_MS_PER_SEC = MAX_STEP_MS / (SAMPLE_INTERVAL_MS / 1000);
/** Verbatim from decoderBudgetSampler.ts:2. */
export const STALE_AFTER_MS = 10_000;
/** A report timestamp this far ahead of observedAtMs is `future-stats`. */
/**
 * Consecutive field-absence observations required before `[avsync] fields-unavailable`
 * is emitted.
 *
 * libwebrtc CANNOT construct a `remote-outbound-rtp` object before the first RTCP SR
 * arrives ("Cannot create RTCRemoteOutboundRtpStreamStats when the RTCP SR arrival
 * timestamp is not available"), so EVERY healthy call begins with at least one
 * `field-absent` observation. Emitting on the first one spends a once-per-session latch
 * during normal startup and leaves a genuine later regression silent -- the dead-guard
 * failure this detector exists to prevent, arriving through the front door.
 *
 * At `SAMPLE_INTERVAL_MS` (5 s), which already exceeds the RTCP SR interval by design,
 * this spans 30 s of continuous absence. A working call produces an SR many times over
 * in that window, so a streak this long is the engine genuinely not emitting the field.
 */
export const FIELD_ABSENCE_STREAK_TO_REPORT = 6;

/**
 * Consecutive `ambiguous-streams` observations before the sampler spends its once-per-session
 * `unmeasured` latch. Same value and same reasoning as the field-absence streak — persist before
 * spending a latch — but a SEPARATE counter and a separate latch, because the two are different
 * facts: one is the engine ceasing to emit the fields, the other is the detector declining to
 * measure fields it can read perfectly well. A participant joining and leaving inside 30 s is a
 * momentary ambiguity that should not consume the latch a genuinely group-shaped call needs.
 */
export const AMBIGUOUS_STREAK_TO_REPORT = 6;

export const MAX_CLOCK_SKEW_MS = 1_000;

/**
 * Local interface declares every field `unknown` and narrows at runtime.
 * Mandatory, not stylistic: `remote-outbound-rtp` has no DOM type at all, and
 * `lib.dom.d.ts` declares `estimatedPlayoutTimestamp` -- a field Chromium does
 * not implement. Do not import or reference any DOM RTC stats type here.
 */
export interface AvSyncStatsEntry {
  readonly [key: string]: unknown;
}

export type AvSyncUnusableReason =
  | 'first-sample'
  | 'missing-stats'
  | 'stale-stats'
  | 'future-stats'
  | 'stream-changed'
  | 'counter-reset'
  | 'out-of-order'
  | 'field-absent'
  | 'sr-not-advanced'
  | 'unpaired'
  | 'kind-incomplete'
  | 'ambiguous-streams'
  | 'clock-regression'
  | 'insufficient-samples'
  | 'offset-step';

export type AvSyncObservation =
  | { usable: false; reason: AvSyncUnusableReason }
  | { usable: true; verdict: 'steady' }
  | {
      usable: true;
      verdict: 'drift';
      slopeMsPerSec: number;
      spanSec: number;
      samples: number;
      jbSkewMs: number | null;
    };

export interface AvSyncDriftDetector {
  observe(entries: Iterable<unknown>, observedAtMs: number): AvSyncObservation;
  reset(): void;
}

const KINDS = ['audio', 'video'] as const;
type Kind = (typeof KINDS)[number];

/** One kind's validated, narrowed per-sample state -- everything guards 8-13 compare against. */
interface KindSnapshot {
  readonly timestamp: number;
  readonly remoteTimestamp: number;
  readonly ssrc: unknown;
  readonly trackIdentifier: unknown;
  readonly jbSec: number;
  readonly jbCount: number;
  readonly offsetMs: number;
}

/** x = observedAtMs (ms), y = skew (ms). jbSkewMs is the INTERVAL delta computed at append time. */
interface Sample {
  readonly x: number;
  readonly y: number;
  readonly jbSkewMs: number | null;
}

function asFiniteNumber(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
}

interface Classification {
  readonly sawInbound: boolean;
  readonly inbound: ReadonlyMap<Kind, AvSyncStatsEntry>;
  readonly outbound: ReadonlyMap<Kind, AvSyncStatsEntry>;
  /** Per-kind entry counts, independent of the last-write-wins `inbound`/`outbound` maps --
   *  these are what let guard #3 detect a kind with MORE THAN ONE entry rather than silently
   *  keeping only the last one seen. */
  readonly inboundCounts: ReadonlyMap<Kind, number>;
  readonly outboundCounts: ReadonlyMap<Kind, number>;
}

/** Groups one getStats()-shaped report's entries by kind and RTP stats type.
 *
 *  SR-LINKAGE IS THE ADMISSION TEST, not merely `type === 'inbound-rtp'`. An inbound
 *  stream that carries no string `remoteId` has no Sender Report partner, so it can never
 *  contribute an offset and is not evidence of a second publisher either. mediasoup-client
 *  creates exactly such a stream in EVERY call: its bandwidth `probator` consumer appears as
 *  an `inbound-rtp` of kind video (ssrc 1234, `mid: 'probator'`) with no `remoteId` and no
 *  `remote-outbound-rtp`. Counting it made `hasAmbiguousStreams` fire on ordinary two-party
 *  calls -- measured on a live call (#2941 AC-11), where the detector refused all 22 ticks
 *  and the feature was inert in production with the whole suite green. Selection is resolved
 *  AFTER the scan by matching the kind's SR `id`, so it no longer depends on iteration order
 *  (last-write-wins previously made the choice a function of Chromium's report ordering). */
function bumpCount(counts: Map<Kind, number>, kind: Kind): void {
  counts.set(kind, (counts.get(kind) ?? 0) + 1);
}

function pushCandidate(
  candidates: Map<Kind, AvSyncStatsEntry[]>,
  kind: Kind,
  entry: AvSyncStatsEntry
): void {
  const list = candidates.get(kind);
  if (list) list.push(entry);
  else candidates.set(kind, [entry]);
}

/** Order-independent selection: prefer the candidate whose `remoteId` names this kind's SR, so
 *  the choice does not depend on Chromium's report ordering.
 *
 *  The trailing `at(-1)` fallback applies in TWO cases, not one: a kind with candidates but no
 *  SR yet, and a kind whose SR exists but names none of them (a stale or foreign `remoteId`).
 *  Both are harmless, for DIFFERENT reasons, and neither leans on the fallback picking well:
 *  with `inboundCounts > 1` the ambiguity guard refuses before `inbound` is ever consumed, and
 *  with exactly one candidate a non-matching id fails `isPairedForKind` and surfaces as
 *  `unpaired`. An earlier version of this comment claimed only the first case, which was a
 *  narrower guarantee than the code makes. */
function selectInbound(
  candidates: ReadonlyMap<Kind, AvSyncStatsEntry[]>,
  outbound: ReadonlyMap<Kind, AvSyncStatsEntry>
): Map<Kind, AvSyncStatsEntry> {
  const chosen = new Map<Kind, AvSyncStatsEntry>();
  for (const [kind, list] of candidates) {
    const srId = outbound.get(kind)?.id;
    const matched = typeof srId === 'string' ? list.find((e) => e.remoteId === srId) : undefined;
    const pick = matched ?? list.at(-1);
    if (pick) chosen.set(kind, pick);
  }
  return chosen;
}

function classifyEntries(entries: Iterable<unknown>): Classification {
  const outbound = new Map<Kind, AvSyncStatsEntry>();
  const inboundCounts = new Map<Kind, number>();
  const outboundCounts = new Map<Kind, number>();
  const inboundCandidates = new Map<Kind, AvSyncStatsEntry[]>();
  let sawInbound = false;

  for (const raw of entries) {
    if (raw === null || typeof raw !== 'object') continue;
    const entry = raw as AvSyncStatsEntry;
    const isInbound = entry.type === 'inbound-rtp';
    // `sawInbound` deliberately counts EVERY inbound-rtp, including ones this function then
    // refuses to admit -- it answers "is the engine emitting RTP stats at all", which is a
    // different question from "is there a measurable stream" and drives `missing-stats`.
    if (isInbound) sawInbound = true;

    const kind = entry.kind;
    if (kind !== 'audio' && kind !== 'video') continue;

    if (isInbound) {
      // No string `remoteId` means no Sender Report partner: not measurable, and not
      // evidence of a second publisher either. See this function's header comment.
      if (typeof entry.remoteId !== 'string') continue;
      pushCandidate(inboundCandidates, kind, entry);
      bumpCount(inboundCounts, kind);
    } else if (entry.type === 'remote-outbound-rtp') {
      outbound.set(kind, entry);
      bumpCount(outboundCounts, kind);
    }
  }

  return {
    sawInbound,
    inbound: selectInbound(inboundCandidates, outbound),
    outbound,
    inboundCounts,
    outboundCounts,
  };
}

/** Guard #3 -- more than one SR-LINKED `inbound-rtp`, or more than one `remote-outbound-rtp`,
 *  for a single kind. There is ONE recv transport per kind for the whole call, so this fires
 *  only when a second PUBLISHER is present -- never for a synthetic stream with no SR partner
 *  (see `classifyEntries`: mediasoup's `probator` consumer is one, in every single call).
 *  Otherwise `classifyEntries`' selection would silently keep one peer's
 *  entry for a kind while discarding the others -- and the inbound and outbound maps could end
 *  up holding entries from DIFFERENT publishers, voiding the shared-sender-clock-basis premise
 *  this module's header comment rests on. Refusing beats selecting: a skew computed across two
 *  different publishers' clocks is a plausible-looking wrong number, and per this module's own
 *  §7.1 rule a wrong number that looks valid is strictly worse than an honest refusal. Must be
 *  checked BEFORE guard #4 (`unpaired`) -- an ambiguous, mis-paired set is a stronger and
 *  earlier fact than a pairing mismatch, and must never surface as `unpaired`. Does not clear
 *  history and does not update `previous`: this is a refusal to measure, not a discontinuity. */
function hasAmbiguousStreams(
  inboundCounts: ReadonlyMap<Kind, number>,
  outboundCounts: ReadonlyMap<Kind, number>
): boolean {
  for (const kind of KINDS) {
    if ((inboundCounts.get(kind) ?? 0) > 1) return true;
    if ((outboundCounts.get(kind) ?? 0) > 1) return true;
  }
  return false;
}

/** Both directions must agree: I_k.remoteId === R_k.id AND R_k.localId === I_k.id. All four
 *  id fields must be strings before comparing -- `inEntry.remoteId === outEntry.id` is
 *  `undefined === undefined` -> true when the engine stops emitting the fields, so without this
 *  check a dead engine would read as *paired* rather than as the field-absence it actually is. */
function isPairedForKind(inEntry: AvSyncStatsEntry, outEntry: AvSyncStatsEntry): boolean {
  const { remoteId, id: inId } = inEntry;
  const { id: outId, localId } = outEntry;
  if (
    typeof remoteId !== 'string' ||
    typeof outId !== 'string' ||
    typeof localId !== 'string' ||
    typeof inId !== 'string'
  ) {
    return false;
  }
  return remoteId === outId && localId === inId;
}

/** Guard #4 -- only evaluated for a kind whose remote-outbound-rtp entry exists at all; a
 *  kind with no such entry is a `field-absent` case (guard #5), not a pairing failure. */
function anyUnpaired(
  inbound: ReadonlyMap<Kind, AvSyncStatsEntry>,
  outbound: ReadonlyMap<Kind, AvSyncStatsEntry>
): boolean {
  for (const kind of KINDS) {
    const inEntry = inbound.get(kind);
    const outEntry = outbound.get(kind);
    if (!inEntry || !outEntry) continue;
    if (!isPairedForKind(inEntry, outEntry)) return true;
  }
  return false;
}

/** Guard #5 -- the remote-outbound-rtp entry is missing for a kind, or its timing fields are
 *  not finite numbers. */
function anyFieldAbsent(outbound: ReadonlyMap<Kind, AvSyncStatsEntry>): boolean {
  for (const kind of KINDS) {
    const outEntry = outbound.get(kind);
    if (!outEntry) return true;
    if (asFiniteNumber(outEntry.remoteTimestamp) === null) return true;
    if (asFiniteNumber(outEntry.timestamp) === null) return true;
  }
  return false;
}

/** Builds one kind's narrowed snapshot, or null if any required field is not a finite number.
 *  A null here also folds into `field-absent` -- the inbound-side counterpart of guard #4. */
function buildSnapshot(inEntry: AvSyncStatsEntry, outEntry: AvSyncStatsEntry): KindSnapshot | null {
  const timestamp = asFiniteNumber(inEntry.timestamp);
  const remoteTimestamp = asFiniteNumber(outEntry.remoteTimestamp);
  const outTimestamp = asFiniteNumber(outEntry.timestamp);
  const jbSec = asFiniteNumber(inEntry.jitterBufferDelay);
  const jbCount = asFiniteNumber(inEntry.jitterBufferEmittedCount);

  if (
    timestamp === null ||
    remoteTimestamp === null ||
    outTimestamp === null ||
    jbSec === null ||
    jbCount === null
  ) {
    return null;
  }

  return {
    timestamp,
    remoteTimestamp,
    ssrc: inEntry.ssrc,
    trackIdentifier: inEntry.trackIdentifier,
    jbSec,
    jbCount,
    offsetMs: outTimestamp - remoteTimestamp,
  };
}

function buildSnapshots(
  inbound: ReadonlyMap<Kind, AvSyncStatsEntry>,
  outbound: ReadonlyMap<Kind, AvSyncStatsEntry>
): Record<Kind, KindSnapshot> | null {
  const audioIn = inbound.get('audio');
  const videoIn = inbound.get('video');
  const audioOut = outbound.get('audio');
  const videoOut = outbound.get('video');
  if (!audioIn || !videoIn || !audioOut || !videoOut) return null;

  const audio = buildSnapshot(audioIn, audioOut);
  const video = buildSnapshot(videoIn, videoOut);
  if (!audio || !video) return null;

  return { audio, video };
}

/** Guards #6 and #7. Future-stats is checked across BOTH kinds before stale-stats is checked
 *  across either, so future-stats always wins regardless of which kind trips which. */
function checkClockBounds(
  current: Record<Kind, KindSnapshot>,
  observedAtMs: number
): 'future-stats' | 'stale-stats' | null {
  const snaps = [current.audio, current.video];
  for (const snap of snaps) {
    if (snap.timestamp > observedAtMs + MAX_CLOCK_SKEW_MS) return 'future-stats';
  }
  for (const snap of snaps) {
    if (observedAtMs - snap.timestamp > STALE_AFTER_MS) return 'stale-stats';
  }
  return null;
}

/** Guard #9. Strictly-decreased, not merely unchanged -- a frozen SR (guard #12's case) must
 *  not also read as regression. */
function hasClockRegression(
  current: Record<Kind, KindSnapshot>,
  previous: Record<Kind, KindSnapshot>
): boolean {
  return (
    current.audio.timestamp < previous.audio.timestamp ||
    current.video.timestamp < previous.video.timestamp ||
    current.audio.remoteTimestamp < previous.audio.remoteTimestamp ||
    current.video.remoteTimestamp < previous.video.remoteTimestamp
  );
}

/** Guard #10. */
function hasStreamChanged(
  current: Record<Kind, KindSnapshot>,
  previous: Record<Kind, KindSnapshot>
): boolean {
  return (
    current.audio.ssrc !== previous.audio.ssrc ||
    current.audio.trackIdentifier !== previous.audio.trackIdentifier ||
    current.video.ssrc !== previous.video.ssrc ||
    current.video.trackIdentifier !== previous.video.trackIdentifier
  );
}

/** Guard #11. */
function hasCounterReset(
  current: Record<Kind, KindSnapshot>,
  previous: Record<Kind, KindSnapshot>
): boolean {
  return (
    current.audio.jbSec < previous.audio.jbSec ||
    current.audio.jbCount < previous.audio.jbCount ||
    current.video.jbSec < previous.video.jbSec ||
    current.video.jbCount < previous.video.jbCount
  );
}

/** Guard #12 -- the single most dangerous false positive: a frozen SR with an advancing local
 *  timestamp would otherwise manufacture a perfect +1 ms/ms drift line. */
function hasSrNotAdvanced(
  current: Record<Kind, KindSnapshot>,
  previous: Record<Kind, KindSnapshot>
): boolean {
  return (
    current.audio.remoteTimestamp === previous.audio.remoteTimestamp ||
    current.video.remoteTimestamp === previous.video.remoteTimestamp
  );
}

function skewOf(snap: Record<Kind, KindSnapshot>): number {
  return snap.audio.offsetMs - snap.video.offsetMs;
}

/** Interval delta (never the lifetime ratio) in ms. Null when the counter did not advance --
 *  never divide by a non-positive count. */
function jbDeltaMs(current: KindSnapshot, previous: KindSnapshot): number | null {
  const dCount = current.jbCount - previous.jbCount;
  if (dCount <= 0) return null;
  const dSec = current.jbSec - previous.jbSec;
  return (1000 * dSec) / dCount;
}

function jbSkewOf(
  current: Record<Kind, KindSnapshot>,
  previous: Record<Kind, KindSnapshot>
): number | null {
  const audioDelta = jbDeltaMs(current.audio, previous.audio);
  const videoDelta = jbDeltaMs(current.video, previous.video);
  return audioDelta === null || videoDelta === null ? null : audioDelta - videoDelta;
}

/** Median of pairwise slopes. Tolerates ~29% outliers with no tuning parameter of its own --
 *  OLS is dragged by a single noisy endpoint, precisely the flake axis a noisy-but-bounded
 *  series exercises. Endpoint-difference is rejected for the same reason: maximally
 *  endpoint-sensitive. x is ms, y is ms, so the raw slope is ms/ms; the caller multiplies by
 *  1000 for ms/s. */
export function theilSenSlopeMsPerMs(points: readonly Sample[]): number {
  const slopes: number[] = [];
  for (let i = 0; i < points.length - 1; i++) {
    for (let j = i + 1; j < points.length; j++) {
      const dx = points[j].x - points[i].x;
      if (dx !== 0) slopes.push((points[j].y - points[i].y) / dx);
    }
  }
  if (slopes.length === 0) return 0;
  slopes.sort((a, b) => a - b);
  const mid = slopes.length >> 1;
  return slopes.length % 2 === 0 ? (slopes[mid - 1] + slopes[mid]) / 2 : slopes[mid];
}

/** The last usable-comparison snapshot, paired with the clock reading it was captured at.
 *  Bundling the two in one field (rather than two separately-optional fields) makes it
 *  impossible for the snapshot and its timestamp to drift out of sync -- every site that
 *  updates one updates both, atomically, by construction. `observedAtMs` is what the
 *  offset-step rate check (guard #13) diffs against the CURRENT observation's clock reading,
 *  and is deliberately NOT `lastObservedAtMs`: on `sr-not-advanced` the snapshot (and this
 *  timestamp) hold at the last GOOD sample while `lastObservedAtMs` keeps advancing, so after a
 *  long SR stall this field correctly reflects the arbitrarily long gap since the last sample
 *  that actually advanced the clock basis. */
interface PreviousState {
  readonly snapshot: Record<Kind, KindSnapshot>;
  readonly observedAtMs: number;
}

/** Guards #9-#11 in evaluation order. Each clears history and advances `previousState`:
 *  the sample pair straddling the discontinuity is meaningless, but the NEW sample is a valid
 *  basis for the next one. Order is load-bearing and is the array's order. */
const DISCONTINUITY_GUARDS: ReadonlyArray<
  readonly [
    (current: Record<Kind, KindSnapshot>, previous: Record<Kind, KindSnapshot>) => boolean,
    Extract<AvSyncUnusableReason, 'clock-regression' | 'stream-changed' | 'counter-reset'>,
  ]
> = [
  [hasClockRegression, 'clock-regression'],
  [hasStreamChanged, 'stream-changed'],
  [hasCounterReset, 'counter-reset'],
];

/** Guard #13. RATE-based, not absolute -- see MAX_STEP_RATE_MS_PER_SEC. `elapsedSec` is the gap
 *  since the last sample that actually updated `previousState` (which may span several
 *  `sr-not-advanced` ticks after an SR stall), so the same 6x headroom applies whether the gap
 *  is one normal tick or arbitrarily long. A non-positive gap cannot yield a rate at all, and
 *  is already refused upstream as `out-of-order`. */
function isOffsetStep(skew: number, previousSkew: number, elapsedSec: number): boolean {
  return elapsedSec > 0 && Math.abs(skew - previousSkew) / elapsedSec >= MAX_STEP_RATE_MS_PER_SEC;
}

class AvSyncDriftDetectorImpl implements AvSyncDriftDetector {
  private previousState: PreviousState | undefined;
  private history: Sample[] = [];
  private lastObservedAtMs: number | undefined;
  /** Consecutive ticks whose skew rate exceeded MAX_STEP_RATE_MS_PER_SEC. 1 = a genuine step;
   *  2+ = a ramp steeper than the step ceiling, which must be measured, not discarded. */
  private consecutiveOffsetSteps = 0;

  /**
   * Guard #13's persistence test. Returns true only for an ISOLATED rate excursion, which is
   * the one that deserves to clear the window.
   *
   * A DISCONTINUITY IS A ONE-OFF BY DEFINITION. A layer switch steps the capture-time basis
   * once; it does not step again on the very next sample, and again after that. A rate that
   * keeps exceeding the ceiling tick after tick is not a sequence of steps at all -- it is a
   * ramp steeper than the ceiling, i.e. exactly the defect this detector exists to report.
   *
   * Clearing history on EVERY such tick made the detector blind above MAX_STEP_RATE_MS_PER_SEC
   * (6 ms/s). Measured before the fix: a sustained 10 ms/s ramp produced `offset-step` on all
   * 17 ticks and never once reached MIN_SAMPLES, so it stayed silent while accumulating 600 ms
   * of skew per minute. The severity response was inverted -- the worse the drift, the more
   * certainly it went unreported.
   */
  private isIsolatedOffsetStep(stepped: boolean): boolean {
    if (!stepped) {
      this.consecutiveOffsetSteps = 0;
      return false;
    }
    this.consecutiveOffsetSteps += 1;
    return this.consecutiveOffsetSteps === 1;
  }

  /** Shared body of guards #9-#11 and #13: clear the window, rebase on the new sample. */
  private discontinuity(
    snapshot: Record<Kind, KindSnapshot>,
    observedAtMs: number,
    reason: AvSyncUnusableReason
  ): AvSyncObservation {
    this.previousState = { snapshot, observedAtMs };
    this.history = [];
    return { usable: false, reason };
  }

  observe(entries: Iterable<unknown>, observedAtMs: number): AvSyncObservation {
    const { sawInbound, inbound, outbound, inboundCounts, outboundCounts } =
      classifyEntries(entries);

    if (!sawInbound) return { usable: false, reason: 'missing-stats' };
    if (inbound.size < 2) return { usable: false, reason: 'kind-incomplete' };
    if (hasAmbiguousStreams(inboundCounts, outboundCounts)) {
      return { usable: false, reason: 'ambiguous-streams' };
    }
    if (anyUnpaired(inbound, outbound)) return { usable: false, reason: 'unpaired' };
    if (anyFieldAbsent(outbound)) return { usable: false, reason: 'field-absent' };

    const current = buildSnapshots(inbound, outbound);
    if (!current) return { usable: false, reason: 'field-absent' };

    const clockBoundsReason = checkClockBounds(current, observedAtMs);
    if (clockBoundsReason) return { usable: false, reason: clockBoundsReason };

    if (this.lastObservedAtMs !== undefined && observedAtMs <= this.lastObservedAtMs) {
      return { usable: false, reason: 'out-of-order' };
    }
    this.lastObservedAtMs = observedAtMs;

    const previous = this.previousState;
    if (!previous) {
      this.previousState = { snapshot: current, observedAtMs };
      return { usable: false, reason: 'first-sample' };
    }

    // Guards #9, #10 and #11, IN ORDER -- each a discontinuity that clears history AND
    // advances `previousState`. Table-driven purely so the shared three-line body is written
    // once; the array order IS the guard order and first match still wins.
    for (const [fires, reason] of DISCONTINUITY_GUARDS) {
      if (fires(current, previous.snapshot))
        return this.discontinuity(current, observedAtMs, reason);
    }

    // Guard #12, BEFORE the step check: a frozen SR manufactures a perfect line, and it must
    // NOT advance `previousState` -- holding it is what lets `elapsedSec` widen across a stall.
    if (hasSrNotAdvanced(current, previous.snapshot)) {
      return { usable: false, reason: 'sr-not-advanced' };
    }

    const skew = skewOf(current);
    const previousSkew = skewOf(previous.snapshot);
    const elapsedSec = (observedAtMs - previous.observedAtMs) / 1000;
    if (this.isIsolatedOffsetStep(isOffsetStep(skew, previousSkew, elapsedSec))) {
      return this.discontinuity(current, observedAtMs, 'offset-step');
    }

    const jbSkewMs = jbSkewOf(current, previous.snapshot);
    this.previousState = { snapshot: current, observedAtMs };
    this.history = [...this.history, { x: observedAtMs, y: skew, jbSkewMs }].slice(-HISTORY_LIMIT);

    return this.evaluateWindow();
  }

  private evaluateWindow(): AvSyncObservation {
    const n = this.history.length;
    const first = this.history[0];
    const last = this.history[n - 1];
    const spanMs = last.x - first.x;

    if (n < MIN_SAMPLES || spanMs < MIN_WINDOW_MS) {
      return { usable: false, reason: 'insufficient-samples' };
    }

    const slopeMsPerSec = theilSenSlopeMsPerMs(this.history) * 1000;
    const excursionMs = Math.abs(last.y - first.y);
    const isDrift =
      Math.abs(slopeMsPerSec) >= DRIFT_SLOPE_MS_PER_SEC &&
      excursionMs >= MIN_EXCURSION_MS &&
      spanMs >= MIN_WINDOW_MS;

    if (!isDrift) return { usable: true, verdict: 'steady' };

    return {
      usable: true,
      verdict: 'drift',
      slopeMsPerSec,
      spanSec: spanMs / 1000,
      samples: n,
      jbSkewMs: last.jbSkewMs,
    };
  }

  reset(): void {
    this.previousState = undefined;
    this.consecutiveOffsetSteps = 0;
    this.history = [];
    this.lastObservedAtMs = undefined;
  }
}

export function createAvSyncDriftDetector(): AvSyncDriftDetector {
  return new AvSyncDriftDetectorImpl();
}

/** Shallow-copies one entry, prefixing its id-bearing fields when they are strings.
 *  Non-object entries pass through unchanged -- the caller (`observe`'s `classifyEntries`)
 *  already discards them, so there is nothing to namespace. */
function namespaceStatsEntry(raw: unknown, prefix: 'a:' | 'v:'): unknown {
  if (raw === null || typeof raw !== 'object') return raw;
  const entry = raw as Record<string, unknown>;
  const copy: Record<string, unknown> = { ...entry };
  for (const key of ['id', 'remoteId', 'localId'] as const) {
    if (typeof copy[key] === 'string') copy[key] = `${prefix}${copy[key]}`;
  }
  return copy;
}

/**
 * Merge two per-transport stats reports into one iterable for `observe()`.
 *
 * `RTCStats.id` is unique only WITHIN a report, and audio and video ride separate
 * RTCPeerConnections (#291), so a raw merge can mis-pair across transports rather
 * than refuse. Namespacing the three id-bearing fields makes a cross-transport
 * pairing impossible by construction.
 *
 * `observe()` REQUIRES id-uniqueness across the iterable it is handed; it does not
 * defend against a violation, because a duplicate-id check inside the estimator
 * would be a second source of truth for a property the caller owns.
 */
export function mergeAvSyncStats(
  audioEntries: Iterable<unknown>,
  videoEntries: Iterable<unknown>
): unknown[] {
  const merged: unknown[] = [];
  for (const raw of audioEntries) merged.push(namespaceStatsEntry(raw, 'a:'));
  for (const raw of videoEntries) merged.push(namespaceStatsEntry(raw, 'v:'));
  return merged;
}
