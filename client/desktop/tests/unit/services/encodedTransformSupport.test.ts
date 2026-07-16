import { describe, expect, it } from 'vitest';
import { resolveEncodedTransformSupport } from '@/renderer/services/encodedTransformSupport';

describe('resolveEncodedTransformSupport', () => {
  it('prefers RTCRtpScriptTransform when both transform APIs are present', () => {
    expect(
      resolveEncodedTransformSupport({
        scriptTransform: class {},
        createEncodedStreams: () => {},
      })
    ).toBe('script-transform');
  });

  it('uses createEncodedStreams only when RTCRtpScriptTransform is absent', () => {
    expect(
      resolveEncodedTransformSupport({
        scriptTransform: undefined,
        createEncodedStreams: () => {},
      })
    ).toBe('encoded-streams');
  });

  it('reports unavailable when neither transform API is present', () => {
    expect(
      resolveEncodedTransformSupport({
        scriptTransform: undefined,
        createEncodedStreams: undefined,
      })
    ).toBe('unavailable');
  });

  it('ignores a non-callable partial modern global and retains the legacy fallback', () => {
    expect(
      resolveEncodedTransformSupport({
        scriptTransform: null,
        createEncodedStreams: () => {},
      })
    ).toBe('encoded-streams');
  });
});
