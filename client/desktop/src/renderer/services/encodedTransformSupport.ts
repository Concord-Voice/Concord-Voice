export type EncodedTransformPath = 'script-transform' | 'encoded-streams' | 'unavailable';

interface EncodedTransformApis {
  scriptTransform: unknown;
  createEncodedStreams: unknown;
}

export function resolveEncodedTransformSupport({
  scriptTransform,
  createEncodedStreams,
}: EncodedTransformApis): EncodedTransformPath {
  if (typeof scriptTransform === 'function') return 'script-transform';
  if (typeof createEncodedStreams === 'function') return 'encoded-streams';
  return 'unavailable';
}
