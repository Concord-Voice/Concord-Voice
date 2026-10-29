import { useCallback, useEffect, useRef, useState } from 'react';
import {
  effectiveMicLevelPercent,
  effectiveNoiseGateMode,
  useAudioSettingsStore,
  type AudioSettings,
} from '../../stores/audio/audioSettingsStore';
import { useDraftSettingsStore } from '../../stores/ui/draftSettingsStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { setDraftAudioSetting } from '../ui/useDraftSettings';
import {
  recommendGateThreshold,
  type CalibrationResult,
} from '../../services/voice/gateCalibration';
import type {
  CalibrationEvent,
  CalibrationPhase,
  PeakWindow,
} from '../../services/voice/micProcessor';
import type { MicCalibrationSession, UseMicTestReturn } from './useMicTest';

export type GateCalibrationState = {
  phase: 'preparation' | 'acquiring' | 'quiet' | 'readyToRead' | 'speech' | 'review' | 'refused';
  secondsRemaining: number;
  meterDbfs: number | null;
  result: CalibrationResult | null;
  message: string | null;
  canStart: boolean;
  canFinishReading: boolean;
};
export type GateCalibrationController = {
  state: GateCalibrationState;
  start(): Promise<void>;
  startReading(): void;
  finishReading(): void;
  cancel(): void;
  useThreshold(): void;
};
type Fingerprint = {
  snapshot: ReturnType<typeof useDraftSettingsStore.getState>['snapshot'];
  configuration: string;
  activity: number;
  revision: number;
};
type Capture = {
  identity: Fingerprint;
  starting: boolean;
  session: MicCalibrationSession | null;
  epoch: number;
  phase: CalibrationPhase;
  acknowledged: boolean;
  quiet: PeakWindow[];
  speech: PeakWindow[];
  nonzeroSpeech: number;
  windowFrames: number;
  quietLimit: number;
  speechLimit: number;
};
type Proposal = { identity: Fingerprint; thresholdDbfs: number | null };

function processing(settings: AudioSettings) {
  return [
    settings.musicMode,
    settings.autoGainControl,
    settings.noiseCancellation,
    settings.echoCancellation,
    effectiveMicLevelPercent(settings),
  ];
}
function configuration() {
  const applied = useAudioSettingsStore.getState();
  const draft = useDraftSettingsStore.getState();
  const candidate = { ...applied, ...draft.snapshot?.audio, ...draft.drafts.audio };
  const devices = useVoiceStore.getState();
  const mode = effectiveNoiseGateMode(candidate);
  const eligible =
    draft.snapshot !== null &&
    (mode === 'autoCalibrate' || mode === 'manualCalibrate') &&
    (applied.musicMode || !applied.autoGainControl) &&
    (candidate.musicMode || !candidate.autoGainControl) &&
    JSON.stringify(processing(applied)) === JSON.stringify(processing(candidate));
  return {
    eligible,
    snapshot: draft.snapshot,
    configuration: JSON.stringify([
      processing(applied),
      processing(candidate),
      applied.advancedMode,
      applied.noiseGateMode,
      candidate.noiseGateMode,
      devices.audioInputDeviceId,
      devices.audioOutputDeviceId,
    ]),
  };
}
function sameIdentity(a: Fingerprint, b: Fingerprint): boolean {
  return (
    a.snapshot === b.snapshot &&
    a.configuration === b.configuration &&
    a.activity === b.activity &&
    a.revision === b.revision
  );
}
function initialState(): GateCalibrationState {
  return {
    phase: 'preparation',
    secondsRemaining: 10,
    meterDbfs: null,
    result: null,
    message: null,
    canStart: configuration().eligible,
    canFinishReading: false,
  };
}

/** Coordinates bounded numeric evidence; the Test owner alone acquires audio. */
export function useGateCalibration(micTest: UseMicTestReturn): GateCalibrationController {
  const { getActivityVersion, stopTest, startCalibration, subscribeActivity } = micTest;
  const [state, setState] = useState(initialState);
  const stateRef = useRef(state);
  const captureRef = useRef<Capture | null>(null);
  const proposalRef = useRef<Proposal | null>(null);
  const revisionRef = useRef(0);
  const mountedRef = useRef(true);
  const publish = useCallback((patch: Partial<GateCalibrationState>) => {
    const previous = stateRef.current;
    if (
      Object.entries(patch).every(
        ([key, value]) => previous[key as keyof typeof previous] === value
      )
    )
      return;
    const next = { ...previous, ...patch };
    stateRef.current = next;
    if (mountedRef.current) setState(next);
  }, []);
  const fingerprint = useCallback((): Fingerprint => {
    const current = configuration();
    return {
      snapshot: current.snapshot,
      configuration: current.configuration,
      activity: getActivityVersion(),
      revision: revisionRef.current,
    };
  }, [getActivityVersion]);
  const invalidate = useCallback(
    (message: string | null, stopOwner = true) => {
      const capture = captureRef.current;
      captureRef.current = null;
      proposalRef.current = null;
      if (capture) {
        capture.quiet.length = 0;
        capture.speech.length = 0;
        // Retire first: synchronous Stop callbacks cannot recurse.
        if (stopOwner) stopTest();
      }
      publish({
        phase: 'preparation',
        secondsRemaining: 10,
        meterDbfs: null,
        result: null,
        message,
        canStart: configuration().eligible,
        canFinishReading: false,
      });
    },
    [stopTest, publish]
  );
  const complete = useCallback(
    (capture: Capture, refusal?: 'invalid' | 'clipping' | 'insufficient') => {
      if (captureRef.current !== capture || !capture.session) return;
      if (!sameIdentity(capture.identity, fingerprint()) || !configuration().eligible) {
        invalidate('Audio settings changed. Start a new calibration.');
        return;
      }
      const result: CalibrationResult = refusal
        ? { kind: 'refused', reason: refusal, summary: null }
        : recommendGateThreshold(capture.quiet, capture.speech, capture.session.sampleRate);
      // Finish advances activity intentionally. Retire capture before notification
      // and retain the returned version, rather than the acquisition version.
      const session = capture.session;
      captureRef.current = null;
      capture.session = null;
      capture.quiet.length = 0;
      capture.speech.length = 0;
      const activity = session.finish();
      const identity = { ...capture.identity, activity };
      if (!sameIdentity(identity, fingerprint()) || !configuration().eligible) {
        invalidate('Audio settings changed. Start a new calibration.');
        return;
      }
      proposalRef.current = {
        identity,
        thresholdDbfs: result.kind === 'recommended' ? result.thresholdDbfs : null,
      };
      publish({
        phase: result.kind === 'recommended' ? 'review' : 'refused',
        secondsRemaining: 0,
        meterDbfs: null,
        result,
        message: null,
        canStart: configuration().eligible,
        canFinishReading: false,
      });
    },
    [fingerprint, invalidate, publish]
  );
  const setPhase = useCallback((capture: Capture, phase: CalibrationPhase) => {
    capture.epoch++;
    capture.phase = phase;
    capture.acknowledged = false;
    capture.session?.setPhase(capture.epoch, phase);
  }, []);
  const onPhaseComplete = useCallback(
    (
      capture: Capture,
      event: Extract<CalibrationEvent, { type: 'calibrationComplete' }>,
      windows: PeakWindow[],
      limit: number
    ) => {
      if (
        Object.keys(event).length !== 4 ||
        windows.length !== limit ||
        event.frames !== limit * capture.windowFrames
      ) {
        complete(capture, 'insufficient');
        return;
      }
      if (capture.phase === 'speech') complete(capture);
      else {
        setPhase(capture, 'idle');
        publish({ phase: 'readyToRead', secondsRemaining: 0, meterDbfs: null });
      }
    },
    [complete, publish, setPhase]
  );
  const onWindow = useCallback(
    (
      capture: Capture,
      event: Extract<CalibrationEvent, { type: 'calibrationWindow' }>,
      windows: PeakWindow[],
      limit: number,
      rate: number
    ) => {
      if (
        Object.keys(event).length !== 7 ||
        !Number.isFinite(event.peak) ||
        event.peak < 0 ||
        event.frames !== capture.windowFrames ||
        event.valid !== true ||
        typeof event.overloaded !== 'boolean'
      ) {
        complete(capture, 'invalid');
        return;
      }
      if (event.overloaded || event.peak >= 1) {
        complete(capture, 'clipping');
        return;
      }
      windows.push({ peak: event.peak, frames: event.frames, valid: true, overloaded: false });
      if (capture.phase === 'speech' && event.peak > 0) capture.nonzeroSpeech++;
      const seconds = (windows.length * capture.windowFrames) / rate;
      const canFinishReading =
        capture.phase === 'speech' &&
        seconds >= 3 &&
        capture.nonzeroSpeech * capture.windowFrames >= rate;
      const patch: Partial<GateCalibrationState> = {
        secondsRemaining: Math.max(0, Math.ceil((capture.phase === 'quiet' ? 10 : 20) - seconds)),
        canFinishReading,
      };
      // Ten meter updates per captured second; no timer can create evidence.
      if (windows.length === 1 || windows.length % 5 === 0)
        patch.meterDbfs = event.peak === 0 ? null : 20 * Math.log10(event.peak);
      publish(patch);
      if (capture.phase === 'speech' && windows.length === limit) complete(capture);
    },
    [complete, publish]
  );
  const onEvent = useCallback(
    (capture: Capture, event: CalibrationEvent) => {
      if (captureRef.current !== capture || !capture.session) return;
      if (!sameIdentity(capture.identity, fingerprint())) {
        invalidate('Audio settings changed. Start a new calibration.');
        return;
      }
      if (event?.epoch !== capture.epoch || event.phase !== capture.phase) return;
      if (event.type === 'calibrationAck') {
        if (Object.keys(event).length === 3) capture.acknowledged = true;
        return;
      }
      if (!capture.acknowledged || capture.phase === 'idle') return;
      const windows = capture.phase === 'quiet' ? capture.quiet : capture.speech;
      const limit = capture.phase === 'quiet' ? capture.quietLimit : capture.speechLimit;
      if (event.type === 'calibrationComplete') {
        onPhaseComplete(capture, event, windows, limit);
        return;
      }
      if (event.type !== 'calibrationWindow' || windows.length >= limit) return;
      onWindow(capture, event, windows, limit, capture.session.sampleRate);
    },
    [fingerprint, invalidate, onPhaseComplete, onWindow]
  );
  const start = useCallback(async () => {
    if (captureRef.current) return;
    if (!configuration().eligible) {
      invalidate(
        'Apply microphone processing and level changes, and turn off automatic gain before calibrating.'
      );
      return;
    }
    proposalRef.current = null;
    const capture: Capture = {
      identity: fingerprint(),
      starting: true,
      session: null,
      epoch: 0,
      phase: 'idle',
      acknowledged: false,
      quiet: [],
      speech: [],
      nonzeroSpeech: 0,
      windowFrames: 0,
      quietLimit: 0,
      speechLimit: 0,
    };
    captureRef.current = capture;
    publish({
      phase: 'acquiring',
      result: null,
      message: null,
      secondsRemaining: 10,
      meterDbfs: null,
      canStart: false,
      canFinishReading: false,
    });
    try {
      const pending = startCalibration({
        onEvent: (event) => onEvent(capture, event),
        onInvalidated: (reason) => {
          if (captureRef.current === capture)
            invalidate(`Calibration stopped (${reason}). Try again.`, false);
        },
      });
      // Starting the owner advances its version synchronously, before permission.
      capture.identity = { ...capture.identity, activity: getActivityVersion() };
      capture.starting = false;
      const session = await pending;
      if (captureRef.current !== capture) return;
      if (!session || !sameIdentity(capture.identity, fingerprint()) || !configuration().eligible) {
        invalidate('Calibration could not start. Check microphone access and try again.');
        return;
      }
      capture.session = session;
      const rate = session.sampleRate;
      capture.windowFrames = Math.round(rate * 0.02);
      capture.quietLimit = Math.ceil((rate * 10) / capture.windowFrames);
      capture.speechLimit = Math.floor((rate * 20) / capture.windowFrames);
      if (
        !Number.isFinite(rate) ||
        rate <= 0 ||
        !Number.isSafeInteger(capture.windowFrames) ||
        capture.windowFrames < 1 ||
        !Number.isSafeInteger(capture.quietLimit) ||
        capture.quietLimit < 1 ||
        capture.quietLimit > 501 ||
        !Number.isSafeInteger(capture.speechLimit) ||
        capture.speechLimit < 1 ||
        capture.speechLimit > 1000 ||
        !Number.isSafeInteger(capture.windowFrames * capture.quietLimit) ||
        !Number.isSafeInteger(capture.windowFrames * capture.speechLimit)
      ) {
        complete(capture, 'invalid');
        return;
      }
      publish({ phase: 'quiet' });
      setPhase(capture, 'quiet');
    } catch {
      if (captureRef.current === capture) invalidate('Calibration could not start. Try again.');
    }
  }, [
    complete,
    fingerprint,
    invalidate,
    startCalibration,
    getActivityVersion,
    onEvent,
    publish,
    setPhase,
  ]);
  const startReading = useCallback(() => {
    const capture = captureRef.current;
    if (!capture || stateRef.current.phase !== 'readyToRead') return;
    if (!sameIdentity(capture.identity, fingerprint())) {
      invalidate('Audio settings changed. Start a new calibration.');
      return;
    }
    publish({ phase: 'speech', secondsRemaining: 20, canFinishReading: false });
    setPhase(capture, 'speech');
  }, [fingerprint, invalidate, publish, setPhase]);
  const finishReading = useCallback(() => {
    const capture = captureRef.current;
    if (capture && stateRef.current.phase === 'speech' && stateRef.current.canFinishReading)
      complete(capture);
  }, [complete]);
  const cancel = useCallback(() => invalidate(null), [invalidate]);
  const useThreshold = useCallback(() => {
    const proposal = proposalRef.current;
    if (!proposal || proposal.thresholdDbfs === null) return;
    if (!sameIdentity(proposal.identity, fingerprint()) || !configuration().eligible) {
      invalidate('This suggestion is out of date. Start a new calibration.');
      return;
    }
    // Consume before the draft setter can synchronously notify subscriptions.
    proposalRef.current = null;
    setDraftAudioSetting('noiseGateLevel', proposal.thresholdDbfs);
    publish({
      result: null,
      phase: 'preparation',
      message: 'Threshold added to pending settings. Apply to use it.',
      canStart: configuration().eligible,
    });
  }, [fingerprint, invalidate, publish]);
  useEffect(() => {
    mountedRef.current = true;
    const check = () => {
      const identity = captureRef.current?.identity ?? proposalRef.current?.identity;
      const current = fingerprint();
      // The owner may publish call-suspension state while starting and before
      // startCalibration returns its promise. Its initial version is expected.
      if (captureRef.current?.starting && identity) current.activity = identity.activity;
      if (identity && !sameIdentity(identity, current))
        invalidate('Audio settings or microphone activity changed. Start a new calibration.');
      else if (!captureRef.current) publish({ canStart: configuration().eligible });
    };
    const unsubscribeAudio = useAudioSettingsStore.subscribe(check);
    const unsubscribeVoice = useVoiceStore.subscribe(check);
    const unsubscribeDraft = useDraftSettingsStore.subscribe((next, previous) => {
      // Revert keeps the snapshot but replaces every overlay, even when empty.
      // Ordinary field edits retain sibling overlays.
      if (
        next.drafts.audio !== previous.drafts.audio &&
        next.drafts.appearance !== previous.drafts.appearance &&
        next.drafts.video !== previous.drafts.video &&
        next.drafts.tts !== previous.drafts.tts
      )
        revisionRef.current++;
      check();
    });
    const unsubscribeActivity = subscribeActivity?.(() => {
      // Acquisition owns the initial activity change; finish retires capture
      // before notification. A retained review always rechecks synchronously.
      if (proposalRef.current) check();
    });
    check();
    return () => {
      mountedRef.current = false;
      unsubscribeAudio();
      unsubscribeVoice();
      unsubscribeDraft();
      unsubscribeActivity?.();
      invalidate(null);
    };
  }, [fingerprint, invalidate, subscribeActivity, publish]);
  return { state, start, startReading, finishReading, cancel, useThreshold };
}
