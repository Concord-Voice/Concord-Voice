import { resetAllStores } from '../../helpers/store-helpers';
import { beforeEach, describe, expect, it } from 'vitest';
import { recommendGateThreshold } from '@/renderer/services/voice/gateCalibration';
import type { PeakWindow } from '@/renderer/services/voice/micProcessor';

const level = (dbfs: number, frames = 960): PeakWindow => ({
  peak: 10 ** (dbfs / 20),
  frames,
  valid: true,
  overloaded: false,
});
const windows = (count: number, dbfs: number, frames = 960) =>
  Array.from({ length: count }, () => level(dbfs, frames));
const quiet = () => windows(500, -60);
const speech = () => windows(150, -30);

describe('conservative gate recommendation', () => {
  it('chooses the lowest supported safe integer and leaves caller evidence unchanged', () => {
    const room = Object.freeze(quiet().map(Object.freeze));
    const voice = Object.freeze(speech().map(Object.freeze));
    expect(recommendGateThreshold(room, voice, 48_000)).toMatchObject({
      kind: 'recommended',
      thresholdDbfs: -57,
      summary: {
        noiseP95Dbfs: -60,
        speechP10Dbfs: -30,
        speechP90Dbfs: -30,
        quietZeroFraction: 0,
        speechZeroFraction: 0,
      },
    });
  });

  it.each(['weak word', 'nonzero pause', 'ambient excursion'])(
    'retains %s even outside displayed quantiles',
    (scenario) => {
      const room = quiet();
      const voice = speech();
      if (scenario === 'ambient excursion') room[11] = level(-31);
      else voice[74] = level(scenario === 'weak word' ? -59 : -60);
      expect(recommendGateThreshold(room, voice, 48_000)).toMatchObject({
        kind: 'refused',
        reason: 'overlap',
      });
    }
  );

  it('uses nearest-rank quantiles while retaining exact-zero fractions', () => {
    const room = [...windows(25, -Infinity), ...windows(450, -60), ...windows(25, -50)];
    const voice = [
      ...windows(50, -Infinity),
      ...windows(10, -40),
      ...windows(80, -30),
      ...windows(10, -25),
    ];
    expect(recommendGateThreshold(room, voice, 48_000)).toMatchObject({
      kind: 'recommended',
      thresholdDbfs: -47,
      summary: {
        noiseP95Dbfs: -50,
        speechP10Dbfs: -40,
        speechP90Dbfs: -30,
        quietZeroFraction: 0.05,
        speechZeroFraction: 1 / 3,
      },
    });
  });

  it.each([
    [-60.01, -30, -57],
    [-60, -30, -57],
    [-59.99, -30, -56],
    [-90, -70, -80],
    [-23, -10, -20],
    [-43.1, -37, -40],
  ])('rounds ambient %s and voice %s to safe threshold %s', (noise, voice, thresholdDbfs) => {
    expect(recommendGateThreshold(windows(500, noise), windows(150, voice), 48_000)).toMatchObject({
      kind: 'recommended',
      thresholdDbfs,
    });
  });

  it.each([
    [-43.1, -37.01],
    [-90, -78],
    [-22.9, -10],
  ])('refuses an unsupported or fractional gap for %s / %s', (noise, voice) => {
    expect(recommendGateThreshold(windows(500, noise), windows(150, voice), 48_000)).toMatchObject({
      kind: 'refused',
      reason: 'overlap',
    });
  });

  it('refuses an all-zero room even with useful voice', () => {
    expect(recommendGateThreshold(windows(500, -Infinity), speech(), 48_000)).toMatchObject({
      kind: 'refused',
      reason: 'silent-reference',
      summary: null,
    });
  });

  it.each([0, 49, 74])('refuses %i nonzero voice windows among 150', (nonzero) => {
    expect(
      recommendGateThreshold(
        quiet(),
        [...windows(nonzero, -30), ...windows(150 - nonzero, -Infinity)],
        48_000
      )
    ).toMatchObject({ kind: 'refused', reason: 'insufficient' });
  });

  it('accepts exactly 50 percent zero speech when the other half is useful', () => {
    expect(
      recommendGateThreshold(quiet(), [...windows(75, -30), ...windows(75, -Infinity)], 48_000)
    ).toMatchObject({ kind: 'recommended', summary: { speechZeroFraction: 0.5 } });
  });

  it.each([
    [499, 150],
    [500, 149],
  ])('refuses incomplete duration (%i room, %i voice)', (roomCount, voiceCount) => {
    expect(
      recommendGateThreshold(windows(roomCount, -60), windows(voiceCount, -30), 48_000)
    ).toMatchObject({ kind: 'refused', reason: 'insufficient' });
  });

  it.each([501, 502])('rejects %i room windows beyond the rate-derived budget', (count) => {
    expect(recommendGateThreshold(windows(count, -60), speech(), 48_000)).toMatchObject({
      kind: 'refused',
      reason: 'invalid',
    });
  });

  it('rejects speech beyond twenty seconds', () => {
    expect(recommendGateThreshold(quiet(), windows(1001, -30), 48_000)).toMatchObject({
      kind: 'refused',
      reason: 'invalid',
    });
  });

  it.each([44_100, 96_000, 22_051])('uses actual %i Hz duration and complete windows', (rate) => {
    const frames = Math.round(rate * 0.02);
    expect(
      recommendGateThreshold(
        windows(Math.ceil((rate * 10) / frames), -60, frames),
        windows(Math.ceil((rate * 3) / frames), -30, frames),
        rate
      )
    ).toMatchObject({ kind: 'recommended', thresholdDbfs: -57 });
  });

  it.each([0, -1, NaN, Infinity, 1])('refuses invalid or unsupported sample rate %s', (rate) => {
    expect(recommendGateThreshold(quiet(), speech(), rate)).toMatchObject({
      kind: 'refused',
      reason: 'invalid',
    });
  });

  it.each([
    { peak: NaN },
    { peak: Infinity },
    { peak: -0.1 },
    { frames: 959 },
    { valid: false },
    { valid: 1 },
    { overloaded: 'false' },
  ])('refuses malformed evidence %o rather than dropping it', (bad) => {
    const room = quiet();
    room[42] = { ...room[42], ...bad } as PeakWindow;
    expect(recommendGateThreshold(room, speech(), 48_000)).toMatchObject({
      kind: 'refused',
      reason: 'invalid',
    });
  });

  it.each([{ peak: 1 }, { peak: Number.MAX_VALUE }, { overloaded: true }])(
    'refuses clipping evidence %o',
    (bad) => {
      const voice = speech();
      voice[42] = { ...voice[42], ...bad };
      expect(recommendGateThreshold(quiet(), voice, 48_000)).toMatchObject({
        kind: 'refused',
        reason: 'clipping',
      });
    }
  );

  it('retains tiny finite nonzero voice as evidence against a threshold', () => {
    const voice = speech();
    voice[42].peak = Number.MIN_VALUE;
    expect(recommendGateThreshold(quiet(), voice, 48_000)).toMatchObject({
      kind: 'refused',
      reason: 'overlap',
    });
  });
});

beforeEach(() => resetAllStores());
