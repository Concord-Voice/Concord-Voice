import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
  useAudioSettingsStore.setState({
    advancedMode: false,
    noiseCancellation: true,
    echoCancellation: true,
    autoGainControl: true,
    noiseGateMode: 'auto',
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
    quietBoost: false,
    quietBoostThreshold: -40,
    networkType: 'auto',
    packetLossWarningThreshold: 5,
  });
});

describe('audioSettingsStore', () => {
  it('has correct defaults', () => {
    const s = useAudioSettingsStore.getState();
    expect(s.advancedMode).toBe(false);
    expect(s.noiseCancellation).toBe(true);
    expect(s.echoCancellation).toBe(true);
    expect(s.autoGainControl).toBe(true);
    expect(s.noiseGateMode).toBe('auto');
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
    useAudioSettingsStore.getState().setNoiseGateMode('manual');
    expect(useAudioSettingsStore.getState().noiseGateMode).toBe('manual');
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
});
