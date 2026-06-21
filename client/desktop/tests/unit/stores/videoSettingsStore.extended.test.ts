import { useVideoSettingsStore, VIDEO_QUALITY_PRESETS } from '@/renderer/stores/videoSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
});

describe('videoSettingsStore — extended coverage', () => {
  it('has correct default values for all fields', () => {
    const s = useVideoSettingsStore.getState();
    expect(s.preferredVideoCodec).toBeNull();
    expect(s.cameraPriority).toBe('medium');
    expect(s.screenResolution).toBe('source');
    expect(s.screenFrameRate).toBe(30);
    expect(s.screenContentType).toBe('auto');
    expect(s.screenSharePriority).toBe('medium');
    expect(s.screenShareBitrate).toBe(0);
    expect(s.degradationPreference).toBe('balanced');
    expect(s.scalabilityMode).toBe('auto');
    expect(s.hardwareAcceleration).toBe(true);
    expect(s.hdrEncoding).toBe(false);
    expect(s.systemHdr).toBe(false);
    expect(s.codecCapabilities).toEqual([]);
    expect(s.gpuInfo).toBeNull();
  });

  describe('setPreferredVideoCodec', () => {
    it('sets codec to a specific value', () => {
      useVideoSettingsStore.getState().setPreferredVideoCodec('video/VP9');
      expect(useVideoSettingsStore.getState().preferredVideoCodec).toBe('video/VP9');
    });

    it('clears codec with null', () => {
      useVideoSettingsStore.getState().setPreferredVideoCodec('video/VP9');
      useVideoSettingsStore.getState().setPreferredVideoCodec(null);
      expect(useVideoSettingsStore.getState().preferredVideoCodec).toBeNull();
    });
  });

  describe('setCameraPriority', () => {
    it('sets camera priority', () => {
      useVideoSettingsStore.getState().setCameraPriority('high');
      expect(useVideoSettingsStore.getState().cameraPriority).toBe('high');
    });
  });

  describe('setScreenResolution', () => {
    it('sets screen resolution', () => {
      useVideoSettingsStore.getState().setScreenResolution('1080p');
      expect(useVideoSettingsStore.getState().screenResolution).toBe('1080p');
    });
  });

  describe('setScreenFrameRate', () => {
    it('sets screen frame rate', () => {
      useVideoSettingsStore.getState().setScreenFrameRate(60);
      expect(useVideoSettingsStore.getState().screenFrameRate).toBe(60);
    });
  });

  describe('setScreenContentType', () => {
    it('sets screen content type', () => {
      useVideoSettingsStore.getState().setScreenContentType('motion');
      expect(useVideoSettingsStore.getState().screenContentType).toBe('motion');
    });
  });

  describe('setScreenSharePriority', () => {
    it('sets screen share priority', () => {
      useVideoSettingsStore.getState().setScreenSharePriority('high');
      expect(useVideoSettingsStore.getState().screenSharePriority).toBe('high');
    });
  });

  describe('setScreenShareBitrate', () => {
    it('sets screen share bitrate', () => {
      useVideoSettingsStore.getState().setScreenShareBitrate(5_000_000);
      expect(useVideoSettingsStore.getState().screenShareBitrate).toBe(5_000_000);
    });
  });

  describe('setDegradationPreference', () => {
    it('sets degradation preference to maintain-framerate', () => {
      useVideoSettingsStore.getState().setDegradationPreference('maintain-framerate');
      expect(useVideoSettingsStore.getState().degradationPreference).toBe('maintain-framerate');
    });

    it('sets degradation preference to maintain-resolution', () => {
      useVideoSettingsStore.getState().setDegradationPreference('maintain-resolution');
      expect(useVideoSettingsStore.getState().degradationPreference).toBe('maintain-resolution');
    });
  });

  describe('setScalabilityMode', () => {
    it('sets scalability mode to L1T3', () => {
      useVideoSettingsStore.getState().setScalabilityMode('L1T3');
      expect(useVideoSettingsStore.getState().scalabilityMode).toBe('L1T3');
    });

    it('sets scalability mode to L3T3', () => {
      useVideoSettingsStore.getState().setScalabilityMode('L3T3');
      expect(useVideoSettingsStore.getState().scalabilityMode).toBe('L3T3');
    });
  });

  describe('setHardwareAcceleration', () => {
    it('disables hardware acceleration', () => {
      useVideoSettingsStore.getState().setHardwareAcceleration(false);
      expect(useVideoSettingsStore.getState().hardwareAcceleration).toBe(false);
    });
  });

  describe('setHdrEncoding', () => {
    it('enables HDR encoding', () => {
      useVideoSettingsStore.getState().setHdrEncoding(true);
      expect(useVideoSettingsStore.getState().hdrEncoding).toBe(true);
    });
  });

  describe('persistence', () => {
    it('excludes systemHdr from persisted state', () => {
      useVideoSettingsStore.setState({ systemHdr: true });
      const stored = JSON.parse(localStorage.getItem('concord:video-settings') || '{}');
      // systemHdr should not be in persisted state (partialize excludes it)
      expect(stored.state?.systemHdr).toBeUndefined();
    });
  });

  describe('VIDEO_QUALITY_PRESETS', () => {
    it('includes 4K presets', () => {
      expect(VIDEO_QUALITY_PRESETS).toHaveProperty('4K30');
      expect(VIDEO_QUALITY_PRESETS).toHaveProperty('4K60');
    });

    it('system preset has zero dimensions (auto-detected)', () => {
      const system = VIDEO_QUALITY_PRESETS['system'];
      expect(system.width).toBe(0);
      expect(system.height).toBe(0);
      expect(system.frameRate).toBe(0);
    });

    it('4K60 has highest bitrate', () => {
      const bitrates = Object.values(VIDEO_QUALITY_PRESETS).map((p) => p.maxBitrate);
      expect(Math.max(...bitrates)).toBe(VIDEO_QUALITY_PRESETS['4K60'].maxBitrate);
    });
  });
});
