import { describe, it, expect } from 'vitest';
import {
  PROFILE_LEVEL_ID_LABELS,
  humanizeProfileLabel,
  getCodecMetadata,
} from '@/renderer/components/Settings/codecMetadata';

describe('humanizeProfileLabel', () => {
  it('maps known H.264 profile-level-id hex to human labels', () => {
    expect(humanizeProfileLabel('42001f', null)).toBe('Constrained Baseline 3.1');
    expect(humanizeProfileLabel('42e01f', 'Baseline')).toBe('Baseline 3.1');
    expect(humanizeProfileLabel('64001f', 'High')).toBe('High 3.1');
    expect(humanizeProfileLabel('640C1F', null)).toBe('Constrained High 3.1');
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

  it('exposes the canonical map for 8 known entries', () => {
    expect(Object.keys(PROFILE_LEVEL_ID_LABELS)).toHaveLength(8);
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
  });

  it('accepts composite key form with profile suffix', () => {
    const meta = getCodecMetadata('video/H264:42001f');
    expect(meta?.quality).toBe('Mid');
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
