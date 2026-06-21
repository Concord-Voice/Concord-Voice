#!/usr/bin/env node
/**
 * Renders the DMG background PNG (1x and 2x) from the HTML mockup at
 * [internal]artifacts/2026-05-22-dmg-background-mockup.html.
 *
 * Output paths:
 *   client/desktop/build/dmg-background.png      — 540×380 (1x)
 *   client/desktop/build/dmg-background@2x.png   — 1080×760 (retina)
 *
 * Invoked manually after editing the mockup, OR by CI if we ever want
 * deterministic per-commit rendering (currently the PNGs are checked
 * in as static assets — re-run this script whenever the mockup changes).
 *
 * Phase 1 of #1159. See:
 *   - [internal]specs/2026-05-22-1159-macos-install-ux-design.md
 *   - [internal]artifacts/2026-05-22-macos-install-ux-handoff.md
 */
import { chromium } from '@playwright/test';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import fs from 'node:fs';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
// client/desktop/scripts/ -> repo root is 3 levels up
const REPO_ROOT = path.resolve(__dirname, '..', '..', '..');
const HTML_PATH = path.join(
  REPO_ROOT,
  '.claude',
  'artifacts',
  '2026-05-22-dmg-background-mockup.html'
);
const BUILD_DIR = path.join(__dirname, '..', 'build');

if (!fs.existsSync(HTML_PATH)) {
  console.error(`HTML mockup not found at ${HTML_PATH}`);
  process.exit(1);
}
fs.mkdirSync(BUILD_DIR, { recursive: true });

async function renderAt(scaleFactor, outName) {
  let browser = null;
  try {
    browser = await chromium.launch();
    const context = await browser.newContext({
      // Viewport covers the simulated chrome (30px) + canvas (380px) in the mockup.
      viewport: { width: 540, height: 410 },
      deviceScaleFactor: scaleFactor,
    });
    const page = await context.newPage();
    await page.goto(`file://${HTML_PATH}`, { waitUntil: 'networkidle' });
    // Strip developer-reference placeholders that exist only in the mockup
    // for design review. The real .app icon + Applications shortcut are
    // overlaid by macOS DMG at the configured (130, 200) / (410, 200)
    // coordinates from forge.config.ts.
    await page.evaluate(() => {
      document.querySelectorAll('.icon-slot').forEach((el) => el.remove());
    });
    // The mockup's .canvas element is exactly 540×380 — taking an element
    // screenshot omits the simulated chrome and any page padding.
    const canvas = page.locator('.canvas').first();
    await canvas.screenshot({
      path: path.join(BUILD_DIR, outName),
      omitBackground: false, // preserves the deep purple ground
      type: 'png',
    });
    console.log(`Rendered ${outName} at ${scaleFactor}x`);
  } finally {
    // Always close the browser even if launch/goto/screenshot threw —
    // otherwise the headless Chromium child process leaks on script error.
    if (browser !== null) {
      await browser.close().catch(() => {
        // Suppress secondary close-failure noise; the primary error
        // (already propagating through the throw) is what the caller needs.
      });
    }
  }
}

try {
  await renderAt(1, 'dmg-background.png');
  await renderAt(2, 'dmg-background@2x.png');
  console.log(`DMG backgrounds written to ${BUILD_DIR}`);
} catch (err) {
  console.error(`\n✗ render-dmg-background.mjs failed: ${err.message}`);
  if (err.message && err.message.includes("Executable doesn't exist")) {
    console.error(
      '\nHint: Playwright browsers are not installed.\n' +
      '  Run: npx playwright install chromium\n' +
      'Then re-run this script.\n'
    );
  }
  process.exit(1);
}
