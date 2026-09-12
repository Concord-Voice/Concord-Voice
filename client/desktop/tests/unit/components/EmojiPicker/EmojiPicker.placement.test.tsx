import React from 'react';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';

// ─── Module mocks (mirrors EmojiPicker.test.tsx) ──────────────────────────

const mockLoadCategory = vi.fn().mockResolvedValue([]);
const mockGetCategory = vi.fn().mockReturnValue([]);
const mockSearch = vi.fn().mockReturnValue([]);
const mockLoadAllForSearch = vi.fn().mockResolvedValue(undefined);

vi.mock('@/renderer/components/EmojiPicker/useEmojiData', () => ({
  useEmojiData: () => ({
    categories: [],
    loadingCategory: null,
    loadCategory: mockLoadCategory,
    getCategory: mockGetCategory,
    search: mockSearch,
    loadAllForSearch: mockLoadAllForSearch,
  }),
}));

vi.mock('@/renderer/components/EmojiPicker/emojiDataCache', () => ({
  getRecentEmojis: () => [],
  addRecentEmoji: () => {},
  getSavedSkinTone: () => '',
  saveSkinTone: () => {},
}));

const { default: EmojiPicker } = await import('@/renderer/components/EmojiPicker/EmojiPicker');

// ─── Geometry helpers (mirrors AttributedPopover.test.tsx's `rect`/mock pattern) ───

const rect = (overrides: Partial<DOMRect> = {}): DOMRect =>
  ({
    x: 0,
    y: 0,
    left: 0,
    right: 0,
    top: 0,
    bottom: 0,
    width: 0,
    height: 0,
    toJSON: () => ({}),
    ...overrides,
  }) satisfies DOMRect;

/** Mocks `.emoji-picker`'s own measured box; every other element reads as a zero rect. */
function mockPickerRect(size: { width: number; height: number }) {
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (
    this: HTMLElement
  ) {
    if (this.classList.contains('emoji-picker')) {
      return rect({
        width: size.width,
        height: size.height,
        right: size.width,
        bottom: size.height,
      });
    }
    return rect();
  });

  // The ANCHORED branch measures offsetWidth/offsetHeight rather than the
  // rect, deliberately: getBoundingClientRect() reports the TRANSFORMED box,
  // and this picker animates in from `scale(0.95) translateY(4px)`, so at
  // mount it measured 95% of its real size and placed itself ~20px too low --
  // driving its caret into the button it points at (#2370). jsdom runs no
  // animations and applies no transforms, so THAT defect is structurally
  // invisible here; these stubs exist only to keep the anchored and legacy
  // branches measuring the same box. The guard against a revert to
  // getBoundingClientRect is the source pin at the bottom of this file.
  vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockImplementation(function (
    this: HTMLElement
  ) {
    return this.classList.contains('emoji-picker') ? size.width : 0;
  });
  vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockImplementation(function (
    this: HTMLElement
  ) {
    return this.classList.contains('emoji-picker') ? size.height : 0;
  });
}

function normalizedHtml(el: Element): string {
  // Normalizes ONLY the two differences the fixtures are built to produce: the
  // arrow-suppression modifier, and the `top` that the differing `anchorTop`
  // yields. Everything else is compared VERBATIM -- `display`, `visibility`,
  // `left`, `--emoji-picker-arrow-x`, every other class -- so an unrelated
  // class or inline-style regression fails this test instead of being
  // normalized away. Stripping `class` and `style` wholesale (the first version
  // of this helper) discarded exactly the attributes most likely to regress.
  //
  // KNOWN LIMIT, stated rather than implied: `outerHTML` does not serialize
  // React event handlers, so this comparison CANNOT observe a conditional
  // handler. That third of spec §2.2 is not covered here, and the test name
  // no longer claims it.
  return el.outerHTML
    .replace(/\s*\bemoji-picker--no-arrow\b/, '')
    .replace(/top:\s*[^;"]*;?/, 'top:<normalized>;')
    .replace(/class="([^"]*)"/, (_m, c: string) => `class="${c.trim().replace(/\s+/g, ' ')}"`);
}

describe('EmojiPicker dual-mode placement (#2370 A10)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('legacy mode (no anchorCenterX) never applies the anchored or arrow classes', async () => {
    mockPickerRect({ width: 300, height: 200 });
    vi.stubGlobal('innerWidth', 1200);
    vi.stubGlobal('innerHeight', 800);

    render(
      <EmojiPicker
        onSelect={vi.fn()}
        onClose={vi.fn()}
        mode="popover"
        position={{ x: 100, y: 200 }}
      />
    );

    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--popover')).toBeInTheDocument()
    );
    const picker = document.body.querySelector('.emoji-picker') as HTMLElement;
    await waitFor(() => expect(picker.style.visibility).toBe('visible'));
    // Byte-identical to pre-#2370: no anchored class -> the base `::after`
    // rule's `display: none` is never overridden, so no arrow ever paints.
    expect(picker.className).not.toMatch(/emoji-picker--anchored/);
    expect(picker.className).not.toMatch(/emoji-picker--no-arrow/);
  });

  it('legacy mode flips above the anchor when there is no room below (pre-#2370 clamp-and-flip math)', async () => {
    mockPickerRect({ width: 300, height: 200 });
    vi.stubGlobal('innerWidth', 1000);
    vi.stubGlobal('innerHeight', 400);

    // position.y = 350: top + height(200) + margin(8) = 558 > vh(400), so the
    // legacy path flips: flipped = 350 - 200 - 8 = 142 (>= margin, so used as-is).
    render(
      <EmojiPicker
        onSelect={vi.fn()}
        onClose={vi.fn()}
        mode="popover"
        position={{ x: 100, y: 350 }}
      />
    );

    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--popover')).toBeInTheDocument()
    );
    const picker = document.body.querySelector('.emoji-picker') as HTMLElement;
    await waitFor(() => expect(picker.style.visibility).toBe('visible'));
    expect(picker.style.top).toBe('142px');
    expect(picker.style.left).toBe('100px');
  });

  it('legacy mode clamps to the bottom when flipping above would also run out of room', async () => {
    mockPickerRect({ width: 300, height: 200 });
    vi.stubGlobal('innerWidth', 1000);
    vi.stubGlobal('innerHeight', 400);

    // position.y = 200: top+height(200)+margin(8) = 408 > vh(400), so the
    // legacy path tries to flip -- but flipped = 200-200-8 = -8 (< margin 8),
    // so it clamps to the viewport bottom instead:
    // max(8, vh - height - margin) = max(8, 400-200-8=192) = 192.
    render(
      <EmojiPicker
        onSelect={vi.fn()}
        onClose={vi.fn()}
        mode="popover"
        position={{ x: 100, y: 200 }}
      />
    );

    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--popover')).toBeInTheDocument()
    );
    const picker = document.body.querySelector('.emoji-picker') as HTMLElement;
    await waitFor(() => expect(picker.style.visibility).toBe('visible'));
    expect(picker.style.top).toBe('192px');
  });

  it('anchored mode (composer, anchorCenterX present) applies the anchored class', async () => {
    mockPickerRect({ width: 300, height: 200 });
    vi.stubGlobal('innerWidth', 1200);
    vi.stubGlobal('innerHeight', 900);

    render(
      <EmojiPicker
        onSelect={vi.fn()}
        onClose={vi.fn()}
        mode="popover"
        position={{ x: 500, y: 800, anchorCenterX: 460 }}
      />
    );

    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--anchored')).toBeInTheDocument()
    );
  });
});

describe('EmojiPicker arrow suppression (#2370 A11)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function renderAnchored(input: {
    anchorTop: number;
    anchorRight: number;
    anchorCenterX: number;
    width: number;
    height: number;
  }) {
    mockPickerRect({ width: input.width, height: input.height });
    vi.stubGlobal('innerWidth', 1200);
    vi.stubGlobal('innerHeight', 900);
    return render(
      <EmojiPicker
        onSelect={vi.fn()}
        onClose={vi.fn()}
        mode="popover"
        position={{ x: input.anchorRight, y: input.anchorTop, anchorCenterX: input.anchorCenterX }}
      />
    );
  }

  async function waitForVisiblePicker(): Promise<HTMLElement> {
    await waitFor(() =>
      expect(document.body.querySelector('.emoji-picker--anchored')).toBeInTheDocument()
    );
    const picker = document.body.querySelector('.emoji-picker') as HTMLElement;
    await waitFor(() => expect(picker.style.visibility).toBe('visible'));
    return picker;
  }

  it('does NOT suppress the arrow in the ordinary case (positive control)', async () => {
    renderAnchored({
      anchorTop: 800,
      anchorRight: 500,
      anchorCenterX: 460,
      width: 300,
      height: 200,
    });
    const picker = await waitForVisiblePicker();
    expect(picker.className).not.toMatch(/emoji-picker--no-arrow/);
  });

  it('suppresses the arrow when the top was viewport-clamped', async () => {
    // anchorTop=50, height=200, PICKER_ANCHOR_GAP=12: naturalTop = 50-200-12
    // = -162 < VIEWPORT_GUTTER(8) -> clamped -> the picker's bottom edge is
    // no longer adjacent to the anchor.
    renderAnchored({
      anchorTop: 50,
      anchorRight: 500,
      anchorCenterX: 460,
      width: 300,
      height: 200,
    });
    const picker = await waitForVisiblePicker();
    expect(picker.className).toMatch(/emoji-picker--no-arrow/);
  });

  it('suppresses the arrow when the anchor centre falls outside the resolved picker span', async () => {
    // anchorRight=2000 pushes the picker's preferred left far past the
    // viewport, so left clamps to (viewportWidth - width - gutter); the
    // anchor's own centre (2000) then falls outside [left, left+width].
    renderAnchored({
      anchorTop: 800,
      anchorRight: 2000,
      anchorCenterX: 2000,
      width: 300,
      height: 200,
    });
    const picker = await waitForVisiblePicker();
    expect(picker.className).toMatch(/emoji-picker--no-arrow/);
  });

  it('suppression changes nothing but the arrow modifier -- no DOM reorder, no tabindex change, no other class or inline-style change', async () => {
    const shown = renderAnchored({
      anchorTop: 800,
      anchorRight: 500,
      anchorCenterX: 460,
      width: 300,
      height: 200,
    });
    const shownPicker = await waitForVisiblePicker();
    expect(shownPicker.className).not.toMatch(/emoji-picker--no-arrow/);
    const shownHtml = normalizedHtml(shownPicker);
    shown.unmount();

    const suppressed = renderAnchored({
      anchorTop: 50, // same X geometry, only the top clamp differs
      anchorRight: 500,
      anchorCenterX: 460,
      width: 300,
      height: 200,
    });
    const suppressedPicker = await waitForVisiblePicker();
    expect(suppressedPicker.className).toMatch(/emoji-picker--no-arrow/);
    const suppressedHtml = normalizedHtml(suppressedPicker);
    suppressed.unmount();

    expect(suppressedHtml).toBe(shownHtml);
  });
});

// ─── Source pin: transform-safe measurement (#2370) ───────────────────────
//
// This is the ONLY automatable guard for a defect jsdom cannot reproduce.
//
// `getBoundingClientRect()` returns the TRANSFORMED box. Both pickers place
// themselves from a measurement taken in a `useLayoutEffect` at mount -- and
// `.emoji-picker--popover` carries `animation: emojiPickerIn`, whose `from`
// keyframe is `scale(0.95) translateY(4px)`. Measuring the rect there yielded
// 95% of the real height, so `top = anchorTop - measuredHeight - GAP` placed
// the picker ~20px too low and its caret landed ON the button it points at.
// `offsetWidth`/`offsetHeight` are layout values and ignore transforms.
//
// jsdom runs no animations and applies no transforms, so a BEHAVIOURAL test
// here would pass either way -- exactly the shape `[internal]rules/tests.md`
// calls out. Pinning the source is what actually holds.
const readSrc = (rel: string) =>
  readFileSync(resolve(__dirname, '../../../../src/renderer/' + rel), 'utf8');

describe('picker placement measures transform-safe dimensions (#2370)', () => {
  /** The anchored branch only -- the legacy branch legitimately still uses the rect. */
  const anchoredBranch = (src: string) => {
    const i = src.indexOf("typeof position.anchorCenterX === 'number'");
    expect(i).toBeGreaterThan(-1);
    return src.slice(i, i + 1200);
  };

  it('EmojiPicker ANCHORED branch reads offset*, never the rect', () => {
    const branch = anchoredBranch(readSrc('components/EmojiPicker/EmojiPicker.tsx'));
    expect(branch).toMatch(/measuredWidth:\s*el\.offsetWidth/);
    expect(branch).toMatch(/measuredHeight:\s*el\.offsetHeight/);
    expect(branch).not.toMatch(/measured(Width|Height):\s*rect\./);
  });

  it('EmojiPicker LEGACY branch still uses the rect (A10 byte-identical)', () => {
    // The seven non-composer consumers must behave exactly as they did before
    // #2370. Changing their measurement would be a behaviour change however
    // well-intentioned, so it is recorded as a residual rather than made here.
    expect(readSrc('components/EmojiPicker/EmojiPicker.tsx')).toMatch(
      /const rect = pickerRef\.current\.getBoundingClientRect\(\)/
    );
  });

  it('GifPicker reads offset*, never the rect', () => {
    const src = readSrc('components/GifPicker/GifPicker.tsx');
    expect(src).toMatch(/measuredWidth:\s*el\.offsetWidth/);
    expect(src).toMatch(/measuredHeight:\s*el\.offsetHeight/);
    expect(src).not.toMatch(/measured(Width|Height):\s*rect\./);
  });
});
