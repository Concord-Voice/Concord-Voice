import { useRef, useEffect, useState } from 'react';
import type { VoiceParticipant } from '../../stores/voiceStore';

// Voice-triggered magnification (subtler than server bar's 40%)
export const VOICE_MAX_SCALE = 1.12; // 12% magnification
const RAMP_UP_MS = 120; // fast attack
const RAMP_DOWN_MS = 300; // slower release

/** Compute the next scale value for a single participant. */
function nextScale(
  prev: number,
  isSpeaking: boolean,
  dt: number
): { value: number; changed: boolean } {
  const target = isSpeaking ? VOICE_MAX_SCALE : 1;

  if (Math.abs(prev - target) < 0.001) {
    return { value: target, changed: prev !== target };
  }

  const rampMs = target > prev ? RAMP_UP_MS : RAMP_DOWN_MS;
  const alpha = Math.min(1, dt / rampMs);
  return { value: prev + (target - prev) * alpha, changed: true };
}

/**
 * Returns a Record<userId, scale> that smoothly animates between 1.0 and 1.12
 * based on each participant's isSpeaking state. Uses requestAnimationFrame
 * with asymmetric ramp rates (fast attack, slow release).
 */
export function useVoiceMagnification(
  participants: Record<string, VoiceParticipant>
): Record<string, number> {
  const [scales, setScales] = useState<Record<string, number>>({});
  const currentScalesRef = useRef<Record<string, number>>({});
  const rafRef = useRef<number>(0);
  const lastTimeRef = useRef<number>(0);

  useEffect(() => {
    let running = true;

    const animate = (now: number) => {
      if (!running) return;

      const dt = lastTimeRef.current ? now - lastTimeRef.current : 16;
      lastTimeRef.current = now;

      const current = currentScalesRef.current;
      let changed = false;
      const next: Record<string, number> = {};

      for (const [userId, p] of Object.entries(participants)) {
        const prev = current[userId] ?? 1;
        const result = nextScale(prev, p.isSpeaking, dt);
        next[userId] = result.value;
        if (result.changed) changed = true;
      }

      // Clean up entries for participants that left
      for (const userId of Object.keys(current)) {
        if (!(userId in participants)) {
          changed = true;
        }
      }

      if (changed) {
        currentScalesRef.current = next;
        setScales({ ...next });
      } else {
        currentScalesRef.current = next;
      }

      rafRef.current = requestAnimationFrame(animate);
    };

    rafRef.current = requestAnimationFrame(animate);

    return () => {
      running = false;
      cancelAnimationFrame(rafRef.current);
    };
  }, [participants]);

  return scales;
}
