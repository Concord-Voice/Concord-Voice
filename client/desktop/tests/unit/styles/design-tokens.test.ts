import { readdirSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { APP_FONT_IDS, themeBundledFontFor } from '@/renderer/utils/ui/effectiveFont';

const ALL_23_TOKENS = [
  // State (3)
  '--state-selected',
  '--state-focused',
  '--state-hover',
  // Typography (4)
  '--font-display-stack',
  '--font-body-stack',
  '--font-display-tracking',
  '--font-display-tracking-tight',
  // Display (4) — 4-step display type scale (#1035)
  '--text-display-xl',
  '--text-display-lg',
  '--text-display-md',
  '--text-display-sm',
  // Scale (3)
  '--radius-base',
  '--radius-elevated',
  '--radius-modal',
  // Motion (5)
  '--motion-duration-fast',
  '--motion-duration-base',
  '--motion-duration-slow',
  '--motion-curve-base',
  '--motion-curve-decel',
  // Link (3)
  '--link-color',
  '--link-color-hover',
  '--link-color-visited',
  // Encryption (1)
  '--state-encryption-pending',
] as const;

const ALL_32_BLOCKS = [
  ':root',
  "[data-theme='light']",
  "[data-scheme='concord']",
  "[data-scheme='concord'][data-theme='light']",
  "[data-scheme='morky']",
  "[data-scheme='morky'][data-theme='light']",
  "[data-scheme='bardic']",
  "[data-scheme='bardic'][data-theme='light']",
  "[data-scheme='foxden']",
  "[data-scheme='foxden'][data-theme='light']",
  "[data-scheme='hacker']",
  "[data-scheme='hacker'][data-theme='light']",
  "[data-scheme='spooky']",
  "[data-scheme='spooky'][data-theme='light']",
  "[data-scheme='leviathan']",
  "[data-scheme='leviathan'][data-theme='light']",
  "[data-scheme='grassynill']",
  "[data-scheme='grassynill'][data-theme='light']",
  "[data-scheme='cottoncandy']",
  "[data-scheme='cottoncandy'][data-theme='light']",
  "[data-scheme='driftwood']",
  "[data-scheme='driftwood'][data-theme='light']",
  "[data-scheme='eclipse']",
  "[data-scheme='eclipse'][data-theme='light']",
  "[data-scheme='midnightsky']",
  "[data-scheme='midnightsky'][data-theme='light']",
  "[data-scheme='agency']",
  "[data-scheme='agency'][data-theme='light']",
  "[data-scheme='defacto']",
  "[data-scheme='defacto'][data-theme='light']",
  "[data-scheme='pride']",
  "[data-scheme='pride'][data-theme='light']",
] as const;

/**
 * Extract the body of a CSS block identified by `selector`.
 *
 * Uses plain string search (indexOf) rather than a dynamic RegExp so that
 * Semgrep's ReDoS taint-sink rule (CWE-1333) is not triggered. The selector
 * strings in ALL_32_BLOCKS are compile-time constants, but the static analyser
 * cannot prove that without data-flow analysis across array indexing.
 *
 * Algorithm:
 *   1. Find the first occurrence of `\n<selector> {` or `<selector> {` at
 *      position 0 (for `:root`).
 *   2. From the opening `{`, count braces until depth returns to 0.
 *   3. Return the substring between `{` and the matching `}` (exclusive).
 *
 * Returns null if the selector is not found or braces are unbalanced.
 */
function extractBlockBody(css: string, selector: string): string | null {
  // Build the search needle: look for the selector followed by optional
  // whitespace and `{`.  We check both `\n<selector> {` and `<selector> {`
  // at position 0 to handle the first block in the file (`:root`).
  const needle = `${selector} {`;
  const needleNl = `\n${selector} {`;

  let openBracePos: number;

  if (css.startsWith(needle)) {
    openBracePos = needle.length; // points to char after `{`
  } else {
    const idx = css.indexOf(needleNl);
    if (idx === -1) {
      // Also try with no space before brace (e.g. `selector{`)
      const needleNoSp = `${selector}{`;
      const needleNlNoSp = `\n${selector}{`;
      const idx2 = css.startsWith(needleNoSp) ? 0 : css.indexOf(needleNlNoSp);
      if (idx2 === -1) {
        return null;
      }
      const rawNeedle = css.startsWith(needleNoSp) ? needleNoSp : needleNlNoSp;
      openBracePos = idx2 + rawNeedle.length;
    } else {
      openBracePos = idx + needleNl.length; // points to char after `{`
    }
  }

  let depth = 1;
  let pos = openBracePos;

  while (pos < css.length && depth > 0) {
    const ch = css[pos];
    if (ch === '{') {
      depth++;
    } else if (ch === '}') {
      depth--;
    }
    pos++;
  }

  if (depth !== 0) {
    return null;
  }

  return css.slice(openBracePos, pos - 1);
}

describe('design-token schema symmetry', () => {
  // From client/desktop/tests/unit/styles/ the relative path to
  // client/desktop/src/renderer/styles/index.css is ../../../src/renderer/styles/index.css
  const cssPath = resolve(__dirname, '../../../src/renderer/styles/index.css');
  const css = readFileSync(cssPath, 'utf-8');

  it('CSS file selector count matches ALL_32_BLOCKS array length (drift guard)', () => {
    // Counts top-level theme-block selectors. Without this check, adding a
    // 33rd block to index.css (e.g., a new color scheme) without updating
    // ALL_32_BLOCKS would let the per-block tests pass (existing 32 still
    // declared) while leaving the new block unchecked. This assertion makes
    // that drift loud.
    //
    // `:root` must be followed by `{` here, so this counts the BASE theme block only. No
    // font rule may add a `:root {` block (the #2366 font stacks live INSIDE the base
    // block), and the font-layer rules start with `[data-appfont=` / `[data-font-`,
    // which this pattern does not match. The sink's own coverage is asserted below.
    //
    // Literal regex (not constructed from variables) — Semgrep CWE-1333
    // ReDoS taint applies only to dynamic RegExp construction.
    const matches = css.match(/^(?::root\s*\{|\[data-(?:scheme|theme)=)/gm);
    expect(matches?.length).toBe(ALL_32_BLOCKS.length);
  });

  describe.each(ALL_32_BLOCKS)('block %s', (block) => {
    const blockBody = extractBlockBody(css, block);

    it('block exists in styles/index.css', () => {
      expect(blockBody).not.toBeNull();
    });

    it.each(ALL_23_TOKENS)('declares %s', (token) => {
      // Use plain string search to avoid dynamic RegExp construction (Semgrep
      // CWE-1333).  CSS custom property declarations use `token:` or `token :`
      // — check both forms.  `token` comes from ALL_23_TOKENS (as const), so
      // it is a compile-time constant, but the static analyser treats any
      // function-parameter RegExp as a taint sink.
      const body = blockBody ?? '';
      const declared = body.includes(`${token}:`) || body.includes(`${token} :`);
      expect(declared).toBe(true);
    });
  });
});

/**
 * #2366 — the application-font sink must cover every font the picker offers.
 *
 * The defect this guards is the one #2366 fixed: a font id can exist in the picker, be
 * typed, be selectable, write `appFont`, reach the DOM as `data-appfont` — and resolve
 * to no CSS at all, so choosing it does nothing. Every layer above the CSS passes its
 * own tests while the feature is dead.
 *
 * Both sides are read as TEXT rather than imported: `FONT_OPTIONS` is a module-local
 * const in a .tsx that imports React and CSS, and pulling that in would buy a jsdom
 * dependency to learn seven strings. Reading both sources also means drift in EITHER
 * direction fails — a font added to the picker with no rule, and a rule left behind for
 * a font the picker dropped.
 *
 * Two shapes, which are not an inconsistency. The body font is INHERITED, so
 * `[data-appfont='<id>'] body` reaches every surface that inherits, and the controls
 * that cannot inherit (`<input>`, `<select>`, `<textarea>`, `<button>`) say
 * `font-family: inherit`. The display font is NOT inherited; it is a token each surface
 * opts into by name, so the Headings layer redefines the token — on `body`, never on
 * :root (#2366). A region layer sets both. Every rule sits on `body` or deeper, so it
 * wins over the theme tokens on <html> by inheritance rather than by specificity.
 *
 * This is a source-parse check, not a rendering one. jsdom cannot resolve custom
 * properties at all, so whether a rule WINS its cascade is unanswerable here and is
 * asserted in real Chromium by tests/e2e/design-tokens.spec.ts. Green here means the
 * rules are present, never that they apply.
 */
describe('application-font sink (#2366)', () => {
  const cssPath = resolve(__dirname, '../../../src/renderer/styles/index.css');
  const css = readFileSync(cssPath, 'utf-8');
  const pickerPath = resolve(
    __dirname,
    '../../../src/renderer/components/Settings/FontSection.tsx'
  );
  const picker = readFileSync(pickerPath, 'utf-8');

  // Start at the `= [` rather than at the declaration, and end at a `];` that begins a
  // line. The obvious `indexOf('];')` can land INSIDE a type annotation — this one once
  // read `{ id: AppearanceSettings['appFont']; … }` and so contained `];` before the
  // array opened — leaving an empty slice, zero parsed ids, and every assertion below
  // running against nothing. The guard below caught exactly that.
  const optionsStart = picker.indexOf('const FONT_OPTIONS');
  const arrayStart = picker.indexOf('= [', optionsStart);
  const optionsEnd = picker.indexOf('\n];', arrayStart);
  const pickerIds = [...picker.slice(arrayStart, optionsEnd).matchAll(/id: '([a-z]+)'/g)].map(
    (m) => m[1]
  );

  it('the picker literal was located and parsed (guards a vacuous pass)', () => {
    // Without this, a rename of FONT_OPTIONS would leave `pickerIds` empty and the
    // assertions below would pass by iterating nothing.
    expect(optionsStart).toBeGreaterThan(-1);
    expect(pickerIds.length).toBeGreaterThan(1);
    expect(pickerIds).toContain('default');
  });

  it('every non-default picker font has a body rule', () => {
    const missing = pickerIds
      .filter((id) => id !== 'default')
      .filter((id) => !css.includes(`[data-appfont='${id}'] body`));
    expect(missing).toEqual([]);
  });

  it("'default' deliberately has NO rule on any layer — the theme's faces stand", () => {
    expect(css).not.toMatch(/data-(?:appfont|font-[a-z]+)='default'/);
  });

  const explicitIds = APP_FONT_IDS.filter((id) => id !== 'default');
  // Prettier breaks long selector lists across lines; match on collapsed whitespace.
  const flat = css.replace(/\s+/g, ' ');

  it('every explicit font id has a rule on every layer (#2366)', () => {
    const missing = explicitIds
      .flatMap((id) => [
        `[data-appfont='${id}'] body`,
        `[data-font-headings='${id}'] body`,
        `[data-font-nav='${id}'] :is(`,
        // The empty/loading state renders .message-list-empty with no
        // .message-list ancestor, so it is named explicitly.
        `[data-font-messages='${id}'] :is(.message-list, .message-list-empty)`,
      ])
      .filter((selector) => !flat.includes(selector));
    expect(explicitIds.length).toBeGreaterThan(5);
    expect(missing).toEqual([]);
  });

  it('every explicit font id has one stack variable in the base :root block', () => {
    const root = extractBlockBody(css, ':root') ?? '';
    expect(explicitIds.filter((id) => !root.includes(`--font-stack-${id}:`))).toEqual([]);
    expect(root).toContain('--font-brand-stack: var(--font-display-stack);');
  });

  it('no font rule sits on :root — every override is on body or deeper (#2366)', () => {
    // Retires the (0,2,0) `:root[data-appfont='opendyslexic']` rule, which only TIED
    // `[data-scheme][data-theme]` and had to stay last in the file.
    expect(css).not.toMatch(/:root\[data-(?:appfont|font-)/);
  });

  it('only the Dyslexic Support brand value redefines the wordmark stack', () => {
    const brandValues = [...css.matchAll(/\[data-font-brand='([a-z]+)'\]/g)].map((m) => m[1]);
    expect(brandValues).toEqual(['opendyslexic']);
    expect(extractBlockBody(css, "[data-font-brand='opendyslexic'] body")).toContain(
      '--font-brand-stack:'
    );
  });

  // A renamed region class, or a state rendered outside the region (the empty/loading
  // .message-list-empty was), silently drops that area's font. Pin both ends.
  it('area rules name one region list per layer, and each class still exists', () => {
    const regions = (layer: 'nav' | 'messages') => {
      const lists = [
        ...flat.matchAll(new RegExp(`\\[data-font-${layer}='[a-z]+'\\] :is\\(([^)]*)\\)`, 'g')),
      ].map((m) =>
        m[1]
          .split(',')
          .map((c) => c.trim().replace(/^\./, ''))
          .sort()
          .join(' ')
      );
      expect(lists.length).toBe(explicitIds.length * 2); // plain + [data-scheme] variant
      expect(new Set(lists).size).toBe(1);
      return lists[0].split(' ');
    };
    const nav = regions('nav');
    const messages = regions('messages');
    expect(nav).toEqual([
      'layout-channel-panel',
      'layout-folder-bar',
      'layout-member-space',
      'layout-server-bar',
    ]);
    expect(messages).toEqual(['message-list', 'message-list-empty']);
    const tsx = ['components/Layout/AppLayout.tsx', 'components/Chat/MessageList.tsx']
      .map((f) => readFileSync(resolve(__dirname, '../../../src/renderer', f), 'utf-8'))
      .join('\n');
    for (const cls of [...nav, ...messages]) expect(tsx).toContain(`className="${cls}"`);
  });

  it('headings and area rules also reach a themed scope root (#2366)', () => {
    // useUserThemeScope puts data-scheme on another user's profile surface, which
    // re-matches a theme block there; only a [data-scheme] rule outranks it.
    const missing = explicitIds
      .flatMap((id) => [
        `[data-font-headings='${id}'] body [data-scheme]`,
        `[data-font-messages='${id}'] :is(.message-list, .message-list-empty) [data-scheme]`,
      ])
      .filter((selector) => !flat.includes(selector));
    expect(missing).toEqual([]);
    expect(flat.match(/\[data-font-nav='[a-z]+'\] :is\([^)]*\) \[data-scheme\]/g)).toHaveLength(
      explicitIds.length
    );
  });

  // Concord Voice Default drives the derived Headings value 'concord', which is not an
  // AppFontId, so the per-id loops above never see it.
  it("the derived 'concord' headings value has its stack and both rules (#3457)", () => {
    const root = extractBlockBody(css, ':root') ?? '';
    expect(root).toContain("--font-stack-concord: 'Droidiga', system-ui, sans-serif;");
    for (const selector of [
      "[data-font-headings='concord'] body",
      "[data-font-headings='concord'] body [data-scheme]",
    ]) {
      expect(flat).toContain(selector);
    }
  });

  // The "Active with the current theme" chip marks Concord Voice Default as what Theme
  // Default applies on every non-bundling theme. That is only true while each such block
  // declares the same display face the 'concord' pin applies — a scheme that changes its
  // heading face without bundling a body font would make the chip lie.
  it('every non-bundling theme block declares the display face Concord Voice Default pins', () => {
    const pinned = /--font-stack-concord:\s*([^;]+);/.exec(
      extractBlockBody(css, ':root') ?? ''
    )?.[1];
    expect(pinned).toBeDefined();
    const drifted: string[] = [];
    let checked = 0;
    for (const block of ALL_32_BLOCKS) {
      const scheme = /data-scheme='([a-z]+)'/.exec(block)?.[1];
      if (scheme && themeBundledFontFor(scheme as Parameters<typeof themeBundledFontFor>[0])) {
        continue;
      }
      const declared = /--font-display-stack:\s*([^;]+);/.exec(
        extractBlockBody(css, block) ?? ''
      )?.[1];
      checked++;
      if (declared?.trim() !== pinned?.trim()) drifted.push(`${block}: ${declared}`);
    }
    expect(checked).toBe(30);
    expect(drifted).toEqual([]);
  });

  // A control that names a body family itself ignores every font setting, Dyslexic
  // Support included (CategoryManagerPanel's input used --font-body-stack).
  it('renderer CSS never hard-codes a body font outside the two deliberate places', () => {
    const renderer = resolve(__dirname, '../../../src/renderer');
    const allowed = /^(inherit|var\(--font-(display|brand)-stack\)|var\(--font-stack-[a-z]+\))$/;
    const deliberate = new Set([
      'components/Auth/SSOButton.css', // Google's sign-in branding mandates Roboto
      'styles/index.css', // the base body rule every layer starts from
    ]);
    const offenders: string[] = [];
    for (const entry of readdirSync(renderer, { recursive: true }) as string[]) {
      const rel = entry.replaceAll('\\', '/'); // Windows returns backslash-separated paths
      if (!rel.endsWith('.css') || deliberate.has(rel)) continue;
      const text = readFileSync(resolve(renderer, rel), 'utf-8')
        .replace(/@font-face\s*\{[^}]*\}/g, '')
        .replace(/\/\*[\s\S]*?\*\//g, '');
      for (const [, value] of text.matchAll(/font-family:\s*([^;]+);/g)) {
        const v = value.trim();
        if (!allowed.test(v) && !v.includes('monospace')) offenders.push(`${rel}: ${v}`);
      }
    }
    expect(offenders).toEqual([]);
  });

  it('form controls inherit the body font through a zero-specificity reset', () => {
    // Chromium gives controls the system font; without this a bare <button> ignores
    // every font setting, Dyslexic Support included.
    expect(flat).toContain(':where(button, input, select, textarea) { font-family: inherit; }');
  });

  it('the titlebar wordmark reads the brand stack, not the display stack', () => {
    const titlebar = readFileSync(
      resolve(__dirname, '../../../src/renderer/components/Titlebar/Titlebar.css'),
      'utf-8'
    );
    expect(titlebar).toContain('var(--font-brand-stack)');
    expect(titlebar).not.toContain('var(--font-display-stack)');
  });
});
