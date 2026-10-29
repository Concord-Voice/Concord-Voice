import React from 'react';
import { act, fireEvent, render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import AudioConfigSection from '@/renderer/components/Settings/AudioConfigSection';
import type {
  MicCalibrationCallbacks,
  MicCalibrationSession,
  UseMicTestReturn,
} from '@/renderer/hooks/device/useMicTest';
import type { CalibrationEvent, CalibrationPhase } from '@/renderer/services/voice/micProcessor';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useDraftSettingsStore } from '@/renderer/stores/ui/draftSettingsStore';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { useVoiceStore } from '@/renderer/stores/voice/voiceStore';
import { resetAllStores } from '../../../helpers/store-helpers';

const boundary = vi.hoisted(() => ({
  owner: null as UseMicTestReturn | null,
  output: {
    isTesting: false,
    error: null,
    playTestTone: vi.fn(async () => {}),
    stopTest: vi.fn(),
  },
}));

vi.mock('@/renderer/hooks/device/useMicTest', () => ({
  useMicTest: () => {
    if (!boundary.owner) throw new Error('Set the fake microphone owner before rendering');
    return boundary.owner;
  },
}));
vi.mock('@/renderer/hooks/device/useOutputTest', () => ({
  useOutputTest: () => boundary.output,
}));
// Device enumeration is an OS boundary; selections still update the actual voice store.
vi.mock('@/renderer/components/Voice/DeviceSelector', () => ({
  default: ({ kind }: { kind: 'audioinput' | 'audiooutput' }) => {
    const input = kind === 'audioinput';
    const selected = useVoiceStore((state) =>
      input ? state.audioInputDeviceId : state.audioOutputDeviceId
    );
    const setDevice = useVoiceStore((state) =>
      input ? state.setAudioInputDevice : state.setAudioOutputDevice
    );
    return (
      <select
        aria-label={input ? 'Microphone' : 'Speaker'}
        value={selected ?? ''}
        onChange={(event) => setDevice(event.currentTarget.value)}
      >
        <option value="">Default</option>
        <option value="replacement-device">Replacement device</option>
      </select>
    );
  },
}));

function fakeOwner() {
  let callbacks: MicCalibrationCallbacks | undefined;
  let activity = 0;
  const listeners = new Set<() => void>();
  const advanceActivity = () => {
    ++activity;
    for (const listener of listeners) listener();
    return activity;
  };
  const session: MicCalibrationSession = {
    sampleRate: 48_000,
    setPhase: vi.fn((_epoch: number, _phase: CalibrationPhase) => {}),
    finish: vi.fn(() => {
      owner.isTesting = false;
      return advanceActivity();
    }),
  };
  const owner: UseMicTestReturn = {
    isTesting: false,
    isPending: false,
    dbfsLevel: -Infinity,
    inputOverloaded: false,
    dynamicGateStatus: null,
    error: null,
    startTest: vi.fn(async () => {
      advanceActivity();
      callbacks?.onInvalidated('competing-test');
      owner.isTesting = true;
    }),
    stopTest: vi.fn(() => {
      owner.isTesting = false;
      advanceActivity();
      callbacks?.onInvalidated('cancelled');
    }),
    startCalibration: vi.fn(async (next: MicCalibrationCallbacks) => {
      advanceActivity();
      callbacks = next;
      owner.isTesting = true;
      return session;
    }),
    getActivityVersion: () => activity,
    subscribeActivity: (listener) => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
  };
  return {
    owner,
    session,
    emit: (event: CalibrationEvent) => callbacks?.onEvent(event),
    epoch(phase: CalibrationPhase) {
      const command = vi.mocked(session.setPhase).mock.calls.findLast((call) => call[1] === phase);
      if (!command) throw new Error(`No ${phase} command issued`);
      return command[0];
    },
  };
}

type FakeOwner = ReturnType<typeof fakeOwner>;
let capture: FakeOwner;

// AudioConfigSection belongs to Settings; these buttons exercise its real shared draft actions.
function SettingsHarness() {
  const apply = useDraftSettingsStore((state) => state.apply);
  const revert = useDraftSettingsStore((state) => state.revert);
  return (
    <>
      <AudioConfigSection />
      <button onClick={() => void apply()}>Apply</button>
      <button onClick={revert}>Revert</button>
    </>
  );
}

function emitWindows(phase: 'quiet' | 'speech', count: number, peak: number) {
  const epoch = capture.epoch(phase);
  for (let index = 0; index < count; index++) {
    capture.emit({
      type: 'calibrationWindow',
      epoch,
      phase,
      peak,
      frames: 960,
      valid: true,
      overloaded: false,
    });
  }
}

async function chooseAuto(user = userEvent.setup()) {
  await user.selectOptions(screen.getByRole('combobox', { name: 'Noise Gate' }), 'autoCalibrate');
  return user;
}

async function finishCalibration(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: 'Start calibration' }));
  act(() => {
    const epoch = capture.epoch('quiet');
    capture.emit({ type: 'calibrationAck', epoch, phase: 'quiet' });
    emitWindows('quiet', 500, 10 ** (-60 / 20));
    capture.emit({ type: 'calibrationComplete', epoch, phase: 'quiet', frames: 480_000 });
  });
  await user.click(screen.getByRole('button', { name: 'Start reading' }));
  act(() => {
    capture.emit({ type: 'calibrationAck', epoch: capture.epoch('speech'), phase: 'speech' });
    emitWindows('speech', 150, 10 ** (-30 / 20));
  });
  await user.click(screen.getByRole('button', { name: 'Done reading' }));
  expect(screen.getByRole('button', { name: 'Use threshold' })).toBeEnabled();
}

beforeEach(() => {
  resetAllStores();
  boundary.output.isTesting = false;
  boundary.output.playTestTone.mockReset();
  boundary.output.stopTest.mockReset();
  useAudioSettingsStore.setState({
    advancedMode: true,
    autoGainControl: false,
    musicMode: false,
    noiseGateMode: 'off',
    noiseGateLevel: -50,
    noiseCancellation: true,
    echoCancellation: true,
    inputVolume: 100,
  });
  useVoiceStore.setState({ audioInputDeviceId: null, audioOutputDeviceId: null });
  useDraftSettingsStore.getState().initialize();
  useSettingsCollapsibleStore.getState().setSectionOpen('section-audio-config', true);
  capture = fakeOwner();
  boundary.owner = capture.owner;
});

describe('AudioConfigSection real calibration integration', () => {
  it('retains the Manual listening Test clipping warning after levels fall and clears it when Test stops', async () => {
    const { rerender } = render(<SettingsHarness />);
    const user = userEvent.setup();
    await user.selectOptions(
      screen.getByRole('combobox', { name: 'Noise Gate' }),
      'manualCalibrate'
    );
    await user.click(screen.getAllByRole('button', { name: /^Test$/ })[0]);
    capture.owner.inputOverloaded = true;
    capture.owner.dbfsLevel = 1;
    rerender(<SettingsHarness />);
    expect(screen.getByRole('meter', { name: 'Microphone level' })).toBeVisible();
    expect(screen.getByRole('alert')).toHaveTextContent(/reached or exceeded 0 dBFS/i);
    expect(screen.getByRole('alert')).toHaveTextContent(/clipping/i);
    expect(capture.owner.startCalibration).not.toHaveBeenCalled();

    capture.owner.dbfsLevel = -45;
    rerender(<SettingsHarness />);
    expect(screen.getByText(/Microphone level: [-−]45 dBFS/i)).toBeVisible();
    expect(screen.getByRole('alert')).toHaveTextContent(/clipping/i);

    await user.click(screen.getByRole('button', { name: 'Stop Testing' }));
    rerender(<SettingsHarness />);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(capture.owner.inputOverloaded).toBe(true);
    expect(capture.owner.isTesting).toBe(false);
  });

  it.each(['playing', 'pending'] as const)(
    'stops an existing %s speaker test before calibration acquisition',
    async (state) => {
      boundary.output.isTesting = state === 'playing';
      render(<SettingsHarness />);
      const user = await chooseAuto();
      if (state === 'pending') {
        // The output owner may still be waiting for its context/sink/play promise.
        // Its isTesting flag remains false until playback has actually started.
        boundary.output.playTestTone.mockImplementation(() => new Promise(() => {}));
        await user.click(screen.getAllByRole('button', { name: /^Test$/ }).at(-1)!);
        expect(boundary.output.playTestTone).toHaveBeenCalledTimes(1);
      }
      await user.click(screen.getByRole('button', { name: 'Start calibration' }));
      expect(boundary.output.stopTest).toHaveBeenCalledTimes(1);
      expect(capture.owner.startCalibration).toHaveBeenCalledTimes(1);
      expect(boundary.output.stopTest.mock.invocationCallOrder[0]).toBeLessThan(
        vi.mocked(capture.owner.startCalibration).mock.invocationCallOrder[0]
      );
    }
  );

  it.each(['acquiring', 'quiet', 'readyToRead', 'speech'] as const)(
    'prevents speaker Test from contaminating calibration during %s outside a call',
    async (phase) => {
      if (phase === 'acquiring') {
        vi.mocked(capture.owner.startCalibration).mockImplementation(() => new Promise(() => {}));
      }
      render(<SettingsHarness />);
      const user = await chooseAuto();
      await user.click(screen.getByRole('button', { name: 'Start calibration' }));
      if (phase === 'readyToRead' || phase === 'speech') {
        act(() => {
          const epoch = capture.epoch('quiet');
          capture.emit({ type: 'calibrationAck', epoch, phase: 'quiet' });
          emitWindows('quiet', 500, 10 ** (-60 / 20));
          capture.emit({ type: 'calibrationComplete', epoch, phase: 'quiet', frames: 480_000 });
        });
      }
      if (phase === 'speech') {
        await user.click(screen.getByRole('button', { name: 'Start reading' }));
      }
      expect(useVoiceStore.getState().localIsTesting).toBe(false);
      expect(screen.getByRole('button', { name: 'Cancel calibration' })).toBeEnabled();
      const speakerTest = screen.getAllByRole('button', { name: /^Test$/ }).at(-1)!;
      expect(speakerTest).toBeDisabled();
      await user.click(speakerTest);
      expect(boundary.output.playTestTone).not.toHaveBeenCalled();
    }
  );

  it('clears a finished proposal before starting speaker Test while preserving ordinary output playback', async () => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    await finishCalibration(user);
    const useThreshold = screen.getByRole('button', { name: 'Use threshold' });
    boundary.output.playTestTone.mockImplementation(async () => {
      // An event already queued against the old review cannot apply it once
      // the competing output owner starts, even before React commits the UI.
      fireEvent.click(useThreshold);
    });
    const speakerTest = screen.getAllByRole('button', { name: /^Test$/ }).at(-1)!;
    expect(speakerTest).toBeEnabled();
    await user.click(speakerTest);
    expect(boundary.output.playTestTone).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('button', { name: 'Use threshold' })).not.toBeInTheDocument();
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-50);
  });

  it('keeps ordinary speaker Test available before calibration starts', async () => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    const speakerTest = screen.getAllByRole('button', { name: /^Test$/ }).at(-1)!;
    expect(speakerTest).toBeEnabled();
    await user.click(speakerTest);
    expect(boundary.output.playTestTone).toHaveBeenCalledTimes(1);
    expect(capture.owner.startCalibration).not.toHaveBeenCalled();
  });

  it('selects Auto as a draft without starting capture, then explicitly starts the one Test owner', async () => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    expect(screen.getByRole('button', { name: 'Start calibration' })).toBeEnabled();
    expect(capture.owner.startCalibration).not.toHaveBeenCalled();
    expect(capture.owner.startTest).not.toHaveBeenCalled();
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('off');
    await user.click(screen.getByRole('button', { name: 'Start calibration' }));
    expect(capture.owner.startCalibration).toHaveBeenCalledTimes(1);
    expect(capture.owner.startTest).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Cancel calibration' })).toBeEnabled();
  });

  it.each([
    ['noiseCancellation', false],
    ['echoCancellation', false],
    ['inputVolume', 80],
    ['musicMode', true],
  ] as const)('blocks Start for unapplied %s', async (setting, value) => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    act(() => {
      useDraftSettingsStore.getState().setAudioDraft(setting, value);
    });
    const start = screen.getByRole('button', { name: 'Start calibration' });
    expect(start).toBeDisabled();
    expect(
      screen.getAllByText(/Apply.*(?:processing|level)|(?:processing|level).*Apply/i).length
    ).toBeGreaterThan(0);
    await user.click(start);
    expect(capture.owner.startCalibration).not.toHaveBeenCalled();
  });

  it('permits threshold-only edits and keeps the applied threshold until Apply', async () => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    fireEvent.change(screen.getByRole('slider', { name: 'Gate Threshold' }), {
      target: { value: '-44' },
    });
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBe(-44);
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-50);
    expect(screen.getByRole('button', { name: 'Start calibration' })).toBeEnabled();
    await user.click(screen.getByRole('button', { name: 'Start calibration' }));
    expect(capture.owner.startCalibration).toHaveBeenCalledTimes(1);
  });

  it('keeps Use threshold draft-only, Revert restores saved values, and Apply publishes the fixed Auto policy', async () => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    await finishCalibration(user);
    expect(capture.session.finish).toHaveBeenCalledTimes(1);
    expect(capture.owner.isTesting).toBe(false);
    await user.click(screen.getByRole('button', { name: 'Use threshold' }));
    expect(screen.getByRole('slider', { name: 'Gate Threshold' })).toHaveValue('-57');
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBe(-57);
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-50);
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('off');
    await user.click(screen.getByRole('button', { name: 'Revert' }));
    expect(screen.getByRole('combobox', { name: 'Noise Gate' })).toHaveValue('off');
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
    await chooseAuto(user);
    await finishCalibration(user);
    await user.click(screen.getByRole('button', { name: 'Use threshold' }));
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(screen.getByRole('slider', { name: 'Gate Threshold' })).toHaveValue('-57');
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-57);
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('autoCalibrate');
    expect(useDraftSettingsStore.getState().drafts.audio).toEqual({});
  });

  it('offers fixed modes only when effective AGC permits them, including Music Mode', () => {
    useAudioSettingsStore.setState({ autoGainControl: true, noiseGateMode: 'dynamic' });
    useDraftSettingsStore.getState().initialize();
    render(<SettingsHarness />);
    const select = screen.getByRole('combobox', { name: 'Noise Gate' });
    expect(
      within(select).queryByRole('option', { name: 'Auto Calibrate' })
    ).not.toBeInTheDocument();
    expect(
      within(select).queryByRole('option', { name: 'Manual Calibrate' })
    ).not.toBeInTheDocument();
    act(() => {
      useAudioSettingsStore.setState({ musicMode: true });
      useDraftSettingsStore.getState().initialize();
    });
    expect(within(select).getByRole('option', { name: 'Auto Calibrate' })).toBeInTheDocument();
    expect(within(select).getByRole('option', { name: 'Manual Calibrate' })).toBeInTheDocument();
    expect(capture.owner.startCalibration).not.toHaveBeenCalled();
  });

  it('invalidates a review when ordinary microphone Test replaces the calibration', async () => {
    render(<SettingsHarness />);
    const user = await chooseAuto();
    await finishCalibration(user);
    await user.click(screen.getAllByRole('button', { name: /^Test$/ })[0]);
    expect(capture.owner.startTest).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole('button', { name: 'Use threshold' })).not.toBeInTheDocument();
    expect(useDraftSettingsStore.getState().drafts.audio.noiseGateLevel).toBeUndefined();
    expect(capture.owner.startCalibration).toHaveBeenCalledTimes(1);
  });

  it('cancels capture and discards the review when Audio Configuration collapses', async () => {
    const { container } = render(<SettingsHarness />);
    const user = await chooseAuto();
    await user.click(screen.getByRole('button', { name: 'Start calibration' }));
    const section = container.querySelector<HTMLDetailsElement>('#section-audio-config');
    if (!section) throw new Error('Audio Configuration disclosure missing');
    act(() => {
      section.open = false;
      fireEvent(section, new Event('toggle', { bubbles: true }));
    });
    expect(capture.owner.stopTest).toHaveBeenCalled();
    expect(capture.owner.isTesting).toBe(false);
    act(() => {
      section.open = true;
      fireEvent(section, new Event('toggle', { bubbles: true }));
    });
    expect(screen.getByRole('button', { name: 'Start calibration' })).toBeEnabled();
    expect(screen.queryByRole('button', { name: 'Use threshold' })).not.toBeInTheDocument();
    expect(capture.owner.startCalibration).toHaveBeenCalledTimes(1);
  });
});
