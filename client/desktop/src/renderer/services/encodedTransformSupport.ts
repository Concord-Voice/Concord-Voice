export type EncodedTransformPath = 'script-transform' | 'encoded-streams' | 'unavailable';

interface EncodedTransformApis {
  scriptTransform: unknown;
  createEncodedStreams: unknown;
}

export interface EncodedTransformOverrides {
  /**
   * Prefer the legacy createEncodedStreams pipeline even when
   * RTCRtpScriptTransform exists. Engaged automatically when the bypass probe
   * confirms the engine is not routing frames through an attached receive
   * transform (Chromium 149/150 V2 regression — 2026-08-21 incident), or
   * manually via localStorage 'concord.forceLegacyE2EE' = '1'. Only honored
   * when the legacy API actually exists — never a path to 'unavailable'.
   */
  forceLegacy?: boolean;
}

export function resolveEncodedTransformSupport(
  { scriptTransform, createEncodedStreams }: EncodedTransformApis,
  { forceLegacy = false }: EncodedTransformOverrides = {}
): EncodedTransformPath {
  if (forceLegacy && typeof createEncodedStreams === 'function') return 'encoded-streams';
  if (typeof scriptTransform === 'function') return 'script-transform';
  if (typeof createEncodedStreams === 'function') return 'encoded-streams';
  return 'unavailable';
}
