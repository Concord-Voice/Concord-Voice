// @vitest-environment node
import { describe, it, expect } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  MAX_KEYS,
  MODES,
  SIZES,
  TOKENS,
  fileNameFor,
  loadFrameKeys,
  manifestFor,
  pageHtmlFor,
  parseKey,
  readSourcePaths,
  svgForFrame,
} from './generate-tray-icons.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SVG_PATH = path.join(__dirname, '..', 'assets', 'tray', 'tray-source.svg');
const BRAND_PATH = path.join(
  __dirname,
  '..',
  '..',
  '..',
  'branding',
  'Concord-Voice',
  'logos',
  'symbol-transparent-vector.svg'
);
const source = fs.readFileSync(SVG_PATH, 'utf8');
const keys: string[] = loadFrameKeys();

/** Strips <defs> so fill checks look only at drawn pieces. */
const withoutDefs = (svg: string) => svg.replace(/<defs>.*?<\/defs>/s, '');

describe('tray-source.svg', () => {
  it('has three groups crescent, sparkle-lg, sparkle-sm', () => {
    const ids = [...source.matchAll(/<g id="([^"]+)"/g)].map((m) => m[1]);
    expect(ids).toEqual(['crescent', 'sparkle-lg', 'sparkle-sm']);
  });

  it('small sparkle path matches the brand mark', () => {
    const brand = fs.readFileSync(BRAND_PATH, 'utf8');
    const m = /zm74\.065-6\.12(h\.07[^M]*?z)M54\.388/.exec(brand);
    expect(m).not.toBeNull();
    const expected = `M74.895 43.88${m?.[1]}`;
    expect(readSourcePaths(source).s).toBe(expected);
  });

  it('crescent and large sparkle match the brand mark subpaths', () => {
    const brand = fs.readFileSync(BRAND_PATH, 'utf8');
    const paths = readSourcePaths(source);
    expect(brand).toContain(paths.c.replace(/z$/, ''));
    expect(brand).toContain(paths.l);
  });

  it('gradient is userSpaceOnUse, vertical, with the brand stops', () => {
    expect(source).toContain('gradientUnits="userSpaceOnUse"');
    expect(source).toContain('x1="0" y1="100" x2="0" y2="0"');
    expect(source).toContain('stop-color="#fa709a"');
    expect(source).toContain('stop-color="#ffe13f"');
  });
});

describe('frames.json', () => {
  it('keys all parse to known tokens', () => {
    expect(keys.length).toBeGreaterThan(0);
    for (const key of keys) {
      expect(() => parseKey(key)).not.toThrow();
    }
  });

  it('has at most 60 keys and no duplicates', () => {
    expect(keys.length).toBeLessThanOrEqual(MAX_KEYS);
    expect(new Set(keys).size).toBe(keys.length);
  });

  it('parseKey rejects malformed and unknown keys', () => {
    expect(() => parseKey('full.full')).toThrow();
    expect(() => parseKey('full.full.nope')).toThrow();
  });
});

describe('svgForFrame', () => {
  it('colour mode carries all three pieces and the gradient', () => {
    const svg = svgForFrame('full.full.star', 'colour');
    const paths = readSourcePaths(source);
    expect(svg).toContain(paths.c);
    expect(svg).toContain(paths.l);
    expect(svg).toContain(paths.s);
    expect(svg).toContain('<linearGradient id="brand"');
    expect(svg).toContain('fill="url(#brand)"');
  });

  it('white mode has no gradient and only #ffffff fills on drawn pieces', () => {
    for (const key of keys) {
      const svg = svgForFrame(key, 'white');
      expect(svg).not.toContain('url(#brand');
      expect(svg).not.toContain('linearGradient');
      const drawn = withoutDefs(svg);
      const fills = [...drawn.matchAll(/ fill="([^"]+)"/g)].map((m) => m[1]);
      for (const fill of fills) expect(['#ffffff', 'none']).toContain(fill);
      const strokes = [...drawn.matchAll(/ stroke="([^"]+)"/g)].map((m) => m[1]);
      for (const stroke of strokes) expect(stroke).toBe('#ffffff');
    }
  });

  it('template and black modes use only #000000', () => {
    for (const mode of ['template', 'black'] as const) {
      for (const key of keys) {
        const colours = withoutDefs(svgForFrame(key, mode)).match(/#[0-9a-fA-F]{6}/g) ?? [];
        for (const c of colours) expect(c).toBe('#000000');
      }
    }
  });

  it('never omits a piece: three pieces per key, each opacity >= 0.35', () => {
    for (const key of keys) {
      for (const mode of MODES) {
        const svg = svgForFrame(key, mode);
        const pieces = [
          ...svg.matchAll(/<(?:path|circle) data-piece="([csl])"[^>]*?opacity="([\d.]+)"/g),
        ];
        expect(pieces.map((p) => p[1]).sort((a, b) => a.localeCompare(b))).toEqual(['c', 'l', 's']);
        for (const p of pieces) expect(Number(p[2])).toBeGreaterThanOrEqual(0.35);
        // The small sparkle is ~3 px at 16 px; below 0.6 it vanishes on a dark bar.
        const small = pieces.find((p) => p[1] === 's');
        expect(Number(small?.[2])).toBeGreaterThanOrEqual(0.6);
      }
    }
  });

  it('status colours appear only in colour mode', () => {
    expect(svgForFrame('green.slash.ring', 'colour')).toContain('#2f9e6a');
    expect(svgForFrame('green.slash.ring', 'colour')).toContain('#f04747');
    expect(svgForFrame('full.full.discmention', 'colour')).toContain('#e0507e');
    expect(svgForFrame('amber.full.dim35', 'colour')).toContain('#c47a00');
    expect(svgForFrame('full.full.discupdate', 'colour')).toContain('#3a86d4');
    expect(svgForFrame('green.slash.ring', 'black')).not.toContain('#f04747');
  });

  it('slash keys draw a knockout mask plus the slash stroke', () => {
    const short = svgForFrame('green.slash.star', 'colour');
    const long = svgForFrame('green35.slashlong.star', 'colour');
    expect(short).toContain('mask="url(#knock)"');
    expect(short).toContain('x1="42" y1="56" x2="70" y2="12"');
    expect(long).toContain('x1="22" y1="83" x2="86" y2="10"');
    expect(svgForFrame('full.full.star', 'colour')).not.toContain('mask=');
  });

  it('burst tokens scale the large sparkle from 1.0 up to 1.35 and back', () => {
    const scales = [1, 2, 3, 4, 5, 6, 7].map(
      (n) => (TOKENS.l as Record<string, { scale?: number }>)[`burst${n}`].scale ?? 0
    );
    expect(scales[0]).toBe(1);
    expect(scales[6]).toBe(1);
    expect(Math.max(...scales)).toBe(1.35);
  });

  it('breath tokens set the large sparkle opacity', () => {
    expect(TOKENS.l.breath70.opacity).toBe(0.7);
    expect(TOKENS.l.breath40.opacity).toBe(0.4);
  });

  it('rejects an unknown mode', () => {
    expect(() => svgForFrame('full.full.star', 'neon')).toThrow();
  });
});

describe('manifest and file layout', () => {
  it('manifestFor lists keys, modes and per-platform sizes', () => {
    const m = manifestFor(['full.full.star']);
    expect(m).toEqual({
      keys: ['full.full.star'],
      modes: ['template', 'white', 'black', 'colour'],
      sizes: { darwin: [16, 32], win32: [16, 32], linux: [22] },
    });
    expect(SIZES.linux).toEqual([22]);
  });

  it('fileNameFor maps sizes to 1x, @2x and -22 names', () => {
    expect(fileNameFor('a.b.c', 16)).toBe('a.b.c.png');
    expect(fileNameFor('a.b.c', 32)).toBe('a.b.c@2x.png');
    expect(fileNameFor('a.b.c', 22)).toBe('a.b.c-22.png');
    expect(() => fileNameFor('a.b.c', 24)).toThrow();
  });

  it('committed sprites exist for every key, mode and size when generated', () => {
    const manifestPath = path.join(__dirname, '..', 'assets', 'tray', 'manifest.json');
    if (!fs.existsSync(manifestPath)) return;
    const manifest = JSON.parse(fs.readFileSync(manifestPath, 'utf8'));
    expect(manifest.keys).toEqual(keys);
    for (const mode of manifest.modes) {
      for (const key of manifest.keys) {
        for (const size of [16, 32, 22]) {
          const file = path.join(__dirname, '..', 'assets', 'tray', mode, fileNameFor(key, size));
          const png = fs.readFileSync(file);
          expect(png.readUInt32BE(16)).toBe(size);
          expect(png.readUInt32BE(20)).toBe(size);
        }
      }
    }
  });
});

describe('pageHtmlFor', () => {
  it('sizes the svg to the exact pixel target', () => {
    const html = pageHtmlFor('<svg>x</svg>', 22);
    expect(html).toContain('width:22px');
    expect(html).toContain('height:22px');
    expect(html).toContain('<svg>x</svg>');
  });

  it('zeroes margins so the raster has no offset', () => {
    expect(pageHtmlFor('<svg/>', 16)).toContain('*{margin:0;padding:0}');
  });
});
