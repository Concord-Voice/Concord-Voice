import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import {
  codecKey,
  codecKeyMime,
  getCodecInfo,
  enumerateWebcamCapabilities,
  detectCodecCapabilities,
  clearCapabilitiesCache,
  prewarmWebRTC,
  type CodecCapability,
} from '@/renderer/services/mediaCapabilities';

// parseProfile is not exported, so we test it indirectly via detectCodecCapabilities.
// However codecKey, codecKeyMime, getCodecInfo are directly testable.

describe('mediaCapabilities', () => {
  beforeEach(() => {
    clearCapabilitiesCache();
    vi.clearAllMocks();
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  describe('codecKey', () => {
    it('returns mimeType:profileId when profileId exists', () => {
      const cap: CodecCapability = {
        mimeType: 'video/H264',
        powerEfficient: true,
        supported: true,
        profileId: '640034',
        profileLabel: 'High',
        isHdr: false,
      };
      expect(codecKey(cap)).toBe('video/H264:640034');
    });

    it('returns just mimeType when no profileId', () => {
      const cap: CodecCapability = {
        mimeType: 'video/VP8',
        powerEfficient: false,
        supported: true,
        profileId: null,
        profileLabel: null,
        isHdr: false,
      };
      expect(codecKey(cap)).toBe('video/VP8');
    });
  });

  describe('codecKeyMime', () => {
    it('extracts mimeType from key with profile', () => {
      expect(codecKeyMime('video/H264:640034')).toBe('video/H264');
    });

    it('returns key as-is when no profile', () => {
      expect(codecKeyMime('video/VP8')).toBe('video/VP8');
    });
  });

  describe('getCodecInfo', () => {
    it('returns AV1 info', () => {
      const info = getCodecInfo('video/av1');
      expect(info.name).toBe('AV1');
      expect(info.quality).toBe('Excellent');
      expect(info.hdr).toBe(true);
    });

    it('returns VP9 info', () => {
      const info = getCodecInfo('video/vp9');
      expect(info.name).toBe('VP9');
      expect(info.quality).toBe('Very Good');
      expect(info.hdr).toBe(false);
    });

    it('returns VP9 HDR info for profile 2', () => {
      const info = getCodecInfo('video/vp9:2');
      expect(info.name).toBe('VP9 (HDR)');
      expect(info.hdr).toBe(true);
    });

    it('returns H.264 High info', () => {
      const info = getCodecInfo('video/h264:640034');
      expect(info.name).toBe('H.264 (High)');
      expect(info.quality).toBe('Very Good');
    });

    it('returns H.264 Main info', () => {
      const info = getCodecInfo('video/h264:4d0028');
      expect(info.name).toBe('H.264 (Main)');
    });

    it('returns H.264 Baseline info for unknown profile', () => {
      const info = getCodecInfo('video/h264');
      expect(info.name).toBe('H.264 (Baseline)');
    });

    it('returns VP8 info', () => {
      const info = getCodecInfo('video/vp8');
      expect(info.name).toBe('VP8');
      expect(info.quality).toBe('Basic');
    });

    it('returns fallback info for unknown codec', () => {
      const info = getCodecInfo('video/unknown');
      expect(info.name).toBe('unknown');
      expect(info.quality).toBe('Unknown');
    });
  });

  describe('enumerateWebcamCapabilities', () => {
    it('probes resolutions and returns supported ones', async () => {
      const mockStream = {
        getTracks: () => [{ stop: vi.fn() }],
      };

      // Only allow 720p30 and 360p30
      const mockGetUserMedia = vi.fn().mockImplementation(
        (constraints: {
          video: {
            width: { exact: number };
            height: { exact: number };
            frameRate: { exact: number };
          };
        }) => {
          const { width, height, frameRate } = constraints.video;
          if (
            (width.exact === 1280 && height.exact === 720 && frameRate.exact === 30) ||
            (width.exact === 640 && height.exact === 360 && frameRate.exact === 30)
          ) {
            return Promise.resolve(mockStream);
          }
          return Promise.reject(new Error('Not supported'));
        }
      );

      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia: mockGetUserMedia },
        configurable: true,
      });

      const caps = await enumerateWebcamCapabilities('device-1');
      expect(caps).toHaveLength(2);
      expect(caps[0]).toEqual({
        width: 1280,
        height: 720,
        frameRate: 30,
        label: '720p30',
      });
      expect(caps[1]).toEqual({
        width: 640,
        height: 360,
        frameRate: 30,
        label: '360p30',
      });
    });

    it('returns cached results for same device', async () => {
      const mockGetUserMedia = vi.fn().mockRejectedValue(new Error('Not supported'));
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia: mockGetUserMedia },
        configurable: true,
      });

      const caps1 = await enumerateWebcamCapabilities('device-cached');
      const caps2 = await enumerateWebcamCapabilities('device-cached');
      expect(caps1).toBe(caps2); // Same reference = cached
    });

    it('returns empty array when no resolutions supported', async () => {
      const mockGetUserMedia = vi.fn().mockRejectedValue(new Error('Not supported'));
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia: mockGetUserMedia },
        configurable: true,
      });

      const caps = await enumerateWebcamCapabilities('device-none');
      expect(caps).toEqual([]);
    });
  });

  describe('detectCodecCapabilities', () => {
    it('returns empty array when getCapabilities returns null', async () => {
      (globalThis as any).RTCRtpSender = {
        getCapabilities: vi.fn(() => null),
      };

      const caps = await detectCodecCapabilities();
      expect(caps).toEqual([]);
    });

    it('detects codecs from RTCRtpSender capabilities', async () => {
      (globalThis as any).RTCRtpSender = {
        getCapabilities: vi.fn(() => ({
          codecs: [
            { mimeType: 'video/VP8' },
            { mimeType: 'video/H264', sdpFmtpLine: 'profile-level-id=640034' },
            { mimeType: 'video/H264', sdpFmtpLine: 'profile-level-id=42e034' },
            { mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=0' },
            { mimeType: 'video/rtx' }, // Should be filtered out
            { mimeType: 'video/red' }, // Should be filtered out
          ],
        })),
      };

      // Mock mediaCapabilities.encodingInfo
      Object.defineProperty(navigator, 'mediaCapabilities', {
        value: {
          encodingInfo: vi.fn().mockResolvedValue({
            supported: true,
            smooth: true,
            powerEfficient: false,
          }),
        },
        configurable: true,
      });

      clearCapabilitiesCache();
      const caps = await detectCodecCapabilities();

      // rtx and red should be filtered out
      expect(caps.length).toBe(4);

      // VP8 should be present
      expect(caps.find((c) => c.mimeType === 'video/VP8')).toBeDefined();

      // H264 High should be present
      const h264High = caps.find((c) => c.mimeType === 'video/H264' && c.profileId === '640034');
      expect(h264High).toBeDefined();
      expect(h264High!.profileLabel).toBe('High');

      // H264 Baseline
      const h264Baseline = caps.find(
        (c) => c.mimeType === 'video/H264' && c.profileId === '42e034'
      );
      expect(h264Baseline).toBeDefined();
      expect(h264Baseline!.profileLabel).toBe('Baseline');

      // VP9 Profile 0
      const vp9 = caps.find((c) => c.mimeType === 'video/VP9');
      expect(vp9).toBeDefined();
      expect(vp9!.profileId).toBe('0');
    });

    it('returns cached results on subsequent calls', async () => {
      (globalThis as any).RTCRtpSender = {
        getCapabilities: vi.fn(() => ({
          codecs: [{ mimeType: 'video/VP8' }],
        })),
      };
      Object.defineProperty(navigator, 'mediaCapabilities', {
        value: {
          encodingInfo: vi.fn().mockResolvedValue({ powerEfficient: false }),
        },
        configurable: true,
      });

      clearCapabilitiesCache();
      const caps1 = await detectCodecCapabilities();
      const caps2 = await detectCodecCapabilities();
      expect(caps1).toBe(caps2); // Same reference = cached
    });

    it('detects hardware acceleration (powerEfficient)', async () => {
      (globalThis as any).RTCRtpSender = {
        getCapabilities: vi.fn(() => ({
          codecs: [{ mimeType: 'video/VP8' }],
        })),
      };
      Object.defineProperty(navigator, 'mediaCapabilities', {
        value: {
          encodingInfo: vi.fn().mockResolvedValue({
            supported: true,
            smooth: true,
            powerEfficient: true,
          }),
        },
        configurable: true,
      });

      clearCapabilitiesCache();
      const caps = await detectCodecCapabilities();
      expect(caps[0].powerEfficient).toBe(true);
    });

    it('deduplicates codecs by mimeType + profile', async () => {
      (globalThis as any).RTCRtpSender = {
        getCapabilities: vi.fn(() => ({
          codecs: [
            { mimeType: 'video/VP8' },
            { mimeType: 'video/VP8' }, // Duplicate
          ],
        })),
      };
      Object.defineProperty(navigator, 'mediaCapabilities', {
        value: {
          encodingInfo: vi.fn().mockResolvedValue({ powerEfficient: false }),
        },
        configurable: true,
      });

      clearCapabilitiesCache();
      const caps = await detectCodecCapabilities();
      expect(caps.length).toBe(1);
    });

    it('handles VP9 HDR profile (profile-id=2)', async () => {
      (globalThis as any).RTCRtpSender = {
        getCapabilities: vi.fn(() => ({
          codecs: [{ mimeType: 'video/VP9', sdpFmtpLine: 'profile-id=2' }],
        })),
      };
      Object.defineProperty(navigator, 'mediaCapabilities', {
        value: {
          encodingInfo: vi.fn().mockResolvedValue({ powerEfficient: false }),
        },
        configurable: true,
      });

      clearCapabilitiesCache();
      const caps = await detectCodecCapabilities();
      expect(caps[0].isHdr).toBe(true);
      expect(caps[0].profileId).toBe('2');
      expect(caps[0].profileLabel).toBe('HDR');
    });
  });

  describe('clearCapabilitiesCache', () => {
    it('clears both webcam and codec caches', async () => {
      const mockGetUserMedia = vi.fn().mockRejectedValue(new Error('Not supported'));
      Object.defineProperty(navigator, 'mediaDevices', {
        value: { getUserMedia: mockGetUserMedia },
        configurable: true,
      });

      // Prime the webcam cache
      await enumerateWebcamCapabilities('device-clear-test');
      const callCount1 = mockGetUserMedia.mock.calls.length;

      clearCapabilitiesCache();

      // After clearing, should re-probe
      await enumerateWebcamCapabilities('device-clear-test');
      expect(mockGetUserMedia.mock.calls.length).toBeGreaterThan(callCount1);
    });
  });

  describe('prewarmWebRTC', () => {
    it('creates and closes a PeerConnection', () => {
      const mockCloseFn = vi.fn();
      (globalThis as any).RTCPeerConnection = function () {
        this.close = mockCloseFn;
      };

      prewarmWebRTC();
      expect(mockCloseFn).toHaveBeenCalled();
    });

    it('does not throw when RTCPeerConnection is unavailable', () => {
      const original = (globalThis as any).RTCPeerConnection;
      (globalThis as any).RTCPeerConnection = vi.fn(() => {
        throw new Error('WebRTC not available');
      });

      // Should not throw
      expect(() => prewarmWebRTC()).not.toThrow();

      (globalThis as any).RTCPeerConnection = original;
    });
  });
});
