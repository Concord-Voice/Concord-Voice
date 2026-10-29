import { useState, useRef, useCallback, useEffect } from 'react';
import {
  effectiveMicLevelPercent,
  useAudioSettingsStore,
} from '../../stores/audio/audioSettingsStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { ensureOsPermission } from '../../stores/voice/osPermissionStore';
import { voiceService } from '../../services/voice/voiceService';
import {
  createMicProcessor,
  gateForAudioSettings,
  type CalibrationEvent,
  type CalibrationPhase,
  type DynamicGateStatus,
  type MicProcessorHandle,
} from '../../services/voice/micProcessor';

export type MicCalibrationSession = {
  sampleRate: number;
  setPhase(epoch: number, phase: CalibrationPhase): void;
  finish(): number;
};

export type MicCalibrationCallbacks = {
  onEvent(event: CalibrationEvent): void;
  onInvalidated(
    reason: 'cancelled' | 'configuration' | 'capture-lost' | 'processor-error' | 'competing-test'
  ): void;
};

type InvalidationReason = Parameters<MicCalibrationCallbacks['onInvalidated']>[0];

export interface UseMicTestReturn {
  isTesting: boolean;
  isPending: boolean;
  dbfsLevel: number;
  inputOverloaded: boolean;
  dynamicGateStatus: DynamicGateStatus | null;
  error: string | null;
  startTest: () => Promise<void>;
  stopTest: () => void;
  startCalibration: (callbacks: MicCalibrationCallbacks) => Promise<MicCalibrationSession | null>;
  getActivityVersion: () => number;
  subscribeActivity?: (listener: () => void) => () => void;
}

interface StopTestOptions {
  keepCallSuspension?: boolean;
  reason?: InvalidationReason;
  finished?: boolean;
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
  if (err instanceof Error && err.message === 'Microphone processing failed') {
    return 'Microphone processing failed. Retry Test.';
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

function buildMicProcessorOptions(adv: MicTestAudioSettings, useProcessing: boolean) {
  return {
    protectAgcPeaks: useProcessing && adv.autoGainControl,
    gate: gateForAudioSettings(adv),
  };
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
    state.musicMode !== prev.musicMode ||
    state.inputVolume !== prev.inputVolume
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
 * as a real voice call (browser processing, one microphone level, peak protection,
 * noise gate), and reads its pre-gate float peak reports. Listening Test plays
 * through the selected output device; calibration keeps processing alive through
 * a silent destination and reports acknowledged observation phases.
 */
export function useMicTest(): UseMicTestReturn {
  const [isTesting, setIsTesting] = useState(false);
  const [isPending, setIsPending] = useState(false);
  const [dbfsLevel, setDbfsLevel] = useState(-Infinity);
  const [inputOverloaded, setInputOverloaded] = useState(false);
  const [dynamicGateStatus, setDynamicGateStatus] = useState<DynamicGateStatus | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Refs for audio resources (not state — avoids re-renders)
  const audioContextRef = useRef<AudioContext | null>(null);
  const micStreamRef = useRef<MediaStream | null>(null);
  const audioElementRef = useRef<HTMLAudioElement | null>(null);
  const gainNodeRef = useRef<GainNode | null>(null);
  const processorRef = useRef<MicProcessorHandle | null>(null);
  const sourceNodeRef = useRef<MediaStreamAudioSourceNode | null>(null);
  const silentSinkRef = useRef<GainNode | null>(null);
  const captureListenersRef = useRef<Array<() => void>>([]);
  const calibrationRef = useRef<MicCalibrationCallbacks | null>(null);
  const activityVersionRef = useRef(0);
  const activityListenersRef = useRef(new Set<() => void>());
  const settingsUnsubRef = useRef<(() => void) | null>(null);
  const deviceUnsubRef = useRef<(() => void) | null>(null);
  const isRestartingRef = useRef(false);
  const isTestingRef = useRef(false);
  const callSuspensionRef = useRef(false);
  const generationRef = useRef(0);
  const pendingRef = useRef(false);
  const restartTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const startTestRef = useRef<() => Promise<void>>(async () => {});

  const releaseCallTestSuspension = useCallback(() => {
    if (!callSuspensionRef.current) return;
    callSuspensionRef.current = false;
    endCallTestSuspension();
  }, []);

  const stopTest = useCallback(
    (options: StopTestOptions = {}) => {
      generationRef.current++;
      activityVersionRef.current++;
      const calibration = calibrationRef.current;
      calibrationRef.current = null;
      pendingRef.current = false;
      setIsPending(false);
      isTestingRef.current = false;
      // Detach before stopping tracks/closing the context or publishing call status.
      // Those effects can synchronously notify store and capture listeners.
      settingsUnsubRef.current?.();
      settingsUnsubRef.current = null;
      deviceUnsubRef.current?.();
      deviceUnsubRef.current = null;
      for (const removeListener of captureListenersRef.current) removeListener();
      captureListenersRef.current = [];
      if (!options.keepCallSuspension) releaseCallTestSuspension();

      if (restartTimeoutRef.current != null) {
        clearTimeout(restartTimeoutRef.current);
        restartTimeoutRef.current = null;
      }
      isRestartingRef.current = false;

      processorRef.current?.close();
      processorRef.current = null;
      sourceNodeRef.current?.disconnect();
      sourceNodeRef.current = null;
      gainNodeRef.current?.disconnect();
      gainNodeRef.current = null;
      silentSinkRef.current?.disconnect();
      silentSinkRef.current = null;

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

      setIsTesting(false);
      setDbfsLevel(-Infinity);
      setInputOverloaded(false);
      setDynamicGateStatus(null);
      setError(null);
      if (!options.finished) calibration?.onInvalidated(options.reason ?? 'cancelled');
      // A completed calibration has no capture callbacks left. Its review still
      // needs to observe later activity, including Stop while already idle.
      for (const listener of activityListenersRef.current) {
        try {
          listener();
        } catch {
          // An observer must not interrupt owner cleanup or other observers.
        }
      }
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

  const createRunningAudioContext = useCallback(
    async (generation: number) => {
      const ctx = new AudioContext({ sampleRate: 48000 });
      audioContextRef.current = ctx;
      if (calibrationRef.current) {
        const onStateChange = () => {
          if (generation === generationRef.current && ctx.state !== 'running') {
            stopTest({ reason: 'capture-lost' });
          }
        };
        ctx.addEventListener('statechange', onStateChange);
        captureListenersRef.current.push(() =>
          ctx.removeEventListener('statechange', onStateChange)
        );
      }
      if (ctx.state === 'suspended') await ctx.resume();
      if (generation !== generationRef.current) {
        if (audioContextRef.current === ctx) {
          audioContextRef.current = null;
          if (ctx.state !== 'closed') await ctx.close().catch(() => {});
        }
        return null;
      }
      return ctx;
    },
    [stopTest]
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

  const createLoopbackAudio = useCallback(
    async (
      ctx: AudioContext,
      processedNode: AudioNode,
      outputDeviceId: string | null,
      generation: number
    ): Promise<boolean> => {
      const destination = ctx.createMediaStreamDestination();
      processedNode.connect(destination);

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

  const scheduleRestart = useCallback(() => {
    const restartActiveTest = isTestingRef.current;
    const restartPendingTest = pendingRef.current && !isTestingRef.current;
    if (restartActiveTest || restartPendingTest) {
      stopTest({ keepCallSuspension: callSuspensionRef.current });
    }
    if (isRestartingRef.current) return;
    isRestartingRef.current = true;
    restartTimeoutRef.current = setTimeout(async () => {
      restartTimeoutRef.current = null;
      if (restartActiveTest || restartPendingTest) {
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
        processorRef.current?.setGate(gateForAudioSettings(state));
      }
    },
    []
  );

  const subscribeToRestarts = useCallback(() => {
    settingsUnsubRef.current = useAudioSettingsStore.subscribe((state, prev) => {
      if (calibrationRef.current) {
        if (shouldRestartForSettings(state, prev) || state.noiseGateLevel !== prev.noiseGateLevel) {
          stopTest({ reason: 'configuration' });
        }
        return;
      }
      applyLiveSettings(state, prev);
      if (!shouldRestartForSettings(state, prev)) return;
      scheduleRestart();
    });

    deviceUnsubRef.current = useVoiceStore.subscribe((state, prev) => {
      if (shouldRestartForDevices(state, prev)) {
        if (calibrationRef.current) stopTest({ reason: 'configuration' });
        else scheduleRestart();
        return;
      }
      const callStarted =
        !isInVoiceCall(prev.connectionState) && isInVoiceCall(state.connectionState);
      const callEnded =
        isInVoiceCall(prev.connectionState) && !isInVoiceCall(state.connectionState);
      const callConflict = state.localIsTesting && !callSuspensionRef.current;
      if (callStarted || callEnded || callConflict) stopTest({ reason: 'competing-test' });
    });
  }, [applyLiveSettings, scheduleRestart, stopTest]);

  const observeCaptureLoss = useCallback(
    (stream: MediaStream, generation: number) => {
      if (calibrationRef.current) {
        const onCaptureLost = () => {
          if (generation === generationRef.current) stopTest({ reason: 'capture-lost' });
        };
        for (const track of stream.getTracks()) {
          track.addEventListener('ended', onCaptureLost);
          track.addEventListener('mute', onCaptureLost);
          captureListenersRef.current.push(() => {
            track.removeEventListener('ended', onCaptureLost);
            track.removeEventListener('mute', onCaptureLost);
          });
        }
      }
    },
    [stopTest]
  );

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
      observeCaptureLoss(stream, generation);

      const ctx = await createRunningAudioContext(generation);
      if (!ctx) return false;
      if (generation !== generationRef.current) return false;
      const processor = await createMicProcessor(
        ctx,
        buildMicProcessorOptions(adv, useProcessing),
        (window) => {
          if (generation !== generationRef.current || !window.valid) return;
          if (window.overloaded) setInputOverloaded(true);
          setDbfsLevel(window.peak > 0 ? Math.max(-80, 20 * Math.log10(window.peak)) : -80);
        },
        () => {
          if (generation !== generationRef.current) return;
          stopTest({ reason: 'processor-error' });
          setError('Microphone processing failed. Retry Test.');
        },
        (status) => {
          if (generation === generationRef.current) setDynamicGateStatus(status);
        },
        (event) => {
          if (generation === generationRef.current) calibrationRef.current?.onEvent(event);
        }
      );
      if (generation !== generationRef.current) {
        processor.close();
        return false;
      }
      processorRef.current = processor;
      const source = ctx.createMediaStreamSource(stream);
      sourceNodeRef.current = source;
      const volumeNode = connectInputVolume(ctx, source, useAudioSettingsStore.getState());
      volumeNode.connect(processor.node);
      const current = useAudioSettingsStore.getState();
      processor.setGate(gateForAudioSettings(current));

      if (calibrationRef.current) {
        const sink = ctx.createGain();
        sink.gain.value = 0;
        silentSinkRef.current = sink;
        processor.node.connect(sink);
        sink.connect(ctx.destination);
        processor.setWindowReporting(true);
        return true;
      }

      const loopbackReady = await createLoopbackAudio(
        ctx,
        processor.node,
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
      processor.setWindowReporting(true);
      return true;
    },
    [
      connectInputVolume,
      createLoopbackAudio,
      createRunningAudioContext,
      observeCaptureLoss,
      stopTest,
    ]
  );

  const calibrationSession = useCallback(
    (generation: number): MicCalibrationSession | null => {
      const processor = processorRef.current;
      const ctx = audioContextRef.current;
      if (!processor || !ctx) return null;
      return {
        sampleRate: ctx.sampleRate,
        setPhase(epoch, phase) {
          if (generation === generationRef.current) processor.setCalibrationPhase(epoch, phase);
        },
        finish() {
          // A retired handle must never stop or change the version of its successor.
          if (generation === generationRef.current) stopTest({ finished: true });
          return activityVersionRef.current;
        },
      };
    },
    [stopTest]
  );

  const startOwner = useCallback(
    async (callbacks?: MicCalibrationCallbacks): Promise<MicCalibrationSession | null> => {
      const keepCallSuspension = callSuspensionRef.current;
      stopTest({ keepCallSuspension, reason: 'competing-test' });
      const generation = ++generationRef.current;
      calibrationRef.current = callbacks ?? null;
      pendingRef.current = true;
      setIsPending(true);
      subscribeToRestarts();

      const adv = useAudioSettingsStore.getState();
      const voiceState = useVoiceStore.getState();
      const inVoiceCall = isInVoiceCall(voiceState.connectionState);
      if (!inVoiceCall) releaseCallTestSuspension();
      if (inVoiceCall && voiceState.localIsTesting && !callSuspensionRef.current) {
        stopTest({ reason: 'competing-test' });
        setError('Another audio test is already running');
        return null;
      }
      const useProcessing = !adv.musicMode;

      try {
        // JIT permission check (#197): request mic access on macOS before getUserMedia.
        if (!(await ensureMicPermission(generation))) {
          if (generation === generationRef.current) {
            stopTest();
            setError(
              'Microphone access denied. Grant permission in System Settings > Privacy & Security.'
            );
          }
          return null;
        }
        if (generation !== generationRef.current) return null;
        ensureCallSuspensionForTest(inVoiceCall);
        if (!(await startMicPipeline(adv, voiceState, useProcessing, generation))) {
          if (generation === generationRef.current) stopTest();
          return null;
        }
        if (generation !== generationRef.current) return null;

        pendingRef.current = false;
        setIsPending(false);
        isTestingRef.current = true;
        setIsTesting(true);
        setError(null);
        if (!callbacks) return null;
        return calibrationSession(generation);
      } catch (err) {
        if (generation !== generationRef.current) return null;
        stopTest({ reason: 'processor-error' });
        setError(getMicAccessErrorMessage(err));
        return null;
      }
    },
    [
      calibrationSession,
      ensureCallSuspensionForTest,
      ensureMicPermission,
      releaseCallTestSuspension,
      startMicPipeline,
      subscribeToRestarts,
      stopTest,
    ]
  );
  const startTest = useCallback(async () => {
    await startOwner();
  }, [startOwner]);
  const startCalibration = useCallback(
    (callbacks: MicCalibrationCallbacks) => startOwner(callbacks),
    [startOwner]
  );
  const getActivityVersion = useCallback(() => activityVersionRef.current, []);
  const subscribeActivity = useCallback((listener: () => void) => {
    activityListenersRef.current.add(listener);
    return () => {
      activityListenersRef.current.delete(listener);
    };
  }, []);
  startTestRef.current = startTest;

  // Cleanup on unmount
  useEffect(() => {
    return () => {
      stopTest();
    };
  }, [stopTest]);

  return {
    isTesting,
    isPending,
    dbfsLevel,
    inputOverloaded,
    dynamicGateStatus,
    error,
    startTest,
    stopTest,
    startCalibration,
    getActivityVersion,
    subscribeActivity,
  };
}
