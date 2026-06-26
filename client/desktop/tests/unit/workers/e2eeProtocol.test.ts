// @vitest-environment node
import { describe, it, expect } from 'vitest';
import {
  codecFamilyFromMimeType,
  codecFamilyFromRtpParameters,
  selectCryptoScheme,
} from '@/renderer/workers/e2eeProtocol';

describe('codecFamilyFromMimeType', () => {
  it('maps known mimeTypes case-insensitively', () => {
    expect(codecFamilyFromMimeType('video/AV1')).toBe('av1');
    expect(codecFamilyFromMimeType('video/av1')).toBe('av1');
    expect(codecFamilyFromMimeType('video/VP9')).toBe('vp9');
    expect(codecFamilyFromMimeType('video/VP8')).toBe('vp8');
    expect(codecFamilyFromMimeType('audio/opus')).toBe('opus');
    expect(codecFamilyFromMimeType('video/H264')).toBe('h264');
  });

  it('unknown / missing mimeType → undefined (whole-frame default)', () => {
    expect(codecFamilyFromMimeType('video/H265')).toBeUndefined();
    expect(codecFamilyFromMimeType('')).toBeUndefined();
    expect(codecFamilyFromMimeType(undefined)).toBeUndefined();
  });
});

describe('codecFamilyFromRtpParameters', () => {
  it('reads the first media codec mimeType', () => {
    expect(codecFamilyFromRtpParameters({ codecs: [{ mimeType: 'video/AV1' }] })).toBe('av1');
    expect(codecFamilyFromRtpParameters({ codecs: [{ mimeType: 'video/VP9' }] })).toBe('vp9');
    expect(codecFamilyFromRtpParameters({ codecs: [{ mimeType: 'audio/opus' }] })).toBe('opus');
  });
  it('empty / missing codecs → undefined', () => {
    expect(codecFamilyFromRtpParameters({ codecs: [] })).toBeUndefined();
    expect(codecFamilyFromRtpParameters({})).toBeUndefined();
    expect(codecFamilyFromRtpParameters(undefined)).toBeUndefined();
  });
  // FIX 2 (Gitar #2): codec-resolution robustness — skip non-media entries so
  // encrypt and decrypt always resolve the SAME media codec regardless of list order.
  it('skips RTX entry and returns the media codec (video/rtx + video/AV1 → av1)', () => {
    expect(
      codecFamilyFromRtpParameters({
        codecs: [{ mimeType: 'video/rtx' }, { mimeType: 'video/AV1' }],
      })
    ).toBe('av1');
  });
  it('returns opus when only a media codec is listed (audio/opus → opus)', () => {
    expect(codecFamilyFromRtpParameters({ codecs: [{ mimeType: 'audio/opus' }] })).toBe('opus');
  });
  it('returns undefined when only non-media codecs are listed (video/rtx alone → undefined)', () => {
    expect(codecFamilyFromRtpParameters({ codecs: [{ mimeType: 'video/rtx' }] })).toBeUndefined();
  });
  it('empty codecs list → undefined', () => {
    expect(codecFamilyFromRtpParameters({ codecs: [] })).toBeUndefined();
  });
});

describe('selectCryptoScheme', () => {
  it('av1 -> per-obu, others -> whole-frame', () => {
    expect(selectCryptoScheme('av1')).toBe('per-obu');
    expect(selectCryptoScheme('vp9')).toBe('whole-frame');
    expect(selectCryptoScheme('opus')).toBe('whole-frame');
    expect(selectCryptoScheme(undefined)).toBe('whole-frame');
  });
});
