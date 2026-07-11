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
    useVideoSettingsStore
      .getState()
      .setGpuInfo({ vendor: 'NVIDIA', device: 'RTX 4090', encodeProfiles: ['video/H264'] });
    expect(useVideoSettingsStore.getState().gpuInfo).toEqual({
      vendor: 'NVIDIA',
      device: 'RTX 4090',
      encodeProfiles: ['video/H264'],
    });
  });

  it('learns and merges the runtime WebRTC hardware signal per codec (case-insensitive)', () => {
    const s = useVideoSettingsStore.getState();
    s.setWebrtcHwForMime('video/AV1', false);
    s.setWebrtcHwForMime('video/H264', true);
    expect(useVideoSettingsStore.getState().webrtcHwByMime).toEqual({
      'video/av1': false,
      'video/h264': true,
    });
    // Last-write-wins overwrite (a codec's B-verdict can change as the encoder settles).
    useVideoSettingsStore.getState().setWebrtcHwForMime('video/av1', true);
    expect(useVideoSettingsStore.getState().webrtcHwByMime['video/av1']).toBe(true);
  });
});

describe('videoSettingsStore casting toggles (#1921)', () => {
  beforeEach(() => {
    resetAllStores();
    localStorage.clear();
  });

  it('defaults supportSvc and supportSimulcast ON', () => {
    const s = useVideoSettingsStore.getState();
    expect(s.supportSvc).toBe(true);
    expect(s.supportSimulcast).toBe(true);
  });

  it('setters flip the values', () => {
    useVideoSettingsStore.getState().setSupportSvc(false);
    useVideoSettingsStore.getState().setSupportSimulcast(false);
    expect(useVideoSettingsStore.getState().supportSvc).toBe(false);
    expect(useVideoSettingsStore.getState().supportSimulcast).toBe(false);
  });
});

describe('autoTuneInScreenShares (#2088)', () => {
  it('defaults to false (off-by-default receive policy)', () => {
    expect(useVideoSettingsStore.getState().autoTuneInScreenShares).toBe(false);
  });

  it('setAutoTuneInScreenShares updates the value', () => {
    useVideoSettingsStore.getState().setAutoTuneInScreenShares(true);
    expect(useVideoSettingsStore.getState().autoTuneInScreenShares).toBe(true);
  });

  it('is included in the persisted partialize payload', () => {
    useVideoSettingsStore.getState().setAutoTuneInScreenShares(true);
    const raw = localStorage.getItem('concord:video-settings');
    expect(raw).not.toBeNull();
    expect(JSON.parse(raw as string).state.autoTuneInScreenShares).toBe(true);
  });
});
