import { describe, expect, it } from 'vitest';
import { resolveEncodedTransformSupport } from '@/renderer/services/e2ee/encodedTransformSupport';

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

describe('resolveEncodedTransformSupport — forceLegacy override', () => {
  const fn = () => {};

  it('prefers the legacy pipeline when forced and available', () => {
    expect(
      resolveEncodedTransformSupport(
        { scriptTransform: fn, createEncodedStreams: fn },
        { forceLegacy: true }
      )
    ).toBe('encoded-streams');
  });

  it('ignores the override when the legacy API is absent — never a path to unavailable', () => {
    expect(
      resolveEncodedTransformSupport(
        { scriptTransform: fn, createEncodedStreams: undefined },
        { forceLegacy: true }
      )
    ).toBe('script-transform');
  });

  it('defaults to the modern preference when the override is not passed', () => {
    expect(resolveEncodedTransformSupport({ scriptTransform: fn, createEncodedStreams: fn })).toBe(
      'script-transform'
    );
  });
});
