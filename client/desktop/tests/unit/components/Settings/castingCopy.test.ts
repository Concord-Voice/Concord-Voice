import { describe, expect, it } from 'vitest';
import { castingCopy } from '@/renderer/components/Settings/castingCopy';

describe('castingCopy (#1921)', () => {
  it('AV1 + svc on → SVC engaged line', () =>
    expect(castingCopy('video/AV1', true, true).svc).toMatch(/AV1.*SVC|layered/i));

  it('H264 + svc on → "applies if you switch" (codec-inert)', () =>
    expect(castingCopy('video/H264', true, true).svc).toMatch(/applies|switch/i));

  it('H264 + simulcast on → Simulcast engaged line', () =>
    expect(castingCopy('video/H264', true, true).simulcast).toMatch(/Simulcast layers/i));

  it('AV1 + simulcast on → codec-inert "applies" line', () =>
    expect(castingCopy('video/AV1', true, true).simulcast).toMatch(/applies|H\.264\/VP8/i));

  it('AV1 + svc off → single-stream copy', () =>
    expect(castingCopy('video/AV1', false, true).svc).toMatch(/single stream/i));

  it('both off → single-stream notice present', () =>
    expect(castingCopy('video/AV1', false, false).notice).toMatch(/single stream/i));

  it('not-both-off → no notice', () => {
    expect(castingCopy('video/AV1', true, false).notice).toBeUndefined();
    expect(castingCopy('video/AV1', false, true).notice).toBeUndefined();
  });

  it('Auto (null codec) → generic, non-empty copy', () => {
    const c = castingCopy(null, true, true);
    expect(c.svc.length).toBeGreaterThan(0);
    expect(c.simulcast.length).toBeGreaterThan(0);
  });

  it('strips a :profile suffix when classifying', () =>
    expect(castingCopy('video/H264:640034', true, true).simulcast).toMatch(/Simulcast layers/i));
});
