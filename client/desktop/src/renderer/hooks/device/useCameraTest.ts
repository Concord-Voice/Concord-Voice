import { useState, useRef, useCallback, useEffect } from 'react';
import { useVoiceStore } from '../../stores/voice/voiceStore';

const VOICE_CALL_CONNECTION_STATES = new Set(['connected', 'connecting', 'reconnecting']);

function isInVoiceCall(connectionState: string): boolean {
  return VOICE_CALL_CONNECTION_STATES.has(connectionState);
}

interface UseCameraTestReturn {
  isTesting: boolean;
  error: string | null;
  stream: MediaStream | null;
  toggleTest: () => Promise<void>;
  stopTest: () => void;
}

/**
 * Toggles a live camera preview for the Settings device panel. Acquires a
 * MediaStream from the currently-selected video input via getUserMedia and
 * exposes it for inline <video> rendering. Guarantees all tracks are stopped
 * on toggle-off and on unmount so no MediaStreamTracks leak.
 */
export function useCameraTest(): UseCameraTestReturn {
  const [isTesting, setIsTesting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [stream, setStream] = useState<MediaStream | null>(null);
  const streamRef = useRef<MediaStream | null>(null);
  const generationRef = useRef(0);
  const pendingRef = useRef(false);

  const stopTest = useCallback(() => {
    generationRef.current++;
    pendingRef.current = false;
    if (streamRef.current) {
      for (const t of streamRef.current.getTracks()) t.stop();
      streamRef.current = null;
    }
    setStream(null);
    setIsTesting(false);
  }, []);

  const startTest = useCallback(async (generation: number) => {
    setError(null);
    try {
      const deviceId = useVoiceStore.getState().videoDeviceId;
      const next = await navigator.mediaDevices.getUserMedia({
        video: deviceId ? { deviceId: { exact: deviceId } } : true,
      });
      if (generation !== generationRef.current) {
        for (const track of next.getTracks()) track.stop();
        return;
      }
      streamRef.current = next;
      setStream(next);
      setIsTesting(true);
    } catch (err) {
      if (generation !== generationRef.current) return;
      const msg =
        err instanceof DOMException && err.name === 'NotAllowedError'
          ? 'Camera access denied'
          : 'Failed to access camera';
      setError(msg);
      setIsTesting(false);
    } finally {
      if (generation === generationRef.current) pendingRef.current = false;
    }
  }, []);

  const toggleTest = useCallback(async () => {
    if (streamRef.current || pendingRef.current) {
      stopTest();
      return;
    }
    const generation = ++generationRef.current;
    pendingRef.current = true;
    await startTest(generation);
  }, [startTest, stopTest]);

  useEffect(() => {
    const unsubscribe = useVoiceStore.subscribe((state, prev) => {
      const callStarted =
        !isInVoiceCall(prev.connectionState) && isInVoiceCall(state.connectionState);
      if (state.videoDeviceId !== prev.videoDeviceId || callStarted) stopTest();
    });
    return () => {
      unsubscribe();
      stopTest();
    };
  }, [stopTest]);

  return { isTesting, error, stream, toggleTest, stopTest };
}
