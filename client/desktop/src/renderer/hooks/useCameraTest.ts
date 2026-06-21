import { useState, useRef, useCallback, useEffect } from 'react';
import { useVoiceStore } from '../stores/voiceStore';

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

  const stopTest = useCallback(() => {
    if (streamRef.current) {
      for (const t of streamRef.current.getTracks()) t.stop();
      streamRef.current = null;
    }
    setStream(null);
    setIsTesting(false);
  }, []);

  const startTest = useCallback(async () => {
    setError(null);
    try {
      const deviceId = useVoiceStore.getState().videoDeviceId;
      const next = await navigator.mediaDevices.getUserMedia({
        video: deviceId ? { deviceId: { exact: deviceId } } : true,
      });
      streamRef.current = next;
      setStream(next);
      setIsTesting(true);
    } catch (err) {
      const msg =
        err instanceof DOMException && err.name === 'NotAllowedError'
          ? 'Camera access denied'
          : 'Failed to access camera';
      setError(msg);
      setIsTesting(false);
    }
  }, []);

  const toggleTest = useCallback(async () => {
    if (streamRef.current) {
      stopTest();
      return;
    }
    await startTest();
  }, [startTest, stopTest]);

  useEffect(() => {
    return () => {
      if (streamRef.current) {
        for (const t of streamRef.current.getTracks()) t.stop();
        streamRef.current = null;
      }
    };
  }, []);

  return { isTesting, error, stream, toggleTest, stopTest };
}
