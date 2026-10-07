import {
  effectiveMicLevelPercent,
  effectiveNoiseGateMode,
  useAudioSettingsStore,
} from '@/renderer/stores/audio/audioSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
  useAudioSettingsStore.setState({
    advancedMode: false,
    noiseCancellation: true,
    echoCancellation: true,
    autoGainControl: true,
    noiseGateMode: 'dynamic',
    noiseGateLevel: -50,
    musicMode: false,
    frameSize: 0,
    silenceDetection: false,
    stereoOverride: null,
    inlineFec: true,
    fecHeadroom: false,
    opusNack: false,
    adaptivePtime: false,
    audioPriority: 'off',
    inputVolume: 100,
    outputVolume: 100,
    perParticipantVolume: {},
    perScreenShareVolume: {},
    quietBoost: false,
    quietBoostThreshold: -40,
    networkType: 'auto',
    packetLossWarningThreshold: 5,
  });
});

describe('audioSettingsStore', () => {
  describe('effective microphone level', () => {
    it.each([100, 200])(
      'uses unity while AGC is on with positive saved level %i',
      (inputVolume) => {
        expect(
          effectiveMicLevelPercent({ musicMode: false, autoGainControl: true, inputVolume })
        ).toBe(100);
      }
    );

    it('preserves a persisted zero as silence while AGC is on', async () => {
      localStorage.setItem(
        'concord:audio-advanced',
        JSON.stringify({
          state: { inputVolume: 0, autoGainControl: true, musicMode: false },
          version: 2,
        })
      );

      await useAudioSettingsStore.persist.rehydrate();

      expect(useAudioSettingsStore.getState().inputVolume).toBe(0);
      expect(effectiveMicLevelPercent(useAudioSettingsStore.getState())).toBe(0);
    });

    it.each([0, 100, 200])('uses saved level %i while AGC is off', (inputVolume) => {
      expect(
        effectiveMicLevelPercent({ musicMode: false, autoGainControl: false, inputVolume })
      ).toBe(inputVolume);
    });

    it('uses the retained manual level in Music Mode even when AGC is stored on', () => {
      expect(
        effectiveMicLevelPercent({ musicMode: true, autoGainControl: true, inputVolume: 200 })
      ).toBe(200);
    });

    it.each([
      [-1, 0],
      [201, 200],
      [Number.NaN, 100],
      [Number.POSITIVE_INFINITY, 100],
      [Number.NEGATIVE_INFINITY, 100],
      ['150' as unknown as number, 100],
    ])('normalizes invalid or out-of-range manual level %s to %i', (inputVolume, expected) => {
      expect(
        effectiveMicLevelPercent({ musicMode: true, autoGainControl: false, inputVolume })
      ).toBe(expected);
    });
  });

  describe('persisted microphone level hydration', () => {
    it.each([
      ['non-finite NaN', Number.NaN],
      ['positive infinity', Number.POSITIVE_INFINITY],
      ['negative infinity', Number.NEGATIVE_INFINITY],
      ['non-numeric text', 'loud'],
    ])(
      'normalizes %s without discarding other same-version settings',
      async (_label, inputVolume) => {
        localStorage.setItem(
          'concord:audio-advanced',
          JSON.stringify({
            state: { inputVolume, advancedMode: true, noiseCancellation: false },
            version: 2,
          })
        );

        await useAudioSettingsStore.persist.rehydrate();

        const state = useAudioSettingsStore.getState();
        expect(state.inputVolume).toBe(100);
        expect(state.advancedMode).toBe(true);
        expect(state.noiseCancellation).toBe(false);
      }
    );

    it('clamps finite persisted manual levels without changing the stored value under AGC', async () => {
      localStorage.setItem(
        'concord:audio-advanced',
        JSON.stringify({
          state: { inputVolume: 250, autoGainControl: true, musicMode: false },
          version: 2,
        })
      );

      await useAudioSettingsStore.persist.rehydrate();

      expect(useAudioSettingsStore.getState().inputVolume).toBe(200);
      expect(effectiveMicLevelPercent(useAudioSettingsStore.getState())).toBe(100);
    });
  });

  it('has correct defaults', () => {
    const s = useAudioSettingsStore.getInitialState();
    expect(s.advancedMode).toBe(false);
    expect(s.noiseCancellation).toBe(true);
    expect(s.echoCancellation).toBe(true);
    expect(s.autoGainControl).toBe(true);
    expect(s.noiseGateMode).toBe('dynamic');
    expect(s.inputVolume).toBe(100);
    expect(s.outputVolume).toBe(100);
  });

  it('toggles advancedMode', () => {
    useAudioSettingsStore.getState().setAdvancedMode(true);
    expect(useAudioSettingsStore.getState().advancedMode).toBe(true);
  });

  it('sets noise cancellation', () => {
    useAudioSettingsStore.getState().setNoiseCancellation(false);
    expect(useAudioSettingsStore.getState().noiseCancellation).toBe(false);
  });

  it('sets echo cancellation', () => {
    useAudioSettingsStore.getState().setEchoCancellation(false);
    expect(useAudioSettingsStore.getState().echoCancellation).toBe(false);
  });

  it('sets auto gain control', () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    expect(useAudioSettingsStore.getState().autoGainControl).toBe(false);
  });

  it('sets noise gate mode', () => {
    useAudioSettingsStore.getState().setAutoGainControl(false);
    useAudioSettingsStore.getState().setNoiseGateMode('manualCalibrate');
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('manualCalibrate');
  });

  it('normalizes direct mode choices against effective AGC', () => {
    useAudioSettingsStore.getState().setAutoGainControl(true);
    useAudioSettingsStore.getState().setNoiseGateMode('manualCalibrate');
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('dynamic');

    useAudioSettingsStore.getState().setNoiseGateMode('off');
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('off');

    useAudioSettingsStore.getState().setMusicMode(true);
    useAudioSettingsStore.getState().setNoiseGateMode('off');
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('off');
    useAudioSettingsStore.getState().setNoiseGateMode('manualCalibrate');
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('manualCalibrate');
  });

  it.each([
    [{ musicMode: false, autoGainControl: true, noiseGateMode: 'manualCalibrate' }, 'dynamic'],
    [
      { musicMode: true, autoGainControl: true, noiseGateMode: 'manualCalibrate' },
      'manualCalibrate',
    ],
    [{ musicMode: false, autoGainControl: false, noiseGateMode: 'off' }, 'off'],
  ] as const)('resolves effective gate mode for %j', (settings, expected) => {
    expect(effectiveNoiseGateMode(settings)).toBe(expected);
  });

  it('updates capture settings atomically for subscribers', () => {
    const listener = vi.fn();
    const unsubscribe = useAudioSettingsStore.subscribe(listener);
    useAudioSettingsStore.getState().setCaptureGateSettings({
      autoGainControl: false,
      musicMode: true,
      noiseGateMode: 'manualCalibrate',
      noiseGateLevel: -42,
    });
    unsubscribe();

    expect(listener).toHaveBeenCalledTimes(1);
    expect(useAudioSettingsStore.getState()).toMatchObject({
      autoGainControl: false,
      musicMode: true,
      noiseGateMode: 'manualCalibrate',
      noiseGateLevel: -42,
    });
  });

  describe('noise gate persisted hydration', () => {
    it.each([
      [0, 'auto', false, 'off'],
      [1, 'auto', false, 'off'],
      [2, 'auto', false, 'off'],
      [2, 'manual', false, 'manualCalibrate'],
      [2, 'manual', true, 'dynamic'],
    ] as const)(
      'migrates v%i legacy %s with AGC=%s to %s',
      async (version, mode, agc, expected) => {
        localStorage.setItem(
          'concord:audio-advanced',
          JSON.stringify({
            state: {
              noiseGateMode: mode,
              autoGainControl: agc,
              musicMode: false,
              noiseGateLevel: -61,
              inputVolume: 0,
            },
            version,
          })
        );
        await useAudioSettingsStore.persist.rehydrate();
        expect(useAudioSettingsStore.getState().noiseGateMode).toBe(expected);
        expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-61);
        expect(useAudioSettingsStore.getState().inputVolume).toBe(0);
      }
    );

    it.each([
      ['invalid mode', { noiseGateMode: 'auto' }],
      ['null mode', { noiseGateMode: null }],
      ['unavailable auto calibration', { noiseGateMode: 'autoCalibrate' }],
      ['invalid threshold', { noiseGateLevel: Number.POSITIVE_INFINITY }],
      ['invalid AGC boolean', { autoGainControl: 'yes' }],
      ['invalid Music Mode boolean', { musicMode: 1 }],
    ])('sanitizes %s in a v3 snapshot', async (_label, state) => {
      localStorage.setItem('concord:audio-advanced', JSON.stringify({ state, version: 3 }));
      await useAudioSettingsStore.persist.rehydrate();
      const hydrated = useAudioSettingsStore.getState();
      if ('noiseGateMode' in state) expect(hydrated.noiseGateMode).toBe('off');
      if ('noiseGateLevel' in state) expect(hydrated.noiseGateLevel).toBe(-50);
      if ('autoGainControl' in state) expect(hydrated.autoGainControl).toBe(true);
      if ('musicMode' in state) expect(hydrated.musicMode).toBe(false);
    });
  });

  it('sets noise gate level', () => {
    useAudioSettingsStore.getState().setNoiseGateLevel(-30);
    expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-30);
  });

  it('sets music mode', () => {
    useAudioSettingsStore.getState().setMusicMode(true);
    expect(useAudioSettingsStore.getState().musicMode).toBe(true);
  });

  it('sets frame size', () => {
    useAudioSettingsStore.getState().setFrameSize(20);
    expect(useAudioSettingsStore.getState().frameSize).toBe(20);
  });

  it('sets inline FEC', () => {
    useAudioSettingsStore.getState().setInlineFec(false);
    expect(useAudioSettingsStore.getState().inlineFec).toBe(false);
  });

  it('sets stereo override', () => {
    useAudioSettingsStore.getState().setStereoOverride(true);
    expect(useAudioSettingsStore.getState().stereoOverride).toBe(true);
  });

  it('sets input volume', () => {
    useAudioSettingsStore.getState().setInputVolume(150);
    expect(useAudioSettingsStore.getState().inputVolume).toBe(150);
  });

  it('sets output volume', () => {
    useAudioSettingsStore.getState().setOutputVolume(50);
    expect(useAudioSettingsStore.getState().outputVolume).toBe(50);
  });

  it('sets quiet boost', () => {
    useAudioSettingsStore.getState().setQuietBoost(true);
    expect(useAudioSettingsStore.getState().quietBoost).toBe(true);
  });

  it('sets network type', () => {
    useAudioSettingsStore.getState().setNetworkType('wired');
    expect(useAudioSettingsStore.getState().networkType).toBe('wired');
  });

  it('sets audio priority', () => {
    useAudioSettingsStore.getState().setAudioPriority('high');
    expect(useAudioSettingsStore.getState().audioPriority).toBe('high');
  });

  describe('per-participant volume', () => {
    it('defaults to an empty record', () => {
      expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({});
    });

    it('sets a participant volume', () => {
      useAudioSettingsStore.getState().setParticipantVolume('user-1', 75);
      expect(useAudioSettingsStore.getState().perParticipantVolume['user-1']).toBe(75);
    });

    it('clamps below 0 to 0', () => {
      useAudioSettingsStore.getState().setParticipantVolume('user-1', -10);
      expect(useAudioSettingsStore.getState().perParticipantVolume['user-1']).toBe(0);
    });

    it('clamps above 200 to 200', () => {
      useAudioSettingsStore.getState().setParticipantVolume('user-1', 500);
      expect(useAudioSettingsStore.getState().perParticipantVolume['user-1']).toBe(200);
    });

    it('setting one participant does not disturb others', () => {
      useAudioSettingsStore.getState().setParticipantVolume('user-1', 50);
      useAudioSettingsStore.getState().setParticipantVolume('user-2', 150);
      expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({
        'user-1': 50,
        'user-2': 150,
      });
    });

    it('clearParticipantVolume removes the entry', () => {
      useAudioSettingsStore.getState().setParticipantVolume('user-1', 50);
      useAudioSettingsStore.getState().clearParticipantVolume('user-1');
      expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({});
    });

    it('clearParticipantVolume on a missing key is a no-op', () => {
      useAudioSettingsStore.getState().setParticipantVolume('user-1', 75);
      useAudioSettingsStore.getState().clearParticipantVolume('user-nonexistent');
      expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({ 'user-1': 75 });
    });
  });

  describe('perScreenShareVolume (#2162)', () => {
    it('sets and clamps screenshare volume independently of participant volume', () => {
      const s = useAudioSettingsStore.getState();
      s.setParticipantVolume('user-1', 50);
      s.setScreenShareVolume('user-1', 175);
      expect(useAudioSettingsStore.getState().perScreenShareVolume['user-1']).toBe(175);
      expect(useAudioSettingsStore.getState().perParticipantVolume['user-1']).toBe(50);
      s.setScreenShareVolume('user-1', 999);
      expect(useAudioSettingsStore.getState().perScreenShareVolume['user-1']).toBe(200);
      s.setScreenShareVolume('user-1', -10);
      expect(useAudioSettingsStore.getState().perScreenShareVolume['user-1']).toBe(0);
    });

    it('clears one and all screenshare volumes', () => {
      const s = useAudioSettingsStore.getState();
      s.setScreenShareVolume('a', 120);
      s.setScreenShareVolume('b', 80);
      s.clearScreenShareVolume('a');
      expect('a' in useAudioSettingsStore.getState().perScreenShareVolume).toBe(false);
      s.clearAllScreenShareVolumes();
      expect(useAudioSettingsStore.getState().perScreenShareVolume).toEqual({});
    });

    it('clearScreenShareVolume on a missing key is a no-op', () => {
      useAudioSettingsStore.getState().setScreenShareVolume('user-1', 75);
      useAudioSettingsStore.getState().clearScreenShareVolume('user-nonexistent');
      expect(useAudioSettingsStore.getState().perScreenShareVolume).toEqual({ 'user-1': 75 });
    });
  });
});
