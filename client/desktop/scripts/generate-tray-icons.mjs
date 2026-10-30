#!/usr/bin/env node
/**
 * Rasterizes the status-aware tray sprite set from the layered glyph in
 * assets/tray/tray-source.svg via Playwright's bundled Chromium
 * (@playwright/test is already a devDependency).
 *
 * A frame key is "<c>.<l>.<s>": one token per glyph layer (c crescent,
 * l large sparkle, s small sparkle). assets/tray/frames.json is the closed
 * list of keys the main-process resolver can produce. Every key is rendered in
 * four modes and three sizes:
 *
 *   assets/tray/<mode>/<key>.png      16x16
 *   assets/tray/<mode>/<key>@2x.png   32x32
 *   assets/tray/<mode>/<key>-22.png   22x22 (Linux)
 *
 * Modes: template (pure #000 + alpha, macOS tints it), white, black, colour.
 * Also written: assets/tray/manifest.json, assets/tray-sheet/contact-sheet.png
 * (review aid, deliberately outside assets/tray so it never ships) and the
 * idle previews src/renderer/assets/tray-preview/<mode>.png for Settings.
 *
 * Pure helpers are exported for scripts/generate-tray-icons.test.ts; the
 * Playwright entry only runs on direct invocation, never on import.
 *
 * See [internal]specs/2026-10-29-tray-status-design.md §2, §2a.
 */
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import fs from 'node:fs';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const DESKTOP_DIR = path.join(__dirname, '..');
const TRAY_DIR = path.join(DESKTOP_DIR, 'assets', 'tray');
const SHEET_DIR = path.join(DESKTOP_DIR, 'assets', 'tray-sheet');
const PREVIEW_DIR = path.join(DESKTOP_DIR, 'src', 'renderer', 'assets', 'tray-preview');
const SVG_PATH = path.join(TRAY_DIR, 'tray-source.svg');
const FRAMES_PATH = path.join(TRAY_DIR, 'frames.json');

export const MAX_KEYS = 60;
export const MODES = ['template', 'white', 'black', 'colour'];
export const SIZES = { darwin: [16, 32], win32: [16, 32], linux: [22] };
const ALL_SIZES = [16, 32, 22];

/** Tray colours (spec §2). Never raw #ffe13f on its own. */
export const COLOURS = {
  green: '#2f9e6a',
  red: '#f04747',
  pink: '#e0507e',
  amber: '#c47a00',
  blue: '#3a86d4',
};
const MONO = { template: '#000000', white: '#ffffff', black: '#000000' };

const BURST_SCALES = [1.0, 1.15, 1.3, 1.35, 1.3, 1.15, 1.0];
const L_CENTER = [54.45, 33.07];
const S_CENTER = [74.93, 55.54];

const burstTokens = {};
BURST_SCALES.forEach((scale, i) => {
  burstTokens[`burst${i + 1}`] = { opacity: 1, fill: 'brand', scale };
});

/**
 * Token tables per layer. fill is a role: mono modes collapse every role to the
 * mode colour, colour mode maps roles to the brand gradient or a tray colour.
 */
export const TOKENS = {
  c: {
    full: { opacity: 1, fill: 'brand' },
    dim35: { opacity: 0.35, fill: 'brand' },
    dim45: { opacity: 0.45, fill: 'brand' },
    outline: { opacity: 1, fill: 'brand', shape: 'outline' },
    green: { opacity: 1, fill: 'green', groove: true },
    green35: { opacity: 0.35, fill: 'green' },
    amber: { opacity: 1, fill: 'amber', shape: 'outline' },
  },
  l: {
    full: { opacity: 1, fill: 'brand' },
    dim35: { opacity: 0.35, fill: 'brand' },
    lit: { opacity: 1, fill: 'brand', scale: 1.2 },
    slash: { opacity: 0.35, fill: 'brand', slash: 'short' },
    slashlong: { opacity: 1, fill: 'brand', slash: 'long' },
    breath70: { opacity: 0.7, fill: 'brand' },
    breath40: { opacity: 0.4, fill: 'brand' },
    ...burstTokens,
  },
  s: {
    star: { opacity: 1, fill: 'brand' },
    // ponytail: key keeps its name; the ~3 px sparkle vanishes on a dark bar below 0.6.
    dim35: { opacity: 0.6, fill: 'brand' },
    disc: { opacity: 1, fill: 'brand', shape: 'disc', r: 11 },
    discmention: { opacity: 1, fill: 'pink', shape: 'disc', r: 14.5 },
    discupdate: { opacity: 1, fill: 'blue', shape: 'disc', r: 11, hole: 3.6 },
    ring: { opacity: 1, fill: 'red', shape: 'ring', r: 10.2, stroke: 4.6 },
  },
};

const SLASHES = {
  short: { x1: 42, y1: 56, x2: 70, y2: 12 },
  long: { x1: 22, y1: 83, x2: 86, y2: 10 },
};

/** Reads the three layer path data out of the committed source svg by group id. */
export function readSourcePaths(svgSource) {
  const grab = (id) => {
    const m = new RegExp(`<g id="${id}">\\s*<path[^>]* d="([^"]+)"`).exec(svgSource);
    if (!m) throw new Error(`tray-source.svg is missing group #${id}`);
    return m[1];
  };
  return { c: grab('crescent'), l: grab('sparkle-lg'), s: grab('sparkle-sm') };
}

let cachedPaths = null;
function sourcePaths() {
  cachedPaths ??= readSourcePaths(fs.readFileSync(SVG_PATH, 'utf8'));
  return cachedPaths;
}

export function parseKey(key) {
  const parts = String(key).split('.');
  if (parts.length !== 3) throw new Error(`bad frame key "${key}"`);
  const [c, l, s] = parts;
  const tc = TOKENS.c[c];
  const tl = TOKENS.l[l];
  const ts = TOKENS.s[s];
  if (!tc || !tl || !ts) throw new Error(`unknown token in frame key "${key}"`);
  return { c: tc, l: tl, s: ts };
}

export function loadFrameKeys() {
  const parsed = JSON.parse(fs.readFileSync(FRAMES_PATH, 'utf8'));
  return parsed.keys;
}

const f = (n) => Number(n.toFixed(2));

/**
 * Pure: the svg for one frame key in one render mode. Every layer is always
 * emitted (alpha floor 35%); the slash and halo are knockouts so they read on
 * every fill.
 */
export function svgForFrame(key, mode) {
  if (!MODES.includes(mode)) throw new Error(`unknown mode "${mode}"`);
  const { c, l, s } = parseKey(key);
  const paths = sourcePaths();
  const colour = mode === 'colour';
  const paint = (role) => {
    if (!colour) return MONO[mode];
    return role === 'brand' ? 'url(#brand)' : COLOURS[role];
  };

  const knock = [];
  const slashSpec = l.slash ? SLASHES[l.slash] : null;
  if (slashSpec) {
    knock.push(
      `<line x1="${slashSpec.x1}" y1="${slashSpec.y1}" x2="${slashSpec.x2}" y2="${slashSpec.y2}" stroke="#000000" stroke-width="15" stroke-linecap="round"/>`
    );
  }
  if (s.shape) {
    const halo = (s.r ?? 11) + (s.stroke ? s.stroke / 2 : 0) + 4.5;
    knock.push(`<circle cx="${S_CENTER[0]}" cy="${S_CENTER[1]}" r="${f(halo)}" fill="#000000"/>`);
  }
  if (c.groove) {
    knock.push(
      '<path d="M13 31A40 40 0 0 0 13 69" fill="none" stroke="#000000" stroke-width="3.4" stroke-linecap="round"/>'
    );
  }

  const defs = [];
  if (colour) {
    defs.push(
      '<linearGradient id="brand" gradientUnits="userSpaceOnUse" x1="0" y1="100" x2="0" y2="0"><stop offset="0%" stop-color="#fa709a"/><stop offset="100%" stop-color="#ffe13f"/></linearGradient>'
    );
  }
  if (knock.length > 0) {
    defs.push(
      `<mask id="knock" maskUnits="userSpaceOnUse" x="0" y="0" width="100" height="100"><rect width="100" height="100" fill="#ffffff"/>${knock.join('')}</mask>`
    );
  }

  const layerAttrs = (t, extra = '') => {
    const p = paint(t.fill);
    const outline = t.shape === 'outline';
    const fillStroke = outline
      ? `fill="none" stroke="${p}" stroke-width="6.5" stroke-linejoin="round"`
      : `fill="${p}"`;
    return `${fillStroke} opacity="${t.opacity}"${extra}`;
  };
  const scaled = (t, center) =>
    t.scale
      ? ` transform="translate(${center[0]} ${center[1]}) scale(${t.scale}) translate(${-center[0]} ${-center[1]})"`
      : '';

  const cEl = `<path data-piece="c" d="${paths.c}" ${layerAttrs(c)}/>`;
  const lEl = `<path data-piece="l" d="${paths.l}" ${layerAttrs(l, scaled(l, L_CENTER))}/>`;

  let sEl;
  if (!s.shape) {
    sEl = `<path data-piece="s" d="${paths.s}" ${layerAttrs(s)}/>`;
  } else if (s.shape === 'ring') {
    sEl = `<circle data-piece="s" cx="${S_CENTER[0]}" cy="${S_CENTER[1]}" r="${s.r}" fill="none" stroke="${paint(s.fill)}" stroke-width="${s.stroke}" opacity="${s.opacity}"/>`;
  } else if (s.hole) {
    // Update-ready disc: a centre knockout tells it apart from the plain unread disc in mono.
    const d = `M${f(S_CENTER[0] - s.r)} ${S_CENTER[1]}a${s.r} ${s.r} 0 1 0 ${2 * s.r} 0a${s.r} ${s.r} 0 1 0 ${-2 * s.r} 0zM${f(S_CENTER[0] - s.hole)} ${S_CENTER[1]}a${s.hole} ${s.hole} 0 1 1 ${2 * s.hole} 0a${s.hole} ${s.hole} 0 1 1 ${-2 * s.hole} 0z`;
    sEl = `<path data-piece="s" fill-rule="evenodd" d="${d}" fill="${paint(s.fill)}" opacity="${s.opacity}"/>`;
  } else {
    sEl = `<circle data-piece="s" cx="${S_CENTER[0]}" cy="${S_CENTER[1]}" r="${s.r}" fill="${paint(s.fill)}" opacity="${s.opacity}"/>`;
  }

  const slashEl = slashSpec
    ? `<line x1="${slashSpec.x1}" y1="${slashSpec.y1}" x2="${slashSpec.x2}" y2="${slashSpec.y2}" stroke="${paint('red')}" stroke-width="6.5" stroke-linecap="round"/>`
    : '';

  const body = knock.length > 0 ? `<g mask="url(#knock)">${cEl}${lEl}</g>` : `${cEl}${lEl}`;
  const defsEl = defs.length > 0 ? `<defs>${defs.join('')}</defs>` : '';
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100">${defsEl}${body}${sEl}${slashEl}</svg>`;
}

export function fileNameFor(key, size) {
  if (size === 16) return `${key}.png`;
  if (size === 32) return `${key}@2x.png`;
  if (size === 22) return `${key}-22.png`;
  throw new Error(`unsupported size ${size}`);
}

export function manifestFor(keys) {
  return { keys: [...keys], modes: [...MODES], sizes: SIZES };
}

/** Minimal page shell sizing the svg to the exact raster target. */
export function pageHtmlFor(svg, size) {
  return `<!doctype html><style>*{margin:0;padding:0}svg{display:block;width:${size}px;height:${size}px}</style>${svg}`;
}

function sheetHtml(keys, pngDir) {
  const rowDefs = [
    { mode: 'template', bg: '#f2f2f2', invert: false, label: 'template on a light bar' },
    { mode: 'template', bg: '#2b2b2b', invert: true, label: 'template on a dark bar (tinted white)' },
    { mode: 'white', bg: '#2b2b2b', invert: false, label: 'white on dark' },
    { mode: 'black', bg: '#f2f2f2', invert: false, label: 'black on light' },
    { mode: 'colour', bg: '#f2f2f2', invert: false, label: 'colour on light' },
    { mode: 'colour', bg: '#2b2b2b', invert: false, label: 'colour on dark' },
  ];
  const sections = rowDefs
    .map(({ mode, bg, invert, label }) => {
      const dark = bg === '#2b2b2b';
      const cells = keys
        .map((key) => {
          const b64 = fs
            .readFileSync(path.join(pngDir, mode, fileNameFor(key, 32)))
            .toString('base64');
          const b16 = fs
            .readFileSync(path.join(pngDir, mode, fileNameFor(key, 16)))
            .toString('base64');
          const filter = invert ? 'filter:invert(1);' : '';
          return `<div class="cell"><img style="${filter}" src="data:image/png;base64,${b64}" width="64" height="64"><img style="${filter}" src="data:image/png;base64,${b16}" width="16" height="16"><div class="k">${key}</div></div>`;
        })
        .join('');
      return `<section style="background:${bg};color:${dark ? '#ddd' : '#222'}"><h2>${label}</h2><div class="grid">${cells}</div></section>`;
    })
    .join('');
  return `<!doctype html><meta charset="utf-8"><style>
body{margin:0;font:11px sans-serif;width:1500px}
section{padding:8px 10px}
h2{margin:0 0 6px;font-size:13px}
.grid{display:flex;flex-wrap:wrap;gap:4px}
.cell{width:80px;text-align:center}
.cell img{image-rendering:pixelated}
.k{font-size:8px;word-break:break-all}
</style>${sections}`;
}

async function main() {
  // Imported lazily so test imports of the pure helpers never pull Playwright.
  const { chromium } = await import('@playwright/test');
  const keys = loadFrameKeys();
  if (keys.length > MAX_KEYS) {
    throw new Error(`frames.json has ${keys.length} keys; the ceiling is ${MAX_KEYS} (re-panel the product change)`);
  }
  for (const key of keys) parseKey(key);

  const browser = await chromium.launch();
  let written = 0;
  try {
    for (const size of ALL_SIZES) {
      const page = await browser.newPage({ viewport: { width: size, height: size } });
      for (const mode of MODES) {
        fs.mkdirSync(path.join(TRAY_DIR, mode), { recursive: true });
        for (const key of keys) {
          await page.setContent(pageHtmlFor(svgForFrame(key, mode), size));
          await page.screenshot({
            path: path.join(TRAY_DIR, mode, fileNameFor(key, size)),
            omitBackground: true,
          });
          written += 1;
        }
      }
      await page.close();
    }

    fs.writeFileSync(
      path.join(TRAY_DIR, 'manifest.json'),
      `${JSON.stringify(manifestFor(keys), null, 2)}\n`
    );

    fs.mkdirSync(PREVIEW_DIR, { recursive: true });
    for (const mode of MODES) {
      fs.copyFileSync(
        path.join(TRAY_DIR, mode, fileNameFor('full.full.star', 32)),
        path.join(PREVIEW_DIR, `${mode}.png`)
      );
    }

    fs.mkdirSync(SHEET_DIR, { recursive: true });
    const sheet = await browser.newPage({ viewport: { width: 1500, height: 800 } });
    await sheet.setContent(sheetHtml(keys, TRAY_DIR));
    await sheet.screenshot({ path: path.join(SHEET_DIR, 'contact-sheet.png'), fullPage: true });
    await sheet.close();
  } finally {
    await browser.close();
  }
  console.log(`wrote ${written} sprites for ${keys.length} keys, manifest, previews and contact sheet`);
}

// Run only when invoked directly (node scripts/generate-tray-icons.mjs) —
// never as an import side effect (the test imports the pure helpers above).
const isDirectRun = process.argv[1] && path.resolve(process.argv[1]) === __filename;
if (isDirectRun) {
  await main();
}
