import { describe, it, expect } from 'vitest';
import {
  PROFILE_LEVEL_ID_LABELS,
  canonicalRouterCodecKey,
  codecProfileMenuLabel,
  humanizeProfileLabel,
  isRouterSupportedCodecProfile,
  getCodecMetadata,
} from '@/renderer/components/Settings/codecMetadata';

describe('humanizeProfileLabel', () => {
  it('maps known H.264 profile-level-id hex to human labels', () => {
    expect(humanizeProfileLabel('42e01f', null)).toBe('Constrained Baseline 3.1');
    expect(humanizeProfileLabel('42001f', null)).toBe('Baseline 3.1');
    expect(humanizeProfileLabel('4d0032', null)).toBe('Main 5.0');
    expect(humanizeProfileLabel('640034', null)).toBe('High 5.2');
    expect(humanizeProfileLabel('4d401f', null)).toBe('Main 3.1');
    expect(humanizeProfileLabel('4d400a', null)).toBe('Main 1.0');
    expect(humanizeProfileLabel('F4000A', null)).toBe('Predictive High 4:4:4 1.0');
  });

  it('falls back to provided label when profile id is unknown', () => {
    expect(humanizeProfileLabel('aabbcc', 'Main')).toBe('Main');
  });

  it('suppresses raw hex-looking fallback labels', () => {
    expect(humanizeProfileLabel('aabbcc', 'aabbcc')).toBeNull();
  });

  it('returns fallback when profile id is null', () => {
    expect(humanizeProfileLabel(null, 'HDR')).toBe('HDR');
    expect(humanizeProfileLabel(null, null)).toBeNull();
  });

  it('exposes the canonical map for 10 known entries', () => {
    expect(Object.keys(PROFILE_LEVEL_ID_LABELS)).toHaveLength(10);
  });
});

describe('codec profile policy', () => {
  it('canonicalizes compatible client profile levels to router codec keys', () => {
    expect(canonicalRouterCodecKey('video/H264', '64001f')).toBe('video/h264:640034');
    expect(canonicalRouterCodecKey('video/H264', '4d401f')).toBe('video/h264:4d0032');
    expect(canonicalRouterCodecKey('video/H264', '42e034')).toBe('video/h264:42e01f');

    for (const profileId of ['42001f', '640c1f', 'f4000a']) {
      expect(canonicalRouterCodecKey('video/H264', profileId)).toBeNull();
    }
  });

  it('accepts client H.264 levels in the same profile classes advertised by the router', () => {
    for (const profileId of ['42e01f', '4d0032', '640034']) {
      expect(isRouterSupportedCodecProfile('video/H264', profileId)).toBe(true);
    }
    expect(isRouterSupportedCodecProfile('video/H264', '4d401f')).toBe(true);
    expect(isRouterSupportedCodecProfile('video/H264', '64001f')).toBe(true);

    for (const profileId of ['42001f', '640c1f', 'f4000a', null]) {
      expect(isRouterSupportedCodecProfile('video/H264', profileId)).toBe(false);
    }
    expect(isRouterSupportedCodecProfile('video/AV1', null)).toBe(true);
    expect(isRouterSupportedCodecProfile('video/VP9', '0')).toBe(true);
    expect(isRouterSupportedCodecProfile('video/VP9', '2')).toBe(true);
    expect(isRouterSupportedCodecProfile('video/VP9', '1')).toBe(false);
    expect(isRouterSupportedCodecProfile('video/VP9', '3')).toBe(false);
    expect(isRouterSupportedCodecProfile('video/VP8', null)).toBe(true);
    expect(isRouterSupportedCodecProfile('video/HEVC', null)).toBe(false);
  });

  it('uses the canonical router label for compatible client H.264 levels', () => {
    expect(codecProfileMenuLabel('video/H264', '64001f')).toBe(
      'H.264 (High 5.2 — Best H.264 quality)'
    );
    expect(codecProfileMenuLabel('video/H264', '4d401f')).toBe('H.264 (Main 5.0 — Balanced)');
  });

  it('keeps AV1 application targets distinct while mapping both to RTP AV1', () => {
    expect(canonicalRouterCodecKey('video/AV1', 'hdr')).toBe('video/av1');
    expect(canonicalRouterCodecKey('video/AV1', 'sdr')).toBe('video/av1');
    expect(codecProfileMenuLabel('video/AV1', 'hdr')).toBe('AV1 (10-bit HDR target)');
    expect(codecProfileMenuLabel('video/AV1', 'sdr')).toBe('AV1 (8-bit SDR target)');
  });
});

describe('getCodecMetadata', () => {
  it('returns AV1 metadata by base name', () => {
    const meta = getCodecMetadata('av1');
    expect(meta).not.toBeNull();
    expect(meta?.quality).toBe('Ultra');
    expect(meta?.hdrCapable).toBe(true);
  });

  it('accepts mimeType form', () => {
    const meta = getCodecMetadata('video/VP9');
    expect(meta?.quality).toBe('High');
    expect(meta?.hdrCapable).toBe(false);
  });

  it('marks only VP9 profile 2 as HDR-capable', () => {
    expect(getCodecMetadata('video/VP9:0')?.hdrCapable).toBe(false);
    expect(getCodecMetadata('video/VP9:2')?.hdrCapable).toBe(true);
  });

  it('accepts composite key form with profile suffix', () => {
    const meta = getCodecMetadata('video/H264:42001f');
    expect(meta?.quality).toBe('Mid');
  });

  it('keeps every surfaced H.264 profile SDR-only', () => {
    for (const profileId of ['42e01f', '42001f', '4d0032', '640034', 'f4000a']) {
      expect(getCodecMetadata(`video/H264:${profileId}`)?.hdrCapable).toBe(false);
    }
  });

  it('returns null for unknown codec', () => {
    expect(getCodecMetadata('theora')).toBeNull();
  });

  it('returns metadata for each canonical base codec', () => {
    for (const base of ['av1', 'vp9', 'vp8', 'h264', 'h265', 'hevc']) {
      expect(getCodecMetadata(base)).not.toBeNull();
    }
  });
});
