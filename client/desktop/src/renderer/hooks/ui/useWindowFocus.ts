// Window-focus primitive — one listener set and one snapshot for the whole
// renderer, however many components subscribe.
// See [internal]specs/2026-09-12-2369-gif-playback-gating-design.md §2.2.
//
// WHY NOT `visibilitychange` ALONE: it does not fire when a VISIBLE window loses
// focus, which is the reporter's entire case (#2369) — alt-tabbing to another app
// leaves Concord fully visible and fully unfocused. It is subscribed here as well,
// because it is the signal that catches minimise and occlusion, which `blur` alone
// does not always deliver.
//
// WHY NOT MAIN-PROCESS IPC: `browser-window-blur` would cost an
// IPC_CONTRACT_VERSION bump plus a preload surface for a signal the renderer
// already has — and because the renderer ships to Cloudflare Pages while the
// shell ships on the electron-updater train, every installed shell below the new
// contract would get the feature DARK until it updated.

import { useSyncExternalStore } from 'react';

/** Subscribers. Size is also the "is the cache being maintained?" signal below. */
const listeners = new Set<() => void>();

/** Last computed value. `null` means "never computed", not "unfocused". */
let cached: boolean | null = null;

function compute(): boolean {
  // Total by construction: a non-DOM context (a bare unit test importing this
  // module) reports focused, which is the non-pausing answer — a missing DOM
  // must never silently freeze every GIF in the app.
  if (typeof document === 'undefined') return true;
  // Both halves matter: `hasFocus()` is false when another app is in front,
  // `hidden` is true when the window is minimised or the tab is occluded.
  return document.hasFocus() && !document.hidden;
}

function handleChange(): void {
  const next = compute();
  if (next === cached) return; // No spurious notifications: N attachments, N re-renders.
  cached = next;
  for (const notify of listeners) notify();
}

function subscribe(onStoreChange: () => void): () => void {
  if (listeners.size === 0) {
    // Re-seed on the first subscription. The cache is only maintained while
    // something is listening, so a remount after a fully-unsubscribed period
    // must not trust whatever value was left behind.
    cached = compute();
    // BUBBLE PHASE — load-bearing, and the single easiest thing to get wrong here.
    // `focus`/`blur` do not bubble but they DO capture, so a capture-phase
    // listener on the window sees the focus event of every inner element and
    // would pause every GIF in the app the moment the user tabs to a button.
    // The default (bubble) phase only sees events targeting the window itself,
    // which is exactly the OS-level focus change we want. Matches the shape
    // already used at components/Voice/VoiceView.tsx:309-312. Pinned by a test
    // that dispatches `focus` on a button and asserts nothing changed (spec A4).
    globalThis.addEventListener('focus', handleChange);
    globalThis.addEventListener('blur', handleChange);
    document.addEventListener('visibilitychange', handleChange);
  }
  listeners.add(onStoreChange);

  return () => {
    listeners.delete(onStoreChange);
    if (listeners.size === 0) {
      globalThis.removeEventListener('focus', handleChange);
      globalThis.removeEventListener('blur', handleChange);
      document.removeEventListener('visibilitychange', handleChange);
    }
  };
}

/**
 * Imperative point-in-time read, for callers that want a value rather than a
 * subscription — the shape `useWebSocketMessages` and `notificationSoundService`
 * use today via bare `document.hasFocus()`. They are deliberately NOT migrated
 * here: this PR does not touch those files, and the boy-scout rule does not
 * reach files a PR does not modify.
 *
 * Doubles as `useSyncExternalStore`'s `getSnapshot`. While something is
 * subscribed it returns the cached snapshot, which is what keeps the snapshot
 * stable between notifications; with no subscriber there is no listener keeping
 * the cache honest, so it reads through to the DOM.
 */
export function readWindowFocus(): boolean {
  if (listeners.size === 0 || cached === null) cached = compute();
  return cached;
}

/**
 * A caller that passes `enabled: false` registers NO listener and never
 * re-renders on a focus change — it is not merely ignoring the value. Both
 * functions below are module constants rather than inline closures, because
 * `useSyncExternalStore` re-subscribes whenever `subscribe`'s identity changes;
 * a fresh closure per render would resubscribe on every render.
 *
 * Exists because every `ImageAttachment` subscribed, including ordinary JPEGs
 * and PNGs whose rendering ignores the verdict entirely, so a photo-heavy
 * unvirtualized channel re-rendered every mounted image on every alt-tab.
 * Found by Codex review on PR #3291.
 */
const noopSubscribe = (): (() => void) => () => undefined;
const alwaysFocused = (): boolean => true;

/** `true` while the Concord window is focused AND visible. */
export function useWindowFocus(enabled = true): boolean {
  return useSyncExternalStore(
    enabled ? subscribe : noopSubscribe,
    enabled ? readWindowFocus : alwaysFocused
  );
}

/**
 * Test-only reset. The module singleton outlives a single test's React tree, so
 * a suite that mounts, unmounts and re-reads would otherwise inherit the
 * previous test's cache. Not exported through any barrel; production never
 * calls it.
 */
export function __resetWindowFocusForTests(): void {
  listeners.clear();
  cached = null;
  if (typeof document !== 'undefined') {
    globalThis.removeEventListener('focus', handleChange);
    globalThis.removeEventListener('blur', handleChange);
    document.removeEventListener('visibilitychange', handleChange);
  }
}
