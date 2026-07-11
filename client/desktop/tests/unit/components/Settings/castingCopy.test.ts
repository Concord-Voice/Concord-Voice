import { describe, expect, it } from 'vitest';
import { castingCopy } from '@/renderer/components/Settings/castingCopy';

describe('castingCopy (#1921)', () => {
  it('AV1 + svc on → SVC engaged line', () =>
    expect(castingCopy('video/AV1', true, true).svc).toMatch(/AV1.*SVC|layered/i));

  it('H264 + svc on → SVC does not apply (simulcast-only codec)', () => {
    // Tightened (#1924): H.264/VP8 are Simulcast-only, so Support SVC has no effect —
    // the copy must say so, not merely "applies if you switch".
    const svc = castingCopy('video/H264', true, true).svc;
    expect(svc).toMatch(/does not apply|Simulcast-only/i);
    expect(svc).toMatch(/switch/i);
  });

  it('H264 + simulcast on → Simulcast engaged line', () =>
    expect(castingCopy('video/H264', true, true).simulcast).toMatch(/Simulcast layers/i));

  it('AV1 + simulcast on → Simulcast does NOT apply (AV1/VP9 are SVC-only)', () => {
    // Tightened (#1924): AV1/VP9 can never Simulcast; enabling Support Simulcast has NO
    // effect for them, and their layering depends on Support SVC. The copy must be explicit.
    const simulcast = castingCopy('video/AV1', true, true).simulcast;
    expect(simulcast).toMatch(/no effect|SVC-only|never Simulcast/i);
    expect(simulcast).toMatch(/Support SVC/i);
  });

  it('AV1 + svc off → single-stream copy that notes AV1/VP9 cannot Simulcast', () => {
    // Tightened (#1924): with SVC off there is NO Simulcast fallback for AV1/VP9.
    const svc = castingCopy('video/AV1', false, true).svc;
    expect(svc).toMatch(/single stream/i);
    expect(svc).toMatch(/cannot Simulcast/i);
  });

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
