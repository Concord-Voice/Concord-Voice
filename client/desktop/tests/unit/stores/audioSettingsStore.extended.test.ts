import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
  // Reset to actual defaults from the store source (not the base test's modified values)
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
    fecHeadroom: true,
    opusNack: false,
    adaptivePtime: false,
    audioPriority: 'medium',
    inputVolume: 100,
    outputVolume: 100,
    quietBoost: false,
    quietBoostThreshold: -35,
    networkType: 'auto',
    packetLossWarningThreshold: 3,
  });
});

describe('audioSettingsStore — extended coverage', () => {
  // --- Clamping behavior ---

  describe('noiseGateLevel clamping', () => {
    it('clamps values below -80 to -80', () => {
      useAudioSettingsStore.getState().setNoiseGateLevel(-100);
      expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-80);
    });

    it('clamps values above -20 to -20', () => {
      useAudioSettingsStore.getState().setNoiseGateLevel(0);
      expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-20);
    });

    it('accepts values within range', () => {
      useAudioSettingsStore.getState().setNoiseGateLevel(-40);
      expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-40);
    });

    it('accepts boundary value -80', () => {
      useAudioSettingsStore.getState().setNoiseGateLevel(-80);
      expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-80);
    });

    it('accepts boundary value -20', () => {
      useAudioSettingsStore.getState().setNoiseGateLevel(-20);
      expect(useAudioSettingsStore.getState().noiseGateLevel).toBe(-20);
    });
  });

  describe('inputVolume clamping', () => {
    it('clamps values below 0 to 0', () => {
      useAudioSettingsStore.getState().setInputVolume(-10);
      expect(useAudioSettingsStore.getState().inputVolume).toBe(0);
    });

    it('clamps values above 200 to 200', () => {
      useAudioSettingsStore.getState().setInputVolume(300);
      expect(useAudioSettingsStore.getState().inputVolume).toBe(200);
    });

    it('accepts boundary value 0', () => {
      useAudioSettingsStore.getState().setInputVolume(0);
      expect(useAudioSettingsStore.getState().inputVolume).toBe(0);
    });

    it('accepts boundary value 200', () => {
      useAudioSettingsStore.getState().setInputVolume(200);
      expect(useAudioSettingsStore.getState().inputVolume).toBe(200);
    });
  });

  describe('outputVolume clamping', () => {
    it('clamps values below 0 to 0', () => {
      useAudioSettingsStore.getState().setOutputVolume(-5);
      expect(useAudioSettingsStore.getState().outputVolume).toBe(0);
    });

    it('clamps values above 200 to 200', () => {
      useAudioSettingsStore.getState().setOutputVolume(999);
      expect(useAudioSettingsStore.getState().outputVolume).toBe(200);
    });
  });

  describe('quietBoostThreshold clamping', () => {
    it('clamps values below -50 to -50', () => {
      useAudioSettingsStore.getState().setQuietBoostThreshold(-60);
      expect(useAudioSettingsStore.getState().quietBoostThreshold).toBe(-50);
    });

    it('clamps values above -20 to -20', () => {
      useAudioSettingsStore.getState().setQuietBoostThreshold(0);
      expect(useAudioSettingsStore.getState().quietBoostThreshold).toBe(-20);
    });

    it('accepts value within range', () => {
      useAudioSettingsStore.getState().setQuietBoostThreshold(-30);
      expect(useAudioSettingsStore.getState().quietBoostThreshold).toBe(-30);
    });
  });

  // --- Setters not covered by base test ---

  describe('setFecHeadroom', () => {
    it('sets fecHeadroom to true', () => {
      useAudioSettingsStore.getState().setFecHeadroom(false);
      expect(useAudioSettingsStore.getState().fecHeadroom).toBe(false);
      useAudioSettingsStore.getState().setFecHeadroom(true);
      expect(useAudioSettingsStore.getState().fecHeadroom).toBe(true);
    });
  });

  describe('setOpusNack', () => {
    it('sets opusNack', () => {
      useAudioSettingsStore.getState().setOpusNack(true);
      expect(useAudioSettingsStore.getState().opusNack).toBe(true);
    });
  });

  describe('setAdaptivePtime', () => {
    it('sets adaptivePtime', () => {
      useAudioSettingsStore.getState().setAdaptivePtime(true);
      expect(useAudioSettingsStore.getState().adaptivePtime).toBe(true);
    });
  });

  describe('setSilenceDetection', () => {
    it('sets silenceDetection', () => {
      useAudioSettingsStore.getState().setSilenceDetection(true);
      expect(useAudioSettingsStore.getState().silenceDetection).toBe(true);
    });
  });

  describe('setPacketLossWarningThreshold', () => {
    it('sets packet loss warning threshold', () => {
      useAudioSettingsStore.getState().setPacketLossWarningThreshold(10);
      expect(useAudioSettingsStore.getState().packetLossWarningThreshold).toBe(10);
    });
  });
});
