import { useState, useRef, useCallback, useEffect } from 'react';
import { useAudioSettingsStore } from '../stores/audioSettingsStore';
import { useVoiceStore } from '../stores/voiceStore';
import { ensureOsPermission } from '../stores/osPermissionStore';

interface UseMicTestReturn {
  isTesting: boolean;
  dbfsLevel: number;
  error: string | null;
  startTest: () => Promise<void>;
  stopTest: () => void;
}

/**
 * Microphone test hook — captures mic, runs it through the same processing chain
 * as a real voice call (noise cancellation, echo cancellation, AGC, noise gate,
 * input volume), plays back through the selected output device, and provides a
 * live dBFS meter reading.
 */
export function useMicTest(): UseMicTestReturn {
  const [isTesting, setIsTesting] = useState(false);
  const [dbfsLevel, setDbfsLevel] = useState(-Infinity);
  const [error, setError] = useState<string | null>(null);

  // Refs for audio resources (not state — avoids re-renders)
  const audioContextRef = useRef<AudioContext | null>(null);
  const micStreamRef = useRef<MediaStream | null>(null);
  const audioElementRef = useRef<HTMLAudioElement | null>(null);
  const gainNodeRef = useRef<GainNode | null>(null);
  const noiseGateGainRef = useRef<GainNode | null>(null);
  const noiseGateAnalyserRef = useRef<AnalyserNode | null>(null);
  const meterAnalyserRef = useRef<AnalyserNode | null>(null);
  const noiseGateTimerRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const meterRafRef = useRef<number | null>(null);
  const settingsUnsubRef = useRef<(() => void) | null>(null);
  const deviceUnsubRef = useRef<(() => void) | null>(null);
  const noiseGateThresholdRef = useRef(-50);
  const isRestartingRef = useRef(false);
  const isTestingRef = useRef(false);

  const stopTest = useCallback(() => {
    isTestingRef.current = false;

    // Stop meter polling
    if (meterRafRef.current != null) {
      cancelAnimationFrame(meterRafRef.current);
      meterRafRef.current = null;
    }

    // Stop noise gate polling
    if (noiseGateTimerRef.current != null) {
      clearInterval(noiseGateTimerRef.current);
      noiseGateTimerRef.current = null;
    }

    // Stop audio playback
    if (audioElementRef.current) {
      audioElementRef.current.pause();
      audioElementRef.current.srcObject = null;
      audioElementRef.current = null;
    }

    // Close AudioContext
    if (audioContextRef.current && audioContextRef.current.state !== 'closed') {
      audioContextRef.current.close().catch(() => {});
    }
    audioContextRef.current = null;

    // Stop mic stream tracks
    if (micStreamRef.current) {
      for (const t of micStreamRef.current.getTracks()) t.stop();
      micStreamRef.current = null;
    }

    // Unsubscribe from stores
    settingsUnsubRef.current?.();
    settingsUnsubRef.current = null;
    deviceUnsubRef.current?.();
    deviceUnsubRef.current = null;

    // Clear node refs
    gainNodeRef.current = null;
    noiseGateGainRef.current = null;
    noiseGateAnalyserRef.current = null;
    meterAnalyserRef.current = null;

    setIsTesting(false);
    setDbfsLevel(-Infinity);
    setError(null);
  }, []);

  const startTest = useCallback(async () => {
    // Idempotent: stop any existing test first
    stopTest();

    const adv = useAudioSettingsStore.getState();
    const voiceState = useVoiceStore.getState();
    const useProcessing = !adv.musicMode;

    try {
      // JIT permission check (#197): request mic access on macOS before getUserMedia
      const micStatus = await ensureOsPermission('microphone');
      if (micStatus !== 'granted') {
        setError(
          'Microphone access denied. Grant permission in System Settings > Privacy & Security.'
        );
        return;
      }

      // 1. Acquire mic stream
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          deviceId: voiceState.audioInputDeviceId
            ? { exact: voiceState.audioInputDeviceId }
            : undefined,
          echoCancellation: useProcessing && adv.echoCancellation,
          noiseSuppression: useProcessing && adv.noiseCancellation,
          autoGainControl: useProcessing && adv.autoGainControl,
          sampleRate: 48000,
          channelCount: 2,
        },
      });
      micStreamRef.current = stream;

      // 2. Build AudioContext
      const ctx = new AudioContext({ sampleRate: 48000 });
      audioContextRef.current = ctx;
      if (ctx.state === 'suspended') await ctx.resume();

      const source = ctx.createMediaStreamSource(stream);
      let currentNode: AudioNode = source;

      // 3. Noise gate (manual mode only) — mirrors voiceService.applyNoiseGate()
      if (adv.noiseGateMode === 'manual') {
        const gateAnalyser = ctx.createAnalyser();
        gateAnalyser.fftSize = 256;
        const gateGain = ctx.createGain();
        noiseGateAnalyserRef.current = gateAnalyser;
        noiseGateGainRef.current = gateGain;
        noiseGateThresholdRef.current = adv.noiseGateLevel;

        currentNode.connect(gateAnalyser);
        gateAnalyser.connect(gateGain);
        currentNode = gateGain;

        // Poll at 50Hz matching voiceService
        const dataArray = new Uint8Array(gateAnalyser.frequencyBinCount);
        noiseGateTimerRef.current = setInterval(() => {
          if (!noiseGateAnalyserRef.current) return;
          noiseGateAnalyserRef.current.getByteTimeDomainData(dataArray);
          let peak = 0;
          for (const sample of dataArray) {
            const offset = Math.abs(sample - 128);
            if (offset > peak) peak = offset;
          }
          const thresholdAmplitude = 128 * Math.pow(10, noiseGateThresholdRef.current / 20);
          const isOpen = peak >= thresholdAmplitude;
          gateGain.gain.setTargetAtTime(isOpen ? 1 : 0, ctx.currentTime, 0.015);
        }, 20);
      }

      // 4. Input volume GainNode — mirrors voiceService.applyInputVolume()
      const volumeGain = ctx.createGain();
      volumeGain.gain.value = adv.inputVolume / 100;
      gainNodeRef.current = volumeGain;
      currentNode.connect(volumeGain);
      currentNode = volumeGain;

      // 5. Meter AnalyserNode (post-processing, read-only)
      const meterAnalyser = ctx.createAnalyser();
      meterAnalyser.fftSize = 2048;
      meterAnalyser.smoothingTimeConstant = 0.4;
      meterAnalyserRef.current = meterAnalyser;
      currentNode.connect(meterAnalyser);

      // 6. Destination → <audio> element for loopback playback
      const destination = ctx.createMediaStreamDestination();
      meterAnalyser.connect(destination);

      const audioEl = new Audio();
      audioEl.srcObject = destination.stream;
      audioElementRef.current = audioEl;

      const outputDeviceId = voiceState.audioOutputDeviceId;
      if (outputDeviceId && 'setSinkId' in audioEl) {
        // Chrome-exclusive API not in the stock HTMLAudioElement lib types.
        // Widening to a minimal interface that declares only the field we
        // use avoids `any` while keeping the guard above as the safety net.
        await (audioEl as HTMLAudioElement & { setSinkId(id: string): Promise<void> }).setSinkId(
          outputDeviceId
        );
      }
      await audioEl.play();

      // 7. Start dBFS meter polling via requestAnimationFrame
      const meterData = new Float32Array(meterAnalyser.fftSize);
      const pollMeter = () => {
        if (!meterAnalyserRef.current) return;
        meterAnalyserRef.current.getFloatTimeDomainData(meterData);
        let peak = 0;
        for (const sample of meterData) {
          const abs = Math.abs(sample);
          if (abs > peak) peak = abs;
        }
        const dbfs = peak > 0 ? Math.max(-80, 20 * Math.log10(peak)) : -80;
        setDbfsLevel(dbfs);
        meterRafRef.current = requestAnimationFrame(pollMeter);
      };
      meterRafRef.current = requestAnimationFrame(pollMeter);

      // 8. Subscribe to real-time settings changes
      settingsUnsubRef.current = useAudioSettingsStore.subscribe((state, prev) => {
        // Input volume: instant GainNode update
        if (
          state.inputVolume !== prev.inputVolume &&
          gainNodeRef.current &&
          audioContextRef.current &&
          audioContextRef.current.state !== 'closed'
        ) {
          gainNodeRef.current.gain.setTargetAtTime(
            state.inputVolume / 100,
            audioContextRef.current.currentTime,
            0.01
          );
        }

        // Noise gate threshold: update ref (polling loop reads it)
        if (state.noiseGateLevel !== prev.noiseGateLevel) {
          noiseGateThresholdRef.current = state.noiseGateLevel;
        }

        // Constraint-level changes: requires full restart
        const needsRestart =
          state.noiseCancellation !== prev.noiseCancellation ||
          state.echoCancellation !== prev.echoCancellation ||
          state.autoGainControl !== prev.autoGainControl ||
          state.noiseGateMode !== prev.noiseGateMode ||
          state.musicMode !== prev.musicMode;

        if (needsRestart && !isRestartingRef.current) {
          isRestartingRef.current = true;
          // Defer to avoid calling startTest within the subscription synchronously
          setTimeout(async () => {
            if (isTestingRef.current) {
              await startTest();
            }
            isRestartingRef.current = false;
          }, 0);
        }
      });

      // 9. Subscribe to device changes
      deviceUnsubRef.current = useVoiceStore.subscribe((state, prev) => {
        if (
          (state.audioInputDeviceId !== prev.audioInputDeviceId ||
            state.audioOutputDeviceId !== prev.audioOutputDeviceId) &&
          !isRestartingRef.current
        ) {
          isRestartingRef.current = true;
          setTimeout(async () => {
            if (isTestingRef.current) {
              await startTest();
            }
            isRestartingRef.current = false;
          }, 0);
        }
      });

      isTestingRef.current = true;
      setIsTesting(true);
      setError(null);
    } catch (err) {
      stopTest();
      const msg =
        err instanceof DOMException && err.name === 'NotAllowedError'
          ? 'Microphone access denied'
          : 'Failed to access microphone';
      setError(msg);
    }
  }, [stopTest]);

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      stopTest();
    };
  }, [stopTest]);

  return { isTesting, dbfsLevel, error, startTest, stopTest };
}
