// Byte-level detection of animated WebP / APNG (#2369, PR #3291).
// Fixtures are built from the real container layouts, not from mocks — the
// whole point of this module is that it reads actual bytes correctly.
import { detectAnimatedImage, ANIMATION_SNIFF_BYTES } from '@/renderer/utils/ui/animatedImage';

function bytes(...parts: (number[] | string)[]): Uint8Array {
  const flat: number[] = [];
  for (const p of parts) {
    if (typeof p === 'string') for (const ch of p) flat.push(ch.charCodeAt(0));
    else flat.push(...p);
  }
  return new Uint8Array(flat);
}

const u32be = (n: number): number[] => [
  (n >>> 24) & 255,
  (n >>> 16) & 255,
  (n >>> 8) & 255,
  n & 255,
];

/** RIFF....WEBP VP8X <len> <flags> — flags bit 1 is ANIMATION. */
function webpVP8X(flags: number): Uint8Array {
  return bytes('RIFF', u32be(30), 'WEBP', 'VP8X', u32be(10), [flags], new Array(9).fill(0));
}

const PNG_MAGIC = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
/** magic + IHDR(13) + the named chunk. */
function png(chunkType: string, extra: number[] = []): Uint8Array {
  return bytes(
    PNG_MAGIC,
    u32be(13),
    'IHDR',
    new Array(13).fill(0),
    u32be(0),
    extra,
    u32be(8),
    chunkType,
    new Array(8).fill(0),
    u32be(0)
  );
}

describe('detectAnimatedImage', () => {
  describe('WebP — the format MIME cannot decide', () => {
    it('VP8X with the ANIMATION bit set is animated', () => {
      expect(detectAnimatedImage(webpVP8X(0x02), 'image/webp')).toBe(true);
    });

    it('VP8X WITHOUT the animation bit is still — a static WebP must not be gated', () => {
      expect(detectAnimatedImage(webpVP8X(0x00), 'image/webp')).toBe(false);
    });

    it('a neighbouring flag bit is not mistaken for ANIMATION', () => {
      // 0x10 is ALPHA. Reading the byte as truthy rather than masking bit 1
      // would call every alpha WebP animated.
      expect(detectAnimatedImage(webpVP8X(0x10), 'image/webp')).toBe(false);
    });

    it('the simple VP8 / VP8L forms are still by definition', () => {
      expect(
        detectAnimatedImage(
          bytes('RIFF', u32be(20), 'WEBP', 'VP8 ', new Array(9).fill(0)),
          'image/webp'
        )
      ).toBe(false);
      expect(
        detectAnimatedImage(
          bytes('RIFF', u32be(20), 'WEBP', 'VP8L', new Array(9).fill(0)),
          'image/webp'
        )
      ).toBe(false);
    });

    it('bytes that are not RIFF/WEBP yield no opinion rather than a guess', () => {
      expect(
        detectAnimatedImage(
          bytes('NOPE', u32be(0), 'XXXX', 'VP8X', new Array(9).fill(0)),
          'image/webp'
        )
      ).toBeNull();
    });

    it('a truncated header yields no opinion', () => {
      expect(detectAnimatedImage(bytes('RIFF', u32be(0), 'WEBP'), 'image/webp')).toBeNull();
    });
  });

  describe('PNG / APNG — the mislabelled case', () => {
    it('acTL before IDAT is animated, under the common image/png label', () => {
      expect(detectAnimatedImage(png('acTL'), 'image/png')).toBe(true);
    });

    it('IDAT with no acTL is still', () => {
      expect(detectAnimatedImage(png('IDAT'), 'image/png')).toBe(false);
    });

    it('acTL is found past an intervening ancillary chunk', () => {
      const iccp = [
        ...u32be(20),
        ...'iCCP'.split('').map((c) => c.charCodeAt(0)),
        ...new Array(20).fill(0),
        ...u32be(0),
      ];
      expect(detectAnimatedImage(png('acTL', iccp), 'image/png')).toBe(true);
    });

    it('walks the chunk TABLE, so the literal acTL inside chunk data is not a match', () => {
      // `acTL` as ordinary bytes inside an IDAT payload. A naive indexOf scan
      // reports animated here; walking lengths does not.
      const idatWithLiteral = [
        ...u32be(8),
        ...'IDAT'.split('').map((c) => c.charCodeAt(0)),
        ...'acTL'.split('').map((c) => c.charCodeAt(0)),
        0,
        0,
        0,
        0,
        ...u32be(0),
      ];
      expect(
        detectAnimatedImage(
          bytes(PNG_MAGIC, u32be(13), 'IHDR', new Array(13).fill(0), u32be(0), idatWithLiteral),
          'image/png'
        )
      ).toBe(false);
    });

    it('running out of prefix before acTL or IDAT yields NULL, not false', () => {
      // A chunk whose declared length runs past the buffer. Answering `false`
      // here would silently mis-gate exactly the file this module exists for.
      const huge = bytes(
        PNG_MAGIC,
        u32be(13),
        'IHDR',
        new Array(13).fill(0),
        u32be(0),
        u32be(9_000_000),
        'iCCP'
      );
      expect(detectAnimatedImage(huge, 'image/png')).toBeNull();
    });

    it('bytes without the PNG magic yield no opinion', () => {
      expect(detectAnimatedImage(bytes('notapng!'), 'image/png')).toBeNull();
    });
  });

  it('formats this module does not judge yield null so the persisted file_type stands', () => {
    expect(detectAnimatedImage(bytes('GIF89a'), 'image/gif')).toBeNull();
    expect(detectAnimatedImage(bytes([0xff, 0xd8, 0xff]), 'image/jpeg')).toBeNull();
  });

  it('the sniff window is large enough for a PNG carrying an embedded colour profile', () => {
    expect(ANIMATION_SNIFF_BYTES).toBeGreaterThanOrEqual(64 * 1024);
  });
});
