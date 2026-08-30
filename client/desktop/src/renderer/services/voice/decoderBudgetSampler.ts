const HISTORY_LIMIT = 20;
const STALE_AFTER_MS = 10_000;

export type DecoderBudgetUnknownReason =
  | 'first-sample'
  | 'paused'
  | 'missing-stats'
  | 'stale-stats'
  | 'future-stats'
  | 'no-progress'
  | 'invalid-derived-metrics'
  | 'stream-changed'
  | 'counter-reset'
  | 'out-of-order';

export type DecoderBudgetResult =
  | { usable: false; reason: DecoderBudgetUnknownReason }
  | { usable: true; rho: number; p95DecodeMs: number; currentFps: number };

export interface DecoderStatsReport {
  id?: unknown;
  type?: unknown;
  kind?: unknown;
  active?: unknown;
  ssrc?: unknown;
  framesPerSecond?: unknown;
  totalDecodeTime?: unknown;
  framesDecoded?: unknown;
  timestamp?: unknown;
}

export interface SelectedDecoderStatsReport {
  reportId: string;
  report: DecoderStatsReport;
}

export interface DecoderBudgetObservation {
  consumerId: string;
  /** Stable RTCStats report identity, including SSRC when available. */
  reportId: string;
  paused: boolean;
  report: DecoderStatsReport | null | undefined;
  /** Use `performance.timeOrigin + performance.now()`, the clock basis of RTCStats.timestamp. */
  observedAtMs: number;
}

interface Snapshot {
  reportId: string;
  totalDecodeTime: number;
  framesDecoded: number;
  timestamp: number;
}

interface ConsumerState {
  baseline: Snapshot | undefined;
  decodeHistoryMs: number[];
}

interface DecoderReportCandidate extends SelectedDecoderStatsReport {
  entryId: string;
  stableId: string;
  matchesNegotiatedSsrc: boolean;
  hasPositiveReportedFps: boolean;
  timestamp: number;
}

function validSsrc(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0 ? value : undefined;
}

/**
 * Selects one active inbound-video report deterministically.
 * Priority: negotiated encoding SSRC, positive reported FPS (selection evidence only),
 * newest timestamp, then stable report id. Interval deltas still determine metric FPS.
 */
export function selectInboundVideoDecoderReport(
  reports: Iterable<[string, unknown]>,
  negotiatedSsrcs: ReadonlySet<number>
): SelectedDecoderStatsReport | null {
  const candidates: DecoderReportCandidate[] = [];
  for (const [entryId, value] of reports) {
    if (!value || typeof value !== 'object') continue;
    const report = value as DecoderStatsReport;
    if (report.type !== 'inbound-rtp' || report.kind !== 'video' || report.active === false) {
      continue;
    }

    const stableId = typeof report.id === 'string' && report.id.length > 0 ? report.id : entryId;
    const ssrc = validSsrc(report.ssrc);
    const framesPerSecond = report.framesPerSecond;
    const timestamp = report.timestamp;
    candidates.push({
      entryId,
      stableId,
      reportId: `${stableId}|${ssrc ?? 'none'}`,
      report,
      matchesNegotiatedSsrc: ssrc !== undefined && negotiatedSsrcs.has(ssrc),
      hasPositiveReportedFps:
        typeof framesPerSecond === 'number' &&
        Number.isFinite(framesPerSecond) &&
        framesPerSecond > 0,
      timestamp:
        typeof timestamp === 'number' && Number.isFinite(timestamp)
          ? timestamp
          : Number.NEGATIVE_INFINITY,
    });
  }

  candidates.sort((left, right) => {
    if (left.matchesNegotiatedSsrc !== right.matchesNegotiatedSsrc) {
      return left.matchesNegotiatedSsrc ? -1 : 1;
    }
    if (left.hasPositiveReportedFps !== right.hasPositiveReportedFps) {
      return left.hasPositiveReportedFps ? -1 : 1;
    }
    if (left.timestamp !== right.timestamp) return left.timestamp > right.timestamp ? -1 : 1;
    return left.stableId.localeCompare(right.stableId) || left.entryId.localeCompare(right.entryId);
  });

  const selected = candidates[0];
  return selected ? { reportId: selected.reportId, report: selected.report } : null;
}

function snapshotOf(observation: DecoderBudgetObservation): Snapshot | null {
  const { report, reportId, observedAtMs } = observation;
  const totalDecodeTime = report?.totalDecodeTime;
  const framesDecoded = report?.framesDecoded;
  const timestamp = report?.timestamp;

  if (
    typeof totalDecodeTime !== 'number' ||
    !Number.isFinite(totalDecodeTime) ||
    totalDecodeTime < 0 ||
    typeof framesDecoded !== 'number' ||
    !Number.isSafeInteger(framesDecoded) ||
    framesDecoded < 0 ||
    typeof timestamp !== 'number' ||
    !Number.isFinite(timestamp) ||
    timestamp < 0 ||
    !Number.isFinite(observedAtMs) ||
    observedAtMs < 0 ||
    typeof reportId !== 'string' ||
    reportId.length === 0
  ) {
    return null;
  }

  return { reportId, totalDecodeTime, framesDecoded, timestamp };
}

/** Nearest-rank percentile: rank = ceil(p * sample count), using a one-based rank. */
function nearestRankP95(values: readonly number[]): number {
  const sorted = [...values].sort((left, right) => left - right);
  return sorted[Math.ceil(sorted.length * 0.95) - 1] ?? 0;
}

/**
 * Converts lifetime inbound-rtp decoder counters into bounded, per-consumer interval samples.
 * Unknown observations never contribute to the rolling percentile.
 */
export class DecoderBudgetSampler {
  private readonly states = new Map<string, ConsumerState>();

  observe(observation: DecoderBudgetObservation): DecoderBudgetResult {
    const { consumerId } = observation;
    if (observation.paused) {
      this.states.delete(consumerId);
      return { usable: false, reason: 'paused' };
    }

    const snapshot = snapshotOf(observation);
    if (!snapshot) return this.resetBaseline(consumerId, 'missing-stats');
    if (snapshot.timestamp > observation.observedAtMs) {
      return this.resetBaseline(consumerId, 'future-stats');
    }
    if (observation.observedAtMs - snapshot.timestamp > STALE_AFTER_MS) {
      return this.resetBaseline(consumerId, 'stale-stats');
    }

    const state = this.states.get(consumerId) ?? {
      baseline: undefined,
      decodeHistoryMs: [],
    };
    this.states.set(consumerId, state);

    const baseline = state.baseline;
    if (!baseline) {
      state.baseline = snapshot;
      return { usable: false, reason: 'first-sample' };
    }

    if (snapshot.reportId !== baseline.reportId) {
      state.baseline = snapshot;
      state.decodeHistoryMs.length = 0;
      return { usable: false, reason: 'stream-changed' };
    }

    if (snapshot.timestamp <= baseline.timestamp) {
      state.baseline = snapshot;
      return { usable: false, reason: 'out-of-order' };
    }

    if (
      snapshot.totalDecodeTime < baseline.totalDecodeTime ||
      snapshot.framesDecoded < baseline.framesDecoded
    ) {
      state.baseline = snapshot;
      state.decodeHistoryMs.length = 0;
      return { usable: false, reason: 'counter-reset' };
    }

    const elapsedMs = snapshot.timestamp - baseline.timestamp;
    const framesDelta = snapshot.framesDecoded - baseline.framesDecoded;
    const decodeTimeDelta = snapshot.totalDecodeTime - baseline.totalDecodeTime;
    state.baseline = snapshot;

    if (framesDelta === 0) return { usable: false, reason: 'no-progress' };

    const currentFps = (framesDelta * 1_000) / elapsedMs;
    const intervalDecodeMs = (decodeTimeDelta * 1_000) / framesDelta;
    if (!Number.isFinite(currentFps) || !Number.isFinite(intervalDecodeMs)) {
      return { usable: false, reason: 'invalid-derived-metrics' };
    }

    const nextHistory = [...state.decodeHistoryMs, intervalDecodeMs].slice(-HISTORY_LIMIT);
    const p95DecodeMs = nearestRankP95(nextHistory);
    const rho = (p95DecodeMs * currentFps) / 1_000;
    if (!Number.isFinite(p95DecodeMs) || !Number.isFinite(rho)) {
      return { usable: false, reason: 'invalid-derived-metrics' };
    }
    state.decodeHistoryMs = nextHistory;
    return {
      usable: true,
      rho,
      p95DecodeMs,
      currentFps,
    };
  }

  deleteConsumer(consumerId: string): void {
    this.states.delete(consumerId);
  }

  clear(): void {
    this.states.clear();
  }

  private resetBaseline(
    consumerId: string,
    reason: Extract<DecoderBudgetUnknownReason, 'missing-stats' | 'stale-stats' | 'future-stats'>
  ): DecoderBudgetResult {
    const state = this.states.get(consumerId);
    if (state) state.baseline = undefined;
    return { usable: false, reason };
  }
}
