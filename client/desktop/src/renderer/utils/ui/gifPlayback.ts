// Pure GIF-playback resolver — the single source of truth for "does this GIF
// animate right now, and which element carries it". No DOM, no store imports,
// no React: unit-testable in isolation, and total over every input combination.
// See [internal]specs/2026-09-12-2369-gif-playback-gating-design.md §2.1.
//
// THE TWO-AXIS MODEL, which is the whole reason this file exists (§0 of the spec):
//
//   The hover gate chooses the ELEMENT.
//   Focus chooses whether the mounted animated element RUNS.
//
// They are two axes, not one. Collapsing them is what makes a naive
// implementation either lose `HTMLMediaElement.pause()` (by unmounting the
// <video> it could have paused) or start fetching mp4 bytes for Reduce-Animations
// users who today fetch none. `GifEmbed` evaluates its still branch BEFORE its
// `animatedKind === 'video'` branch, so under Reduce Animations without hover the
// <video> is never mounted to be paused — which is why `surface` and `playing`
// are separate fields rather than one boolean.

/**
 * `'auto'` is the "no explicit pick" sentinel, mirroring `AppFontId`'s
 * `'default'` in `effectiveFont.ts`. It is a real stored value, not `undefined`:
 * that is what lets Reduce Animations change the RESOLVED behaviour without ever
 * writing this field — see `REDUCE_MOTION_WRITES_GIF_PLAYBACK`.
 */
export type GifPlaybackMode = 'auto' | 'always' | 'hover';

/**
 * Reduce Animations NEVER writes `gifPlayback`. The follow is derived on every
 * read, so turning Reduce Animations off restores the user's prior behaviour
 * STRUCTURALLY — by re-deriving it — rather than by replaying a snapshot some
 * handler had to remember to take.
 *
 * This is the half of the `resolveEffectiveFont` precedent that transfers
 * (`[internal]rules/frontend.md` § Application font, `DYSLEXIC_WRITES_APPFONT`).
 * The other half — the hard lock — deliberately does NOT: Dyslexic Support is a
 * declared accessibility requirement whose value is being undefeatable, whereas
 * `reduceAnimations` is a broad appearance preference that is not even wired to
 * `prefers-reduced-motion`. A hard lock here would delete the feature in the
 * configuration the reporter most likely runs, and could not express
 * "Reduce Animations off, hover-only on" — the reporter's own case.
 */
export const REDUCE_MOTION_WRITES_GIF_PLAYBACK = false;

/** The resolved gate. `'always'` and `'hover'` are the only reachable values. */
export type GifPlaybackGate = 'always' | 'hover';

export interface GifPlaybackVerdict {
  /**
   * Which element to render. `'still'` means the animated element is not mounted
   * at all, so there is nothing to pause and (for the video kind) no mp4 is
   * requested.
   */
  surface: 'animated' | 'still';
  /** Whether the animated element, if mounted, should be running. */
  playing: boolean;
  /**
   * The resolved gate. Exported on the verdict rather than recomputed by each
   * consumer: components need it to decide whether to attach hover handlers at
   * all, and a second copy of the `'auto'` rule at every call site is exactly
   * the duplication this resolver exists to remove.
   */
  gate: GifPlaybackGate;
}

export interface GifPlaybackInput {
  mode: GifPlaybackMode;
  reduceAnimations: boolean;
  hovering: boolean;
  windowFocused: boolean;
}

/**
 * Total over `{auto,always,hover} × {RM on,off} × {focused,unfocused} × {hovering,not}`
 * — all 24 combinations, pinned by a table-driven test (spec A7).
 */
export function resolveGifPlayback(input: GifPlaybackInput): GifPlaybackVerdict {
  const { mode, reduceAnimations, hovering, windowFocused } = input;

  // 1. Resolve the sentinel. Only `'auto'` consults Reduce Animations; an
  //    explicit pick is honoured in both directions, which is what makes
  //    "Reduce Animations on, GIFs always play" expressible.
  const followed: GifPlaybackGate = reduceAnimations ? 'hover' : 'always';
  const gate: GifPlaybackGate = mode === 'auto' ? followed : mode;

  // 2. The gate chooses the ELEMENT.
  const surface: 'animated' | 'still' = gate === 'hover' && !hovering ? 'still' : 'animated';

  // 3. Focus chooses whether that element RUNS. A still frame is already
  //    motionless, so `playing` is meaningless there and reports false.
  const playing = surface === 'animated' && windowFocused;

  return { surface, playing, gate };
}

/**
 * The Settings hint sentence. It describes the RESOLVED behaviour rather than
 * the enum, which is what keeps `'auto'` and `'always'` legible while Reduce
 * Animations is off — they are behaviourally identical in that configuration
 * and the enum alone cannot say so (spec §5 residual 6).
 *
 * `hovering` is deliberately absent from the input: the hint describes the
 * setting, not the pointer's current position.
 */
export function describeGifPlayback(input: {
  mode: GifPlaybackMode;
  reduceAnimations: boolean;
  windowFocused: boolean;
}): string {
  const { mode, reduceAnimations, windowFocused } = input;

  let base: string;
  if (mode === 'auto') {
    base = reduceAnimations
      ? 'Following Reduce Animations: GIFs play on hover only.'
      : 'Following Reduce Animations: GIFs play automatically.';
  } else if (mode === 'always') {
    base = 'GIFs always play, even with Reduce Animations on.';
  } else {
    base = 'GIFs only play while you hover over them.';
  }

  // Appended, never substituted: the unfocused pause is a transient state on top
  // of the setting, and replacing the sentence would hide what the setting is.
  return windowFocused ? base : `${base} Currently paused because Concord isn't focused.`;
}
