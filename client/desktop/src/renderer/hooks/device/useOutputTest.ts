import { useState, useRef, useCallback, useEffect } from 'react';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { voiceService } from '../../services/voice/voiceService';

interface UseOutputTestReturn {
  isTesting: boolean;
  error: string | null;
  playTestTone: () => Promise<void>;
  stopTest: () => void;
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
  const generationRef = useRef(0);

  const closeOwnedAudioContext = useCallback(async (ctx: AudioContext) => {
    if (audioContextRef.current !== ctx) return;
    audioContextRef.current = null;
    if (ctx.state !== 'closed') await ctx.close().catch(() => {});
  }, []);

  const cleanup = useCallback(() => {
    generationRef.current++;
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
      const source = audioElementRef.current.srcObject;
      if (source && 'getTracks' in source) {
        for (const track of source.getTracks()) track.stop();
      }
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
    const generation = ++generationRef.current;
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
        callSuspensionRef.current = true;
        voiceService.setLocalTestingStatus(true);
      }

      const ctx = new AudioContext({ sampleRate: 48000 });
      audioContextRef.current = ctx;
      if (ctx.state === 'suspended') await ctx.resume();
      if (generation !== generationRef.current) {
        await closeOwnedAudioContext(ctx);
        return;
      }

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
        if (generation !== generationRef.current) return;
      }
      await audioEl.play();
      if (generation !== generationRef.current) {
        audioEl.pause();
        audioEl.srcObject = null;
        await closeOwnedAudioContext(ctx);
        return;
      }

      setIsTesting(true);
      timeoutRef.current = setTimeout(
        () => {
          cleanup();
        },
        duration * 1000 + 50
      );
    } catch {
      if (generation !== generationRef.current) return;
      cleanup();
      setError('Failed to play test tone');
    }
  }, [cleanup, closeOwnedAudioContext]);

  useEffect(() => {
    const unsubscribe = useVoiceStore.subscribe((state, prev) => {
      const deviceChanged = state.audioOutputDeviceId !== prev.audioOutputDeviceId;
      const wasInCall = ['connected', 'connecting', 'reconnecting'].includes(prev.connectionState);
      const isInCall = ['connected', 'connecting', 'reconnecting'].includes(state.connectionState);
      const callConflict = state.localIsTesting && !callSuspensionRef.current;
      const callStarted = !wasInCall && isInCall && !callSuspensionRef.current;
      if (deviceChanged || callConflict || callStarted) cleanup();
    });
    return () => {
      unsubscribe();
      cleanup();
    };
  }, [cleanup]);

  return { isTesting, error, playTestTone, stopTest: cleanup };
}
