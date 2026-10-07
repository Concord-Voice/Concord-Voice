import { useState, useRef, useCallback, useEffect } from 'react';
import {
  effectiveMicLevelPercent,
  useAudioSettingsStore,
} from '../../stores/audio/audioSettingsStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { ensureOsPermission } from '../../stores/voice/osPermissionStore';
import { voiceService } from '../../services/voice/voiceService';

interface UseMicTestReturn {
  isTesting: boolean;
  dbfsLevel: number;
  error: string | null;
  startTest: () => Promise<void>;
  stopTest: () => void;
}

interface StopTestOptions {
  keepCallSuspension?: boolean;
}

type MicTestAudioSettings = ReturnType<typeof useAudioSettingsStore.getState>;
type MicTestVoiceState = ReturnType<typeof useVoiceStore.getState>;

const VOICE_CALL_CONNECTION_STATES = new Set(['connected', 'connecting', 'reconnecting']);

function isInVoiceCall(connectionState: string): boolean {
  return VOICE_CALL_CONNECTION_STATES.has(connectionState);
}

function stopStreamTracks(stream: Pick<MediaStream, 'getTracks'>): void {
  for (const track of stream.getTracks()) track.stop();
}

function stopAudioElement(audioElement: HTMLAudioElement): void {
  audioElement.pause();
  const source = audioElement.srcObject;
  if (source && 'getTracks' in source) stopStreamTracks(source);
  audioElement.srcObject = null;
}

function endCallTestSuspension(): void {
  voiceService.endTestSuspension();
  voiceService.setLocalTestingStatus(false);
}

function getMicAccessErrorMessage(err: unknown): string {
  if (err instanceof DOMException && err.name === 'NotAllowedError') {
    return 'Microphone access denied';
  }
  return 'Failed to access microphone';
}

function buildMicConstraints(
  adv: MicTestAudioSettings,
  voiceState: MicTestVoiceState,
  useProcessing: boolean
): MediaTrackConstraints {
  return {
    deviceId: voiceState.audioInputDeviceId ? { exact: voiceState.audioInputDeviceId } : undefined,
    echoCancellation: useProcessing && adv.echoCancellation,
    noiseSuppression: useProcessing && adv.noiseCancellation,
    autoGainControl: useProcessing && adv.autoGainControl,
    sampleRate: 48000,
    channelCount: 2,
  };
}

function getBytePeak(dataArray: Uint8Array): number {
  let peak = 0;
  for (const sample of dataArray) {
    const offset = Math.abs(sample - 128);
    if (offset > peak) peak = offset;
  }
  return peak;
}

function getFloatPeak(dataArray: Float32Array): number {
  let peak = 0;
  for (const sample of dataArray) {
    const abs = Math.abs(sample);
    if (abs > peak) peak = abs;
  }
  return peak;
}

function shouldRestartForSettings(
  state: MicTestAudioSettings,
  prev: MicTestAudioSettings
): boolean {
  return (
    state.noiseCancellation !== prev.noiseCancellation ||
    state.echoCancellation !== prev.echoCancellation ||
    state.autoGainControl !== prev.autoGainControl ||
    state.noiseGateMode !== prev.noiseGateMode ||
    state.musicMode !== prev.musicMode
  );
}

function shouldRestartForDevices(state: MicTestVoiceState, prev: MicTestVoiceState): boolean {
  return (
    state.audioInputDeviceId !== prev.audioInputDeviceId ||
    state.audioOutputDeviceId !== prev.audioOutputDeviceId
  );
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
  const callSuspensionRef = useRef(false);
  const generationRef = useRef(0);
  const pendingRef = useRef(false);
  const restartTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const startTestRef = useRef<() => Promise<void>>(async () => {});

  const releaseCallTestSuspension = useCallback(() => {
    if (!callSuspensionRef.current) return;
    endCallTestSuspension();
    callSuspensionRef.current = false;
  }, []);

  const stopTest = useCallback(
    (options: StopTestOptions = {}) => {
      generationRef.current++;
      pendingRef.current = false;
      isTestingRef.current = false;
      if (!options.keepCallSuspension) releaseCallTestSuspension();

      if (restartTimeoutRef.current != null) {
        clearTimeout(restartTimeoutRef.current);
        restartTimeoutRef.current = null;
      }
      isRestartingRef.current = false;

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
        stopAudioElement(audioElementRef.current);
        audioElementRef.current = null;
      }

      // Close AudioContext
      if (audioContextRef.current && audioContextRef.current.state !== 'closed') {
        audioContextRef.current.close().catch(() => {});
      }
      audioContextRef.current = null;

      // Stop mic stream tracks
      if (micStreamRef.current) {
        stopStreamTracks(micStreamRef.current);
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
    },
    [releaseCallTestSuspension]
  );

  const ensureMicPermission = useCallback(async (generation: number): Promise<boolean> => {
    const micStatus = await ensureOsPermission('microphone');
    if (generation !== generationRef.current) return false;
    if (micStatus === 'granted') return true;

    setError('Microphone access denied. Grant permission in System Settings > Privacy & Security.');
    return false;
  }, []);

  const ensureCallSuspensionForTest = useCallback((inVoiceCall: boolean) => {
    if (!inVoiceCall || callSuspensionRef.current) return;
    voiceService.beginTestSuspension();
    callSuspensionRef.current = true;
    voiceService.setLocalTestingStatus(true);
  }, []);

  const createRunningAudioContext = useCallback(async (generation: number) => {
    const ctx = new AudioContext({ sampleRate: 48000 });
    audioContextRef.current = ctx;
    if (ctx.state === 'suspended') await ctx.resume();
    if (generation !== generationRef.current) {
      if (audioContextRef.current === ctx) {
        audioContextRef.current = null;
        if (ctx.state !== 'closed') await ctx.close().catch(() => {});
      }
      return null;
    }
    return ctx;
  }, []);

  const connectNoiseGate = useCallback(
    (ctx: AudioContext, currentNode: AudioNode, adv: MicTestAudioSettings): AudioNode => {
      if (adv.noiseGateMode !== 'manual') return currentNode;

      const gateAnalyser = ctx.createAnalyser();
      gateAnalyser.fftSize = 256;
      const gateGain = ctx.createGain();
      noiseGateAnalyserRef.current = gateAnalyser;
      noiseGateGainRef.current = gateGain;
      noiseGateThresholdRef.current = adv.noiseGateLevel;

      currentNode.connect(gateAnalyser);
      gateAnalyser.connect(gateGain);

      const dataArray = new Uint8Array(gateAnalyser.frequencyBinCount);
      noiseGateTimerRef.current = setInterval(() => {
        if (!noiseGateAnalyserRef.current) return;
        noiseGateAnalyserRef.current.getByteTimeDomainData(dataArray);
        const thresholdAmplitude = 128 * Math.pow(10, noiseGateThresholdRef.current / 20);
        const isOpen = getBytePeak(dataArray) >= thresholdAmplitude;
        gateGain.gain.setTargetAtTime(isOpen ? 1 : 0, ctx.currentTime, 0.015);
      }, 20);

      return gateGain;
    },
    []
  );

  const connectInputVolume = useCallback(
    (ctx: AudioContext, currentNode: AudioNode, adv: MicTestAudioSettings): AudioNode => {
      const volumeGain = ctx.createGain();
      volumeGain.gain.value = effectiveMicLevelPercent(adv) / 100;
      gainNodeRef.current = volumeGain;
      currentNode.connect(volumeGain);
      return volumeGain;
    },
    []
  );

  const connectMeter = useCallback((ctx: AudioContext, currentNode: AudioNode): AnalyserNode => {
    const meterAnalyser = ctx.createAnalyser();
    meterAnalyser.fftSize = 2048;
    meterAnalyser.smoothingTimeConstant = 0.4;
    meterAnalyserRef.current = meterAnalyser;
    currentNode.connect(meterAnalyser);
    return meterAnalyser;
  }, []);

  const createLoopbackAudio = useCallback(
    async (
      ctx: AudioContext,
      meterAnalyser: AnalyserNode,
      outputDeviceId: string | null,
      generation: number
    ): Promise<boolean> => {
      const destination = ctx.createMediaStreamDestination();
      meterAnalyser.connect(destination);

      const audioEl = new Audio();
      audioEl.srcObject = destination.stream;
      audioElementRef.current = audioEl;
      if (outputDeviceId && 'setSinkId' in audioEl) {
        // Chrome-exclusive API not in the stock HTMLAudioElement lib types.
        // Widening to a minimal interface that declares only the field we
        // use avoids `any` while keeping the guard above as the safety net.
        await (audioEl as HTMLAudioElement & { setSinkId(id: string): Promise<void> }).setSinkId(
          outputDeviceId
        );
        if (generation !== generationRef.current) {
          audioEl.pause();
          audioEl.srcObject = null;
          return false;
        }
      }
      await audioEl.play();
      if (generation !== generationRef.current) {
        audioEl.pause();
        audioEl.srcObject = null;
        return false;
      }
      return true;
    },
    []
  );

  const startMeterPolling = useCallback(
    (meterAnalyser: AnalyserNode) => {
      const meterData = new Float32Array(meterAnalyser.fftSize);
      const pollMeter = () => {
        if (!meterAnalyserRef.current) return;
        meterAnalyserRef.current.getFloatTimeDomainData(meterData);
        const peak = getFloatPeak(meterData);
        const dbfs = peak > 0 ? Math.max(-80, 20 * Math.log10(peak)) : -80;
        setDbfsLevel(dbfs);
        meterRafRef.current = requestAnimationFrame(pollMeter);
      };
      meterRafRef.current = requestAnimationFrame(pollMeter);
    },
    [setDbfsLevel]
  );

  const scheduleRestart = useCallback(() => {
    const restartPendingTest = pendingRef.current && !isTestingRef.current;
    if (restartPendingTest) {
      stopTest({ keepCallSuspension: callSuspensionRef.current });
    }
    if (isRestartingRef.current) return;
    isRestartingRef.current = true;
    restartTimeoutRef.current = setTimeout(async () => {
      restartTimeoutRef.current = null;
      if (isTestingRef.current || restartPendingTest) {
        await startTestRef.current();
      }
      isRestartingRef.current = false;
    }, 0);
  }, [stopTest]);

  const applyLiveSettings = useCallback(
    (state: MicTestAudioSettings, prev: MicTestAudioSettings) => {
      if (
        effectiveMicLevelPercent(state) !== effectiveMicLevelPercent(prev) &&
        gainNodeRef.current &&
        audioContextRef.current &&
        audioContextRef.current.state !== 'closed'
      ) {
        gainNodeRef.current.gain.setTargetAtTime(
          effectiveMicLevelPercent(state) / 100,
          audioContextRef.current.currentTime,
          0.01
        );
      }

      if (state.noiseGateLevel !== prev.noiseGateLevel) {
        noiseGateThresholdRef.current = state.noiseGateLevel;
      }
    },
    []
  );

  const subscribeToRestarts = useCallback(() => {
    settingsUnsubRef.current = useAudioSettingsStore.subscribe((state, prev) => {
      applyLiveSettings(state, prev);
      if (!shouldRestartForSettings(state, prev)) return;
      scheduleRestart();
    });

    deviceUnsubRef.current = useVoiceStore.subscribe((state, prev) => {
      if (shouldRestartForDevices(state, prev)) {
        scheduleRestart();
        return;
      }
      const callStarted =
        !isInVoiceCall(prev.connectionState) && isInVoiceCall(state.connectionState);
      const callEnded =
        isInVoiceCall(prev.connectionState) && !isInVoiceCall(state.connectionState);
      const callConflict = state.localIsTesting && !callSuspensionRef.current;
      if (callStarted || callEnded || callConflict) stopTest();
    });
  }, [applyLiveSettings, scheduleRestart, stopTest]);

  const startMicPipeline = useCallback(
    async (
      adv: MicTestAudioSettings,
      voiceState: MicTestVoiceState,
      useProcessing: boolean,
      generation: number
    ): Promise<boolean> => {
      const stream = await navigator.mediaDevices.getUserMedia({
        audio: buildMicConstraints(adv, voiceState, useProcessing),
      });
      if (generation !== generationRef.current) {
        stopStreamTracks(stream);
        return false;
      }
      micStreamRef.current = stream;

      const ctx = await createRunningAudioContext(generation);
      if (!ctx) {
        stopStreamTracks(stream);
        return false;
      }
      if (generation !== generationRef.current) return false;
      const source = ctx.createMediaStreamSource(stream);
      const volumeNode = connectInputVolume(ctx, source, adv);
      const gatedNode = connectNoiseGate(ctx, volumeNode, adv);
      const meterAnalyser = connectMeter(ctx, gatedNode);

      const loopbackReady = await createLoopbackAudio(
        ctx,
        meterAnalyser,
        voiceState.audioOutputDeviceId,
        generation
      );
      if (generation !== generationRef.current) return false;
      if (!loopbackReady) {
        if (micStreamRef.current === stream) {
          stopStreamTracks(stream);
          micStreamRef.current = null;
        }
        if (audioContextRef.current === ctx) {
          audioContextRef.current = null;
          if (ctx.state !== 'closed') await ctx.close().catch(() => {});
        }
        return false;
      }
      startMeterPolling(meterAnalyser);
      return true;
    },
    [
      connectInputVolume,
      connectMeter,
      connectNoiseGate,
      createLoopbackAudio,
      createRunningAudioContext,
      startMeterPolling,
    ]
  );

  const startTest = useCallback(async () => {
    // Idempotent: stop any existing test first
    const keepCallSuspension = callSuspensionRef.current;
    stopTest({ keepCallSuspension });
    const generation = ++generationRef.current;
    pendingRef.current = true;
    subscribeToRestarts();

    const adv = useAudioSettingsStore.getState();
    const voiceState = useVoiceStore.getState();
    const inVoiceCall = isInVoiceCall(voiceState.connectionState);
    if (!inVoiceCall) releaseCallTestSuspension();
    if (inVoiceCall && voiceState.localIsTesting && !callSuspensionRef.current) {
      setError('Another audio test is already running');
      pendingRef.current = false;
      settingsUnsubRef.current?.();
      settingsUnsubRef.current = null;
      deviceUnsubRef.current?.();
      deviceUnsubRef.current = null;
      return;
    }
    const useProcessing = !adv.musicMode;

    try {
      // JIT permission check (#197): request mic access on macOS before getUserMedia
      if (!(await ensureMicPermission(generation))) {
        if (generation === generationRef.current) {
          stopTest();
          setError(
            'Microphone access denied. Grant permission in System Settings > Privacy & Security.'
          );
        }
        return;
      }
      if (generation !== generationRef.current) return;
      ensureCallSuspensionForTest(inVoiceCall);
      if (!(await startMicPipeline(adv, voiceState, useProcessing, generation))) return;
      if (generation !== generationRef.current) return;

      pendingRef.current = false;
      isTestingRef.current = true;
      setIsTesting(true);
      setError(null);
    } catch (err) {
      if (generation !== generationRef.current) return;
      stopTest();
      setError(getMicAccessErrorMessage(err));
    }
  }, [
    ensureCallSuspensionForTest,
    ensureMicPermission,
    releaseCallTestSuspension,
    startMicPipeline,
    subscribeToRestarts,
    stopTest,
  ]);
  startTestRef.current = startTest;

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      stopTest();
    };
  }, [stopTest]);

  return { isTesting, dbfsLevel, error, startTest, stopTest };
}
