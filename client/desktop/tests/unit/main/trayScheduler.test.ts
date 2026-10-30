import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { createTrayScheduler, type MotionGate } from '../../../src/main/trayScheduler';
import type { RenderMode, TrayVisual } from '../../../src/main/trayVisual';

interface Call {
  mode: RenderMode;
  key: string;
  at: number;
}

const gate: MotionGate = {
  reducedMotion: false,
  richAnimation: true,
  onBattery: false,
  dnd: false,
  platform: 'darwin',
};
const g = (o: Partial<MotionGate> = {}): MotionGate => ({ ...gate, ...o });

const burstV = (mode: RenderMode = 'colour'): TrayVisual => ({
  mode,
  frameKey: 'full.full.discmention',
  animation: 'burst',
  tooltip: '',
  menu: [],
  menuKey: '',
});
const breathV = (mode: RenderMode = 'colour'): TrayVisual => ({
  mode,
  frameKey: 'amber.full.dim35',
  animation: 'breath',
  tooltip: '',
  menu: [],
  menuKey: '',
});
const staticV = (key = 'full.full.star', mode: RenderMode = 'colour'): TrayVisual => ({
  mode,
  frameKey: key,
  animation: null,
  tooltip: '',
  menu: [],
  menuKey: '',
});

let calls: Call[];
function make() {
  calls = [];
  return createTrayScheduler({
    setFrame: (mode, key) => calls.push({ mode, key, at: Date.now() }),
    now: () => Date.now(),
  });
}
const keys = () => calls.map((c) => c.key);

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(1_000_000);
});
afterEach(() => {
  vi.useRealTimers();
});

describe('burst', () => {
  it('emits 7 burst frames then the base, total <= 1.2 s', () => {
    const s = make();
    s.apply(burstV(), gate);
    expect(keys()).toEqual(['full.burst1.discmention']);
    expect(s.activeTimers()).toBe(1);
    vi.advanceTimersByTime(1200);
    expect(keys()).toEqual([
      ...[1, 2, 3, 4, 5, 6, 7].map((i) => `full.burst${i}.discmention`),
      'full.full.discmention',
    ]);
    expect(calls[calls.length - 1].at - calls[0].at).toBeLessThanOrEqual(1200);
    expect(s.activeTimers()).toBe(0);
  });

  it('a second mention within 60 s does not burst; after 60 s it does', () => {
    const s = make();
    s.apply(burstV(), gate);
    vi.advanceTimersByTime(1500);
    s.apply(staticV('full.full.star'), gate); // mention cleared
    calls.length = 0;
    vi.advanceTimersByTime(10_000);
    s.apply(burstV(), gate);
    expect(keys()).toEqual(['full.full.discmention']);
    expect(s.activeTimers()).toBe(0);
    s.apply(staticV('full.full.star'), gate);
    vi.advanceTimersByTime(60_000);
    calls.length = 0;
    s.apply(burstV(), gate);
    expect(keys()).toEqual(['full.burst1.discmention']);
  });

  it('re-applying the same finished burst does not replay it', () => {
    const s = make();
    s.apply(burstV(), gate);
    vi.advanceTimersByTime(1500);
    calls.length = 0;
    vi.advanceTimersByTime(70_000);
    s.apply(burstV(), gate);
    expect(keys()).toEqual([]);
    expect(s.activeTimers()).toBe(0);
  });

  it.each([
    ['reducedMotion', { reducedMotion: true }],
    ['!richAnimation', { richAnimation: false }],
    ['dnd', { dnd: true }],
    ['linux', { platform: 'linux' as const }],
  ])('%s snaps to the final frame', (_n, o) => {
    const s = make();
    s.apply(burstV(), g(o));
    expect(keys()).toEqual(['full.full.discmention']);
    expect(s.activeTimers()).toBe(0);
  });

  it('battery does not gate the burst', () => {
    const s = make();
    s.apply(burstV(), g({ onBattery: true }));
    expect(keys()).toEqual(['full.burst1.discmention']);
  });

  it('a gate turning on mid-burst snaps to the final frame and stops', () => {
    const s = make();
    s.apply(burstV(), gate);
    vi.advanceTimersByTime(340);
    s.apply(burstV(), g({ reducedMotion: true }));
    expect(keys()[keys().length - 1]).toBe('full.full.discmention');
    expect(s.activeTimers()).toBe(0);
  });

  it('a state change mid-burst drops the animation', () => {
    const s = make();
    s.apply(burstV(), gate);
    s.apply(staticV('full.full.star'), gate);
    expect(keys()[keys().length - 1]).toBe('full.full.star');
    expect(s.activeTimers()).toBe(0);
  });
});

describe('breath', () => {
  it('cycles 100/70/40/70 at 400 ms and stops on the base after 30 s', () => {
    const s = make();
    s.apply(breathV(), gate);
    vi.advanceTimersByTime(1600);
    expect(keys()).toEqual([
      'amber.full.dim35',
      'amber.breath70.dim35',
      'amber.breath40.dim35',
      'amber.breath70.dim35',
      'amber.full.dim35',
    ]);
    vi.advanceTimersByTime(29_000);
    expect(s.activeTimers()).toBe(0);
    const last = calls[calls.length - 1];
    expect(last.key).toBe('amber.full.dim35');
    expect(last.at - calls[0].at).toBe(30_000);
    const n = calls.length;
    vi.advanceTimersByTime(5000);
    expect(calls.length).toBe(n);
  });

  it('does not restart after the cap while the same state persists', () => {
    const s = make();
    s.apply(breathV(), gate);
    vi.advanceTimersByTime(31_000);
    const n = calls.length;
    s.apply(breathV(), gate);
    expect(calls.length).toBe(n);
    expect(s.activeTimers()).toBe(0);
  });

  it.each([
    ['reducedMotion', { reducedMotion: true }],
    ['!richAnimation', { richAnimation: false }],
    ['onBattery', { onBattery: true }],
    ['dnd', { dnd: true }],
    ['linux', { platform: 'linux' as const }],
  ])('%s snaps to the final frame', (_n, o) => {
    const s = make();
    s.apply(breathV(), g(o));
    expect(keys()).toEqual(['amber.full.dim35']);
    expect(s.activeTimers()).toBe(0);
  });

  it('going on battery mid-breath stops it; returning to AC resumes', () => {
    const s = make();
    s.apply(breathV(), gate);
    vi.advanceTimersByTime(500);
    s.apply(breathV(), g({ onBattery: true }));
    expect(keys()[keys().length - 1]).toBe('amber.full.dim35');
    expect(s.activeTimers()).toBe(0);
    s.apply(breathV(), gate);
    expect(s.activeTimers()).toBe(1);
  });
});

describe('scheduler invariants', () => {
  it('restyle mid-animation keeps frame index, swaps mode', () => {
    const s = make();
    s.apply(burstV('colour'), gate);
    vi.advanceTimersByTime(167 * 2); // burst3 on screen
    expect(keys()[keys().length - 1]).toBe('full.burst3.discmention');
    s.apply(burstV('white'), gate);
    expect(calls[calls.length - 1]).toMatchObject({
      mode: 'white',
      key: 'full.burst3.discmention',
    });
    vi.advanceTimersByTime(167);
    expect(calls[calls.length - 1]).toMatchObject({
      mode: 'white',
      key: 'full.burst4.discmention',
    });
    vi.advanceTimersByTime(1200);
    expect(calls[calls.length - 1]).toMatchObject({ mode: 'white', key: 'full.full.discmention' });
  });

  it('applying an identical visual again is a no-op', () => {
    const s = make();
    s.apply(staticV(), gate);
    s.apply(staticV(), gate);
    expect(calls).toHaveLength(1);
  });

  it('setFrame is skipped when (mode,key) is unchanged but a mode change emits', () => {
    const s = make();
    s.apply(staticV('full.full.star', 'colour'), gate);
    s.apply(staticV('full.full.star', 'white'), gate);
    expect(calls.map((c) => c.mode)).toEqual(['colour', 'white']);
  });

  it('timer-driven setFrame calls are never closer than 125 ms (burst and breath)', () => {
    const s = make();
    s.apply(burstV(), gate);
    vi.advanceTimersByTime(2000);
    s.apply(staticV(), gate);
    s.apply(breathV(), gate);
    vi.advanceTimersByTime(3000);
    const timerDriven = calls.filter((_, i) => i > 0 && calls[i].key !== 'full.full.star');
    expect(timerDriven.length).toBeGreaterThan(10);
    for (let i = 1; i < calls.length; i++) {
      if (
        calls[i].key === 'full.full.star' ||
        (calls[i].key === 'amber.full.dim35' && calls[i - 1].key === 'full.full.star')
      )
        continue;
      expect(calls[i].at - calls[i - 1].at).toBeGreaterThanOrEqual(125);
    }
  });

  it('dispose() clears the timer and resets state', () => {
    const s = make();
    s.apply(breathV(), gate);
    expect(s.activeTimers()).toBe(1);
    s.dispose();
    expect(s.activeTimers()).toBe(0);
    const n = calls.length;
    vi.advanceTimersByTime(5000);
    expect(calls.length).toBe(n);
    s.apply(breathV(), gate);
    expect(s.activeTimers()).toBe(1);
  });

  it('a non-animatable base key falls back to the static frame', () => {
    const s = make();
    s.apply({ ...burstV(), frameKey: 'full.full.disc' }, gate);
    expect(keys()).toEqual(['full.full.disc']);
    expect(s.activeTimers()).toBe(0);
  });
});
