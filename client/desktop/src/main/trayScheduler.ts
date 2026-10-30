// Bounded tray animation scheduler (plan Task 4, spec §3 / R8 / R9).
//
// One setTimeout chain at most. It plays the mention burst (7 frames at ~6 fps,
// once per 60 s) and the reconnect breath (alpha 100/70/40/70 at 400 ms, capped
// at 30 s) over a base frame key, and otherwise just lands the static frame.
// Every gate degrades to "show the final frame": an animation is never the only
// carrier of state.

import {
  animationFrameKeys,
  type AnimationKind,
  type RenderMode,
  type TrayVisual,
} from './trayVisual';

export interface SchedulerDeps {
  setFrame(mode: RenderMode, key: string): void;
  now(): number;
}

export interface MotionGate {
  reducedMotion: boolean;
  richAnimation: boolean;
  onBattery: boolean;
  dnd: boolean;
  platform: NodeJS.Platform;
}

const MIN_INTERVAL_MS = 125;
const BURST_INTERVAL_MS = 167;
const BREATH_INTERVAL_MS = 400;
const BREATH_MAX_MS = 30_000;
const BURST_MIN_GAP_MS = 60_000;

interface Running {
  id: string;
  kind: AnimationKind;
  frames: string[];
  base: string;
  idx: number;
  mode: RenderMode;
  startedAt: number;
}

/** DND gates all motion; battery gates looping motion only, so the one-shot burst ignores it (R9). */
function motionBlocked(kind: AnimationKind, gate: MotionGate): boolean {
  if (gate.platform === 'linux' || gate.reducedMotion || !gate.richAnimation || gate.dnd) {
    return true;
  }
  return kind === 'breath' && gate.onBattery;
}

export function createTrayScheduler(deps: SchedulerDeps): {
  apply(v: TrayVisual, gate: MotionGate): void;
  dispose(): void;
  activeTimers(): number;
} {
  let timer: ReturnType<typeof setTimeout> | null = null;
  let running: Running | null = null;
  /** Identity of the animation that already played/was skipped, so a re-apply doesn't replay it. */
  let settledId: string | null = null;
  let lastBurstAt: number | null = null;
  let shown: string | null = null;

  const emit = (mode: RenderMode, key: string): void => {
    const sig = `${mode}/${key}`;
    if (sig === shown) return;
    shown = sig;
    deps.setFrame(mode, key);
  };

  const clearTimer = (): void => {
    if (timer !== null) {
      clearTimeout(timer);
      timer = null;
    }
  };

  const intervalOf = (kind: AnimationKind): number =>
    Math.max(MIN_INTERVAL_MS, kind === 'burst' ? BURST_INTERVAL_MS : BREATH_INTERVAL_MS);

  const finish = (): void => {
    if (running === null) return;
    const done = running;
    running = null;
    settledId = done.id;
    emit(done.mode, done.base);
  };

  const tick = (): void => {
    timer = null;
    if (running === null) return;
    const r = running;
    if (r.kind === 'burst') {
      r.idx += 1;
      if (r.idx >= r.frames.length) return finish();
    } else {
      if (deps.now() - r.startedAt >= BREATH_MAX_MS) return finish();
      r.idx = (r.idx + 1) % r.frames.length;
    }
    emit(r.mode, r.frames[r.idx] ?? r.base);
    schedule();
  };

  const schedule = (): void => {
    clearTimer();
    if (running === null) return;
    timer = setTimeout(tick, intervalOf(running.kind));
  };

  const stopAndShow = (v: TrayVisual): void => {
    clearTimer();
    running = null;
    emit(v.mode, v.frameKey);
  };

  /** The same animation is already running: stop it if gated, else restyle it. */
  const continueRunning = (run: Running, v: TrayVisual, gate: MotionGate): void => {
    if (motionBlocked(run.kind, gate)) {
      // Breath may resume when the gate lifts; a burst never replays.
      settledId = run.kind === 'burst' ? run.id : null;
      stopAndShow(v);
      return;
    }
    if (run.mode !== v.mode) {
      // Restyle: same frame index, new mode, next tick a full interval away.
      run.mode = v.mode;
      emit(run.mode, run.frames[run.idx] ?? run.base);
      schedule();
    }
  };

  const burstTooSoon = (): boolean =>
    lastBurstAt !== null && deps.now() - lastBurstAt < BURST_MIN_GAP_MS;

  return {
    apply(v, gate) {
      const id = v.animation === null ? null : `${v.animation}:${v.frameKey}`;

      if (running !== null && running.id === id) {
        continueRunning(running, v, gate);
        return;
      }

      clearTimer();
      running = null;

      if (id === null || v.animation === null) {
        settledId = null;
        emit(v.mode, v.frameKey);
        return;
      }
      if (id === settledId) {
        emit(v.mode, v.frameKey);
        return;
      }

      const frames = animationFrameKeys(v.frameKey, v.animation);
      const tooSoon = v.animation === 'burst' && burstTooSoon();
      if (frames === null || motionBlocked(v.animation, gate) || tooSoon) {
        settledId = v.animation === 'burst' ? id : null;
        emit(v.mode, v.frameKey);
        return;
      }

      const startedAt = deps.now();
      if (v.animation === 'burst') lastBurstAt = startedAt;
      running = {
        id,
        kind: v.animation,
        frames,
        base: v.frameKey,
        idx: 0,
        mode: v.mode,
        startedAt,
      };
      emit(v.mode, frames[0] ?? v.frameKey);
      schedule();
    },

    dispose() {
      clearTimer();
      running = null;
      settledId = null;
      lastBurstAt = null;
      shown = null;
    },

    activeTimers() {
      return timer === null ? 0 : 1;
    },
  };
}
