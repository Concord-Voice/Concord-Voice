#!/usr/bin/env node
/**
 * Rasterizes the tray icon PNGs from assets/tray/tray-source.svg via
 * Playwright's bundled Chromium (@playwright/test is already a devDependency;
 * same pattern as render-dmg-background.mjs).
 *
 * Outputs (committed as static assets — re-run when the source SVG changes):
 *   assets/tray/iconTemplate.png      16×16  black+alpha (macOS template)
 *   assets/tray/iconTemplate@2x.png   32×32  black+alpha (macOS retina)
 *   assets/tray/icon.png              16×16  brand gradient (Windows)
 *   assets/tray/icon@2x.png           32×32  brand gradient (Windows HiDPI)
 *   assets/tray/icon-22.png           22×22  brand gradient (Linux SNI)
 *
 * Pure helpers (TARGETS, svgVariantFor, pageHtmlFor) are exported for
 * scripts/generate-tray-icons.test.ts; the Playwright entry below only runs
 * on direct invocation (node scripts/generate-tray-icons.mjs), never on import.
 *
 * #1099. See [internal]specs/2026-06-10-1099-system-tray-design.md §5.
 */
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import fs from 'node:fs';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const TRAY_DIR = path.join(__dirname, '..', 'assets', 'tray');
const SVG_PATH = path.join(TRAY_DIR, 'tray-source.svg');

// 'template' renders the glyph pure #000 + alpha (macOS recolors via the
// Template filename convention); 'color' keeps the brand gradient.
export const TARGETS = [
  { file: 'iconTemplate.png', size: 16, mode: 'template' },
  { file: 'iconTemplate@2x.png', size: 32, mode: 'template' },
  { file: 'icon.png', size: 16, mode: 'color' },
  { file: 'icon@2x.png', size: 32, mode: 'color' },
  { file: 'icon-22.png', size: 22, mode: 'color' },
];

/** Template mode swaps the brand-gradient fill for pure black; color keeps it. */
export function svgVariantFor(svgSource, mode) {
  return mode === 'template'
    ? svgSource.replace('fill="url(#brand)"', 'fill="#000000"')
    : svgSource;
}

/** Minimal page shell sizing the svg to the exact raster target. */
export function pageHtmlFor(svg, size) {
  return `<!doctype html><style>*{margin:0;padding:0}svg{display:block;width:${size}px;height:${size}px}</style>${svg}`;
}

async function main() {
  // Imported lazily so test imports of the pure helpers never pull Playwright.
  const { chromium } = await import('@playwright/test');
  const svgSource = fs.readFileSync(SVG_PATH, 'utf8');
  const browser = await chromium.launch();
  try {
    for (const { file, size, mode } of TARGETS) {
      const page = await browser.newPage({ viewport: { width: size, height: size } });
      await page.setContent(pageHtmlFor(svgVariantFor(svgSource, mode), size));
      await page.screenshot({ path: path.join(TRAY_DIR, file), omitBackground: true });
      await page.close();
      console.log(`wrote assets/tray/${file} (${size}x${size}, ${mode})`);
    }
  } finally {
    await browser.close();
  }
}

// Run only when invoked directly (node scripts/generate-tray-icons.mjs) —
// never as an import side effect (the test imports the pure helpers above).
const isDirectRun = process.argv[1] && path.resolve(process.argv[1]) === __filename;
if (isDirectRun) {
  await main();
}
