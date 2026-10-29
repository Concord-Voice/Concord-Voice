import type { PeakWindow } from './micProcessor';

export type CalibrationSummary = {
  noiseP95Dbfs: number;
  speechP10Dbfs: number;
  speechP90Dbfs: number;
  quietZeroFraction: number;
  speechZeroFraction: number;
};

export type CalibrationResult =
  | { kind: 'recommended'; thresholdDbfs: number; summary: CalibrationSummary }
  | {
      kind: 'refused';
      reason: 'invalid' | 'clipping' | 'insufficient' | 'silent-reference' | 'overlap';
      summary: CalibrationSummary | null;
    };

type Observations = { levels: number[]; zeroCount: number };
type Evidence = Observations | { reason: 'invalid' | 'clipping' };

function inspectWindows(windows: readonly PeakWindow[], frames: number): Evidence {
  const levels: number[] = [];
  let zeroCount = 0;
  for (const window of windows) {
    if (
      !window ||
      !Number.isFinite(window.peak) ||
      window.peak < 0 ||
      window.frames !== frames ||
      window.valid !== true ||
      typeof window.overloaded !== 'boolean'
    ) {
      return { reason: 'invalid' };
    }
    if (window.overloaded || window.peak >= 1) return { reason: 'clipping' };
    if (window.peak === 0) zeroCount++;
    else levels.push(20 * Math.log10(window.peak));
  }
  // Only our bounded copy is sorted; caller evidence remains untouched.
  levels.sort((a, b) => a - b);
  return { levels, zeroCount };
}

function nearestRank(sorted: readonly number[], fraction: number): number {
  return sorted[Math.ceil(fraction * sorted.length) - 1];
}

/** A starting point constrained by every nonzero observation, never by quantiles. */
export function recommendGateThreshold(
  quiet: readonly PeakWindow[],
  speech: readonly PeakWindow[],
  sampleRate: number
): CalibrationResult {
  const windowFrames = Math.round(sampleRate * 0.02);
  const quietLimit = Math.ceil((sampleRate * 10) / windowFrames);
  const speechLimit = Math.floor((sampleRate * 20) / windowFrames);
  if (
    !Number.isFinite(sampleRate) ||
    sampleRate <= 0 ||
    !Number.isSafeInteger(windowFrames) ||
    windowFrames < 1 ||
    !Number.isSafeInteger(quietLimit) ||
    quietLimit < 1 ||
    quietLimit > 501 ||
    !Number.isSafeInteger(speechLimit) ||
    speechLimit < 1 ||
    speechLimit > 1000 ||
    !Array.isArray(quiet) ||
    !Array.isArray(speech) ||
    quiet.length > quietLimit ||
    speech.length > speechLimit
  ) {
    return { kind: 'refused', reason: 'invalid', summary: null };
  }

  const room = inspectWindows(quiet, windowFrames);
  if ('reason' in room) return { kind: 'refused', reason: room.reason, summary: null };
  const voice = inspectWindows(speech, windowFrames);
  if ('reason' in voice) return { kind: 'refused', reason: voice.reason, summary: null };
  if (
    quiet.length < quietLimit ||
    speech.length * windowFrames < sampleRate * 3 ||
    voice.levels.length * windowFrames < sampleRate ||
    voice.zeroCount > speech.length / 2
  ) {
    return { kind: 'refused', reason: 'insufficient', summary: null };
  }
  const noiseCeiling = room.levels.at(-1);
  if (noiseCeiling === undefined) {
    return { kind: 'refused', reason: 'silent-reference', summary: null };
  }

  const summary: CalibrationSummary = {
    noiseP95Dbfs: nearestRank(room.levels, 0.95),
    speechP10Dbfs: nearestRank(voice.levels, 0.1),
    speechP90Dbfs: nearestRank(voice.levels, 0.9),
    quietZeroFraction: room.zeroCount / quiet.length,
    speechZeroFraction: voice.zeroCount / speech.length,
  };
  // A single weak word or ambient excursion must still veto the proposal.
  const speechFloor = voice.levels[0];
  const lower = Math.max(-80, Math.ceil(noiseCeiling + 3));
  const upper = Math.min(-20, Math.floor(speechFloor - 3));
  if (lower > upper) return { kind: 'refused', reason: 'overlap', summary };
  return { kind: 'recommended', thresholdDbfs: lower, summary };
}
