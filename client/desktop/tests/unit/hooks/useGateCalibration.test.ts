import { act, renderHook } from '@testing-library/react';
import { useGateCalibration } from '@/renderer/hooks/device/useGateCalibration';
import type {
  MicCalibrationCallbacks,
  MicCalibrationSession,
  UseMicTestReturn,
} from '@/renderer/hooks/device/useMicTest';
import type { CalibrationEvent, CalibrationPhase } from '@/renderer/services/voice/micProcessor';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useDraftSettingsStore } from '@/renderer/stores/ui/draftSettingsStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import * as draftSettings from '@/renderer/hooks/ui/useDraftSettings';
import { resetAllStores } from '../../helpers/store-helpers';

function fakeOwner(sampleRate = 48_000) {
  let callbacks: MicCalibrationCallbacks | undefined;
  let version = 0;
  let active = false;
  const activityListeners = new Set<() => void>();
  const advanceActivity = () => {
    ++version;
    for (const listener of activityListeners) listener();
    return version;
  };
  const subscribeActivity = vi.fn((listener: () => void) => {
    activityListeners.add(listener);
    return () => {
      activityListeners.delete(listener);
    };
  });
  const session: MicCalibrationSession = {
    sampleRate,
    setPhase: vi.fn((_epoch: number, _phase: CalibrationPhase) => {}),
    finish: vi.fn(() => {
      active = false;
      return advanceActivity();
    }),
  };
  const owner: UseMicTestReturn = {
    isTesting: false,
    isPending: false,
    dbfsLevel: -100,
    inputOverloaded: false,
    dynamicGateStatus: null,
    error: null,
    startTest: vi.fn(async () => {
      active = false;
      advanceActivity();
    }),
    stopTest: vi.fn(() => {
      const wasActive = active;
      active = false;
      advanceActivity();
      if (wasActive) callbacks?.onInvalidated('cancelled');
    }),
    startCalibration: vi.fn(async (next: MicCalibrationCallbacks) => {
      callbacks = next;
      active = true;
      advanceActivity();
      return session;
    }),
    getActivityVersion: () => version,
    subscribeActivity,
  };
  return {
    owner,
    session,
    activityListenerCount: () => activityListeners.size,
    emit: (event: CalibrationEvent) => callbacks?.onEvent(event),
    invalidate: (reason: Parameters<MicCalibrationCallbacks['onInvalidated']>[0]) => {
      active = false;
      advanceActivity();
      callbacks?.onInvalidated(reason);
    },
    replaceActivity: advanceActivity,
    // Deliberately omit notification to independently exercise the synchronous Use guard.
    changeVersionWithoutNotification: () => {
      ++version;
    },
    command: (phase: CalibrationPhase) => {
      const calls = vi.mocked(session.setPhase).mock.calls.filter((call) => call[1] === phase);
      const command = calls.at(-1);
      if (!command) throw new Error(`No ${phase} command issued`);
      return command[0];
    },
  };
}

type Owner = ReturnType<typeof fakeOwner>;
function acknowledge(fake: Owner, phase: CalibrationPhase) {
  const epoch = fake.command(phase);
  fake.emit({ type: 'calibrationAck', epoch, phase });
  return epoch;
}
function windows(
  fake: Owner,
  phase: 'quiet' | 'speech',
  count: number,
  peak = 0.001,
  epoch = fake.command(phase)
) {
  for (let index = 0; index < count; index++) {
    fake.emit({
      type: 'calibrationWindow',
      epoch,
      phase,
      peak,
      frames: Math.round(fake.session.sampleRate * 0.02),
      valid: true,
      overloaded: false,
    });
  }
}
async function setupSpeech(fake = fakeOwner()) {
  const hook = renderHook(() => useGateCalibration(fake.owner));
  await act(async () => {
    await hook.result.current.start();
  });
  act(() => {
    const epoch = acknowledge(fake, 'quiet');
    windows(fake, 'quiet', 500);
    fake.emit({
      type: 'calibrationComplete',
      epoch,
      phase: 'quiet',
      frames: fake.session.sampleRate * 10,
    });
  });
  expect(hook.result.current.state.phase).toBe('readyToRead');
  act(() => {
    hook.result.current.startReading();
    acknowledge(fake, 'speech');
  });
  return { ...hook, fake };
}
async function setupReview() {
  const hook = await setupSpeech();
  act(() => {
    windows(hook.fake, 'speech', 150, 10 ** (-30 / 20));
  });
  expect(hook.result.current.state.canFinishReading).toBe(true);
  act(() => {
    hook.result.current.finishReading();
  });
  expect(hook.result.current.state.phase).toBe('review');
  expect(hook.result.current.state.result).toMatchObject({
    kind: 'recommended',
    thresholdDbfs: -57,
  });
  return hook;
}

beforeEach(() => {
  resetAllStores();
  useDraftSettingsStore.getState().teardown();
  useAudioSettingsStore.setState({
    advancedMode: true,
    autoGainControl: false,
    musicMode: false,
    noiseGateMode: 'manualCalibrate',
    noiseGateLevel: -50,
    inputVolume: 100,
    noiseCancellation: true,
    echoCancellation: true,
  });
  useVoiceStore.setState({ audioInputDeviceId: null, audioOutputDeviceId: null });
  useDraftSettingsStore.getState().initialize();
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe('useGateCalibration captured phases', () => {
  it('requires explicit Start and gives permission waits and unacknowledged reports no capture credit', async () => {
    vi.useFakeTimers();
    const fake = fakeOwner();
    let resolve: ((session: MicCalibrationSession) => void) | undefined;
    vi.mocked(fake.owner.startCalibration).mockImplementation(
      () =>
        new Promise((done) => {
          resolve = done;
        })
    );
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    expect(result.current.state.phase).toBe('preparation');
    expect(fake.owner.startCalibration).not.toHaveBeenCalled();
    let pending: Promise<void> | undefined;
    act(() => {
      pending = result.current.start();
    });
    expect(result.current.state.phase).toBe('acquiring');
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(result.current.state.phase).toBe('acquiring');
    await act(async () => {
      resolve?.(fake.session);
      await pending;
    });
    expect(result.current.state.secondsRemaining).toBe(10);
    expect(fake.session.setPhase).toHaveBeenCalledWith(expect.any(Number), 'quiet');
  });

  it('counts only acknowledged quiet windows, then waits for an explicit reading action', async () => {
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    await act(async () => {
      await result.current.start();
    });
    act(() => {
      windows(fake, 'quiet', 100);
    });
    expect(result.current.state.secondsRemaining).toBe(10);
    act(() => {
      acknowledge(fake, 'quiet');
      windows(fake, 'quiet', 499);
    });
    expect(result.current.state.phase).toBe('quiet');
    expect(result.current.state.secondsRemaining).toBe(1);
    act(() => {
      windows(fake, 'quiet', 1);
      fake.emit({
        type: 'calibrationComplete',
        epoch: fake.command('quiet'),
        phase: 'quiet',
        frames: 480_000,
      });
    });
    expect(result.current.state.phase).toBe('readyToRead');
    expect(fake.session.setPhase).toHaveBeenCalledWith(expect.any(Number), 'idle');
    expect(
      vi.mocked(fake.session.setPhase).mock.calls.some(([, phase]) => phase === 'speech')
    ).toBe(false);
  });

  it('gives elapsed time and an unbacked completion event no speech credit', async () => {
    vi.useFakeTimers();
    const { result, fake } = await setupSpeech();
    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(result.current.state.canFinishReading).toBe(false);
    expect(result.current.state.phase).toBe('speech');
    act(() => {
      fake.emit({
        type: 'calibrationComplete',
        epoch: fake.command('speech'),
        phase: 'speech',
        frames: 960_000,
      });
    });
    expect(result.current.state.result?.kind).not.toBe('recommended');
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
  });

  it('does not count speech reports before its exact acknowledgement', async () => {
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    await act(async () => {
      await result.current.start();
    });
    act(() => {
      acknowledge(fake, 'quiet');
      windows(fake, 'quiet', 500);
      fake.emit({
        type: 'calibrationComplete',
        epoch: fake.command('quiet'),
        phase: 'quiet',
        frames: 480_000,
      });
    });
    act(() => {
      result.current.startReading();
    });
    act(() => {
      fake.emit({ type: 'calibrationAck', epoch: fake.command('quiet'), phase: 'quiet' });
      windows(fake, 'speech', 150, 0.03);
    });
    expect(result.current.state.canFinishReading).toBe(false);
    act(() => {
      acknowledge(fake, 'speech');
      windows(fake, 'speech', 149, 0.03);
    });
    expect(result.current.state.canFinishReading).toBe(false);
    act(() => {
      windows(fake, 'speech', 1, 0.03);
    });
    expect(result.current.state.canFinishReading).toBe(true);
  });

  it('cancels permission acquisition immediately and ignores a late session', async () => {
    const fake = fakeOwner();
    let resolve: ((session: MicCalibrationSession) => void) | undefined;
    vi.mocked(fake.owner.startCalibration).mockImplementation(
      () =>
        new Promise((done) => {
          resolve = done;
        })
    );
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    let pending: Promise<void> | undefined;
    act(() => {
      pending = result.current.start();
    });
    expect(result.current.state.phase).toBe('acquiring');
    act(() => {
      result.current.cancel();
    });
    expect(result.current.state.phase).toBe('preparation');
    expect(fake.owner.stopTest).toHaveBeenCalled();
    await act(async () => {
      resolve?.(fake.session);
      await pending;
    });
    expect(result.current.state.phase).toBe('preparation');
    expect(fake.session.setPhase).not.toHaveBeenCalled();
  });

  it.each(['unchanged', 'configuration', 'device'] as const)(
    'handles in-call owner publication during pending acquisition: %s',
    async (change) => {
      useVoiceStore.setState({ connectionState: 'connected', localIsTesting: false });
      const fake = fakeOwner();
      const begin = vi.mocked(fake.owner.startCalibration).getMockImplementation();
      if (!begin) throw new Error('Fake owner must provide acquisition behavior');
      let resolve: ((session: MicCalibrationSession) => void) | undefined;
      vi.mocked(fake.owner.startCalibration).mockImplementation((callbacks) => {
        // The real owner advances activity before publishing call suspension.
        void begin(callbacks);
        useVoiceStore.setState({ localIsTesting: true });
        return new Promise((done) => {
          resolve = done;
        });
      });
      const { result } = renderHook(() => useGateCalibration(fake.owner));
      let pending: Promise<void> | undefined;
      act(() => {
        pending = result.current.start();
      });
      expect(result.current.state.phase).toBe('acquiring');
      expect(fake.owner.stopTest).not.toHaveBeenCalled();
      if (change !== 'unchanged') {
        act(() => {
          if (change === 'configuration') useAudioSettingsStore.setState({ inputVolume: 80 });
          else useVoiceStore.setState({ audioInputDeviceId: 'changed-during-acquisition' });
        });
        expect(result.current.state.phase).not.toBe('acquiring');
        expect(fake.owner.stopTest).toHaveBeenCalled();
      }
      await act(async () => {
        resolve?.(fake.session);
        await pending;
      });
      if (change === 'unchanged') {
        expect(result.current.state.phase).toBe('quiet');
        expect(fake.session.setPhase).toHaveBeenCalledWith(expect.any(Number), 'quiet');
        expect(fake.owner.stopTest).not.toHaveBeenCalled();
      } else {
        expect(result.current.state.phase).not.toBe('quiet');
        expect(fake.session.setPhase).not.toHaveBeenCalled();
      }
    }
  );

  it('stops the owner on unmount during sampling', async () => {
    const { unmount, fake } = await setupSpeech();
    act(() => {
      unmount();
    });
    expect(fake.owner.stopTest).toHaveBeenCalled();
  });

  it.each([44_100, 48_000, 96_000])(
    'uses captured frames at %i Hz and ignores old epoch windows after speech acknowledgement',
    async (sampleRate) => {
      const { result, fake } = await setupSpeech(fakeOwner(sampleRate));
      act(() => {
        windows(fake, 'speech', 149, 0.03);
        windows(fake, 'quiet', 500, 0.9);
        windows(fake, 'speech', 500, 0.9, fake.command('quiet'));
      });
      expect(result.current.state.canFinishReading).toBe(false);
      act(() => {
        windows(fake, 'speech', 1, 0.03);
      });
      expect(result.current.state.canFinishReading).toBe(true);
      act(() => {
        result.current.finishReading();
      });
      expect(result.current.state.result?.kind).toBe('recommended');
    }
  );

  it('requires one second of nonzero windows as well as three seconds captured', async () => {
    const { result, fake } = await setupSpeech();
    act(() => {
      windows(fake, 'speech', 101, 0);
      windows(fake, 'speech', 49, 0.03);
    });
    expect(result.current.state.canFinishReading).toBe(false);
    act(() => {
      result.current.finishReading();
    });
    expect(result.current.state.phase).toBe('speech');
    act(() => {
      windows(fake, 'speech', 1, 0.03);
    });
    expect(result.current.state.canFinishReading).toBe(true);
    act(() => {
      result.current.finishReading();
    });
    expect(result.current.state.result).toMatchObject({ kind: 'refused', reason: 'insufficient' });
  });

  it('automatically completes after twenty captured seconds and stops the owner once', async () => {
    const { result, fake } = await setupSpeech();
    act(() => {
      windows(fake, 'speech', 1000, 0.03);
      fake.emit({
        type: 'calibrationComplete',
        epoch: fake.command('speech'),
        phase: 'speech',
        frames: 960_000,
      });
    });
    expect(result.current.state.phase).toBe('review');
    expect(fake.session.finish).toHaveBeenCalledTimes(1);
    const review = result.current.state.result;
    act(() => {
      windows(fake, 'speech', 1000, 1);
    });
    expect(result.current.state.result).toEqual(review);
    expect(fake.session.finish).toHaveBeenCalledTimes(1);
  });

  it.each([
    { peak: Number.NaN, valid: true, overloaded: false },
    { peak: 0, valid: false, overloaded: false },
    { peak: 1, valid: true, overloaded: true },
  ])(
    'refuses invalid or overloaded evidence instead of silently filtering it: %j',
    async (sample) => {
      const { result, fake } = await setupSpeech();
      act(() => {
        fake.emit({
          type: 'calibrationWindow',
          epoch: fake.command('speech'),
          phase: 'speech',
          frames: 960,
          ...sample,
        });
        windows(fake, 'speech', 150, 0.03);
        result.current.finishReading();
      });
      expect(result.current.state.phase).toBe('refused');
      expect(result.current.state.result?.kind).toBe('refused');
      expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
    }
  );

  it.each([
    'cancelled',
    'configuration',
    'capture-lost',
    'processor-error',
    'competing-test',
  ] as const)('invalidates current evidence on owner %s', async (reason) => {
    const { result, fake } = await setupSpeech();
    act(() => {
      fake.invalidate(reason);
      windows(fake, 'speech', 150, 0.03);
      result.current.finishReading();
    });
    expect(result.current.state.result?.kind).not.toBe('recommended');
    expect(result.current.state.phase).not.toBe('speech');
  });
});

describe('useGateCalibration eligibility and proposal identity', () => {
  it.each([
    { noiseCancellation: false },
    { echoCancellation: false },
    { musicMode: true },
    { autoGainControl: true },
    { inputVolume: 80 },
  ])('does not acquire with unapplied capture changes %j', async (draft) => {
    useDraftSettingsStore.getState().batchSetAudioDrafts(draft);
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    expect(result.current.state.canStart).toBe(false);
    await act(async () => {
      await result.current.start();
    });
    expect(fake.owner.startCalibration).not.toHaveBeenCalled();
  });

  it('rechecks dirty processing synchronously before Start without a rerender', async () => {
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    const start = result.current.start;
    await act(async () => {
      useDraftSettingsStore.getState().setAudioDraft('inputVolume', 80);
      await start();
    });
    expect(fake.owner.startCalibration).not.toHaveBeenCalled();
  });

  it('blocks applied effective AGC even when a draft turns it off', async () => {
    useAudioSettingsStore.setState({ autoGainControl: true });
    useDraftSettingsStore.getState().initialize();
    useDraftSettingsStore
      .getState()
      .batchSetAudioDrafts({ autoGainControl: false, noiseGateMode: 'manualCalibrate' });
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    await act(async () => {
      await result.current.start();
    });
    expect(fake.owner.startCalibration).not.toHaveBeenCalled();
  });

  it('allows stored AGC in Music Mode when effective AGC is off', async () => {
    useAudioSettingsStore.setState({ musicMode: true, autoGainControl: true });
    useDraftSettingsStore.getState().initialize();
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    expect(result.current.state.canStart).toBe(true);
    await act(async () => {
      await result.current.start();
    });
    expect(fake.owner.startCalibration).toHaveBeenCalledTimes(1);
  });

  it('allows threshold-only drafts without applying them', async () => {
    useDraftSettingsStore.getState().setAudioDraft('noiseGateLevel', -42);
    const fake = fakeOwner();
    const { result } = renderHook(() => useGateCalibration(fake.owner));
    expect(result.current.state.canStart).toBe(true);
    await act(async () => {
      await result.current.start();
    });
    expect(fake.owner.startCalibration).toHaveBeenCalledTimes(1);
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-50);
  });

  it('retains the finished owner version and writes the recommendation once to drafts until Apply', async () => {
    const { result, fake } = await setupReview();
    expect(fake.session.finish).toHaveBeenCalledTimes(1);
    const setter = vi.spyOn(draftSettings, 'setDraftAudioSetting');
    const apply = vi.spyOn(useDraftSettingsStore.getState(), 'apply');
    const persist = vi.spyOn(useAudioSettingsStore.getState(), 'setNoiseGateLevel');
    act(() => {
      result.current.useThreshold();
      result.current.useThreshold();
    });
    expect(setter).toHaveBeenCalledExactlyOnceWith('noiseGateLevel', -57);
    expect(apply).not.toHaveBeenCalled();
    expect(persist).not.toHaveBeenCalled();
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBe(-57);
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-50);
    await act(async () => {
      await useDraftSettingsStore.getState().apply();
    });
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-57);
  });

  it.each([
    ['input device', () => useVoiceStore.setState({ audioInputDeviceId: 'replacement-input' })],
    ['output device', () => useVoiceStore.setState({ audioOutputDeviceId: 'replacement-output' })],
    ['applied level', () => useAudioSettingsStore.setState({ inputVolume: 80 })],
    [
      'draft processing',
      () => useDraftSettingsStore.getState().setAudioDraft('echoCancellation', false),
    ],
    ['mode', () => useDraftSettingsStore.getState().setAudioDraft('noiseGateMode', 'off')],
    ['Revert', () => useDraftSettingsStore.getState().revert()],
    [
      'new visit',
      () => {
        useDraftSettingsStore.getState().teardown();
        useDraftSettingsStore.getState().initialize();
      },
    ],
  ] as const)(
    'rejects %s changes immediately before Use threshold without a render',
    async (_label, change) => {
      const { result } = await setupReview();
      const useThreshold = result.current.useThreshold;
      const setter = vi.spyOn(draftSettings, 'setDraftAudioSetting');
      act(() => {
        change();
        useThreshold();
      });
      expect(setter).not.toHaveBeenCalled();
      expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-50);
      expect(result.current.state.result?.kind).not.toBe('recommended');
    }
  );

  it('rechecks the owner version at Use even without an activity notification', async () => {
    const { result, fake } = await setupReview();
    const setter = vi.spyOn(draftSettings, 'setDraftAudioSetting');
    act(() => {
      fake.changeVersionWithoutNotification();
      result.current.useThreshold();
    });
    expect(setter).not.toHaveBeenCalled();
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
  });

  it.each(['stop', 'replacement'] as const)(
    'invalidates review on external %s notification without rerendering the owner or clicking Use',
    async (activity) => {
      const { result, fake } = await setupReview();
      const setter = vi.spyOn(draftSettings, 'setDraftAudioSetting');
      expect(fake.activityListenerCount()).toBeGreaterThan(0);
      act(() => {
        if (activity === 'stop') fake.owner.stopTest();
        else fake.replaceActivity();
      });
      expect(result.current.state.phase).not.toBe('review');
      expect(result.current.state.result?.kind).not.toBe('recommended');
      expect(setter).not.toHaveBeenCalled();
      expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
    }
  );

  it('unsubscribes owner activity notifications when the settings controller unmounts', async () => {
    const { unmount, fake } = await setupReview();
    expect(fake.activityListenerCount()).toBeGreaterThan(0);
    unmount();
    expect(fake.activityListenerCount()).toBe(0);
    const setter = vi.spyOn(draftSettings, 'setDraftAudioSetting');
    act(() => {
      fake.replaceActivity();
    });
    expect(setter).not.toHaveBeenCalled();
  });

  it.each(['configuration', 'device', 'Revert', 'activity'] as const)(
    'clears a completed refusal summary immediately on %s',
    async (change) => {
      const { result, fake } = await setupSpeech();
      act(() => {
        windows(fake, 'speech', 150, 0.001);
      });
      expect(result.current.state.canFinishReading).toBe(true);
      act(() => {
        result.current.finishReading();
      });
      expect(result.current.state.phase).toBe('refused');
      expect(result.current.state.result).toMatchObject({ kind: 'refused', reason: 'overlap' });
      expect(result.current.state.result?.summary).not.toBeNull();
      expect(fake.session.finish).toHaveBeenCalledTimes(1);
      act(() => {
        if (change === 'configuration') useAudioSettingsStore.setState({ echoCancellation: false });
        else if (change === 'device')
          useVoiceStore.setState({ audioInputDeviceId: 'replacement-input' });
        else if (change === 'Revert') useDraftSettingsStore.getState().revert();
        else fake.replaceActivity();
      });
      expect(result.current.state.result).toBeNull();
      expect(result.current.state.phase).not.toBe('refused');
      expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
    }
  );
});
