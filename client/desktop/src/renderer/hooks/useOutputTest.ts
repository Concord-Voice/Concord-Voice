import { useState, useRef, useCallback, useEffect } from 'react';
import { useVoiceStore } from '../stores/voiceStore';
import { voiceService } from '../services/voiceService';

interface UseOutputTestReturn {
  isTesting: boolean;
  error: string | null;
  playTestTone: () => Promise<void>;
}

/**
 * Plays a short 440Hz sine test tone (~600ms with fade in/out) through the
 * currently-selected audio output device. Uses setSinkId to route the tone
 * to the chosen speaker; falls back to the system default when setSinkId is
 * unavailable (older Chromium) or rejects.
 *
 * The tone is produced via a MediaStreamAudioDestinationNode so that a plain
 * <audio> element can set its sink.
 */
export function useOutputTest(): UseOutputTestReturn {
  const [isTesting, setIsTesting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const audioContextRef = useRef<AudioContext | null>(null);
  const audioElementRef = useRef<HTMLAudioElement | null>(null);
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const callSuspensionRef = useRef(false);

  const cleanup = useCallback(() => {
    if (callSuspensionRef.current) {
      voiceService.endTestSuspension();
      voiceService.setLocalTestingStatus(false);
      callSuspensionRef.current = false;
    }
    if (timeoutRef.current) {
      clearTimeout(timeoutRef.current);
      timeoutRef.current = null;
    }
    if (audioElementRef.current) {
      audioElementRef.current.pause();
      audioElementRef.current.srcObject = null;
      audioElementRef.current = null;
    }
    if (audioContextRef.current && audioContextRef.current.state !== 'closed') {
      audioContextRef.current.close().catch(() => {});
    }
    audioContextRef.current = null;
    setIsTesting(false);
  }, []);

  const playTestTone = useCallback(async () => {
    // Idempotent: stop any existing tone before starting
    cleanup();
    setError(null);

    try {
      const voiceState = useVoiceStore.getState();
      const inVoiceCall =
        voiceState.connectionState === 'connected' ||
        voiceState.connectionState === 'connecting' ||
        voiceState.connectionState === 'reconnecting';
      if (inVoiceCall && voiceState.localIsTesting) {
        setError('Another audio test is already running');
        return;
      }
      if (inVoiceCall) {
        voiceService.beginTestSuspension();
        voiceService.setLocalTestingStatus(true);
        callSuspensionRef.current = true;
      }

      const ctx = new AudioContext({ sampleRate: 48000 });
      audioContextRef.current = ctx;
      if (ctx.state === 'suspended') await ctx.resume();

      const destination = ctx.createMediaStreamDestination();

      const osc = ctx.createOscillator();
      osc.type = 'sine';
      osc.frequency.value = 440;

      const gain = ctx.createGain();
      const now = ctx.currentTime;
      // Linear fade-in / fade-out over 80ms each, 600ms total
      const duration = 0.6;
      const fade = 0.08;
      gain.gain.setValueAtTime(0, now);
      gain.gain.linearRampToValueAtTime(0.2, now + fade);
      gain.gain.linearRampToValueAtTime(0.2, now + duration - fade);
      gain.gain.linearRampToValueAtTime(0, now + duration);

      osc.connect(gain);
      gain.connect(destination);
      osc.start(now);
      osc.stop(now + duration);

      const audioEl = new Audio();
      audioEl.srcObject = destination.stream;
      audioElementRef.current = audioEl;

      const outputDeviceId = useVoiceStore.getState().audioOutputDeviceId;
      if (outputDeviceId && 'setSinkId' in audioEl) {
        try {
          await (audioEl as unknown as { setSinkId: (id: string) => Promise<void> }).setSinkId(
            outputDeviceId
          );
        } catch {
          // setSinkId rejected — fall back to default sink
        }
      }
      await audioEl.play();

      setIsTesting(true);
      timeoutRef.current = setTimeout(
        () => {
          cleanup();
        },
        duration * 1000 + 50
      );
    } catch (err) {
      cleanup();
      const msg = err instanceof Error ? err.message : 'Failed to play test tone';
      setError(msg);
    }
  }, [cleanup]);

  useEffect(() => {
    return () => {
      cleanup();
    };
  }, [cleanup]);

  return { isTesting, error, playTestTone };
}
