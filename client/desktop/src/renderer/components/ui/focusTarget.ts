import { getFocusable } from './Modal';

/**
 * Whether a `.focus()` on `el` would take: not disabled (itself, or through a
 * disabled fieldset), not inside an `inert` subtree, and rendered. A call that
 * does not take is silent, so a caller whose `focusFallback` names such an
 * element leaves focus on `<body>`.
 *
 * `getFocusable` filters only `[disabled]` and `aria-hidden`. The layout half
 * is the engine's own answer (`checkVisibility`: `display: none` anywhere above
 * the element, `visibility: hidden`, `content-visibility`). jsdom has no layout
 * and no such method, so there an element counts as rendered, as it already
 * does in `getFocusable`.
 */
function canTakeFocus(el: HTMLElement): boolean {
  if (el.matches(':disabled') || el.closest('[inert]') !== null) return false;
  return typeof el.checkVisibility === 'function'
    ? el.checkVisibility({ checkVisibilityCSS: true })
    : true;
}

/**
 * Where focus lands in `region` once the control that opened a dialog is gone:
 * the region itself when it takes focus (a tree with `tabindex="0"`), else its
 * first focusable descendant that can take it now. Null when the region is
 * absent, or nothing in it can take focus, so a caller's `focusFallback` never
 * names an element that has left the document or that `.focus()` would skip.
 */
export function focusTargetIn(region: HTMLElement | null): HTMLElement | null {
  if (!region?.isConnected) return null;
  if (region.tabIndex >= 0 && canTakeFocus(region)) return region;
  return getFocusable(region).find(canTakeFocus) ?? null;
}
