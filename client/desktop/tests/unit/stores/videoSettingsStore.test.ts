import { useVideoSettingsStore, VIDEO_QUALITY_PRESETS } from '@/renderer/stores/videoSettingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  localStorage.clear();
});

describe('videoSettingsStore', () => {
  it('has correct defaults', () => {
    const s = useVideoSettingsStore.getState();
    expect(s.videoAdvancedMode).toBe(false);
    expect(s.cameraPreset).toBe('system');
  });

  it('toggles advancedMode', () => {
    useVideoSettingsStore.getState().setVideoAdvancedMode(true);
    expect(useVideoSettingsStore.getState().videoAdvancedMode).toBe(true);
  });

  it('sets camera preset', () => {
    useVideoSettingsStore.getState().setCameraPreset('720p30');
    expect(useVideoSettingsStore.getState().cameraPreset).toBe('720p30');
  });

  it('has all expected quality presets', () => {
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('system');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('360p30');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('720p30');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('720p60');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('1080p30');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('1080p60');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('1440p30');
    expect(VIDEO_QUALITY_PRESETS).toHaveProperty('1440p60');
  });

  it('quality presets have valid configurations', () => {
    for (const [key, preset] of Object.entries(VIDEO_QUALITY_PRESETS)) {
      if (key === 'system') continue;
      expect(preset.width).toBeGreaterThan(0);
      expect(preset.height).toBeGreaterThan(0);
      expect(preset.frameRate).toBeGreaterThan(0);
      expect(preset.maxBitrate).toBeGreaterThan(0);
    }
  });

  it('sets codec capabilities', () => {
    useVideoSettingsStore.getState().setCodecCapabilities({ vp9: true, h264: true });
    expect(useVideoSettingsStore.getState().codecCapabilities).toEqual({ vp9: true, h264: true });
  });

  it('sets GPU info', () => {
    useVideoSettingsStore.getState().setGpuInfo({ vendor: 'NVIDIA', renderer: 'RTX 4090' });
    expect(useVideoSettingsStore.getState().gpuInfo).toEqual({
      vendor: 'NVIDIA',
      renderer: 'RTX 4090',
    });
  });
});
