// @vitest-environment node
import { describe, it, expect } from 'vitest';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { TARGETS, svgVariantFor, pageHtmlFor } from './generate-tray-icons.mjs';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SVG_PATH = path.join(__dirname, '..', 'assets', 'tray', 'tray-source.svg');

describe('TARGETS', () => {
  it('declares the five spec §5 outputs with exact sizes', () => {
    expect(TARGETS).toEqual([
      { file: 'iconTemplate.png', size: 16, mode: 'template' },
      { file: 'iconTemplate@2x.png', size: 32, mode: 'template' },
      { file: 'icon.png', size: 16, mode: 'color' },
      { file: 'icon@2x.png', size: 32, mode: 'color' },
      { file: 'icon-22.png', size: 22, mode: 'color' },
    ]);
  });

  it('macOS template pair uses the load-bearing Template basename convention', () => {
    const templates = TARGETS.filter((t) => t.mode === 'template').map((t) => t.file);
    for (const file of templates) {
      expect(file.startsWith('iconTemplate')).toBe(true);
    }
  });
});

describe('svgVariantFor', () => {
  const source = fs.readFileSync(SVG_PATH, 'utf8');

  it('template mode swaps the brand-gradient fill for pure black', () => {
    const svg = svgVariantFor(source, 'template');
    expect(svg).toContain('fill="#000000"');
    expect(svg).not.toContain('fill="url(#brand)"');
  });

  it('color mode keeps the brand gradient untouched', () => {
    const svg = svgVariantFor(source, 'color');
    expect(svg).toBe(source);
    expect(svg).toContain('fill="url(#brand)"');
  });

  it('the committed source SVG carries the gradient marker the template swap relies on', () => {
    // Guards the replace() contract: if the source SVG's fill attribute is
    // renamed, the template render silently keeps the gradient. Fail loudly here.
    expect(source).toContain('fill="url(#brand)"');
    expect(source).toContain('stop-color="#fa709a"');
    expect(source).toContain('stop-color="#ffe13f"');
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
    const html = pageHtmlFor('<svg/>', 16);
    expect(html).toContain('*{margin:0;padding:0}');
  });
});
