import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';

// Hook surface intentionally narrow: only the readers + actions the
// in-tree consumer (ExpandCollapseAllButton) needs. Earlier drafts
// exposed `anyExpanded`, `expandAll`, and `collapseAll` for "future
// callers" (keyboard shortcuts, explicit dev tooling), but YAGNI
// removed them when no second caller materialised — if a future
// surface needs the granular actions, add them back with a real
// caller in the same PR.

/**
 * Aggregate state of all `<details.settings-collapsible>` elements inside a
 * container, plus actions to bulk-toggle them. Powers the Expand/Collapse
 * All button in `SettingsPage` (closes #297).
 *
 * Why DOM-walk rather than React-controlled state?
 * `CollapsibleSection` uses native `<details>`/`<summary>` and stores its
 * open state on the DOM element (via the `[open]` attribute), not in React.
 * Refactoring it to controlled state would touch all 9 call sites and the
 * existing tests for those call sites. Walking the DOM keeps the change
 * surgical and consistent with the existing pattern at SettingsPage.tsx
 * around the sidebar nav, which already does `el.open = true` on
 * `<details>` elements to auto-expand on navigation.
 *
 * Aggregate state is kept in sync via a MutationObserver watching for
 * `[open]` attribute changes on every matching child of the container.
 * The observer also fires when sections mount/unmount (e.g., on a settings
 * tab switch), so the toggle correctly hides itself when navigating to a
 * panel that has no collapsibles.
 */
export interface UseExpandCollapseAllResult {
  /** Every section in the container is currently expanded. */
  allExpanded: boolean;
  /** True if the container has at least one matching section. */
  hasSections: boolean;
  /**
   * Bulk action: collapse if every section is currently expanded; otherwise
   * expand (mixed-state default per the #297 spec). This is the action the
   * single-button UI binds to.
   */
  toggle: () => void;
}

const SECTION_SELECTOR = 'details.settings-collapsible';

function scanSections(container: HTMLElement | null): HTMLDetailsElement[] {
  if (!container) return [];
  return [...container.querySelectorAll<HTMLDetailsElement>(SECTION_SELECTOR)];
}

/**
 * Track and bulk-toggle every collapsible section under `containerRef`.
 *
 * The hook re-scans the container whenever:
 *   - A new `<details.settings-collapsible>` is added or removed (childList)
 *   - An existing one's `[open]` attribute changes (attributes)
 *   - The container ref changes (effect re-runs)
 *
 * Pass a stable ref to the panel-content wrapper. The hook is safe to call
 * even when the ref is null (e.g., before the panel mounts) — it will
 * report `hasSections: false` and the bulk actions are no-ops.
 */
export function useExpandCollapseAll(
  containerRef: RefObject<HTMLElement | null>
): UseExpandCollapseAllResult {
  const [allExpanded, setAllExpanded] = useState(false);
  const [hasSections, setHasSections] = useState(false);

  // Recompute aggregate state from the current DOM. Pulled into a ref so
  // the MutationObserver callback can call it without re-binding the
  // observer on every state change.
  const recomputeRef = useRef<() => void>(() => {});
  recomputeRef.current = () => {
    const sections = scanSections(containerRef.current);
    setHasSections(sections.length > 0);
    if (sections.length === 0) {
      setAllExpanded(false);
      return;
    }
    const openCount = sections.filter((s) => s.open).length;
    setAllExpanded(openCount === sections.length);
  };

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;

    // Initial scan — runs after mount when children are present.
    recomputeRef.current();

    const observer = new MutationObserver(() => {
      recomputeRef.current();
    });

    observer.observe(container, {
      childList: true,
      subtree: true,
      attributes: true,
      attributeFilter: ['open'],
    });

    return () => observer.disconnect();
  }, [containerRef]);

  const toggle = useCallback(() => {
    // Spec (#297): mixed state → default to expand all. So we only collapse
    // when EVERY section is currently expanded; in every other case we
    // expand. Reading `allExpanded` from state would risk staleness across
    // rapid clicks, so we re-derive from the DOM at click time.
    const sections = scanSections(containerRef.current);
    if (sections.length === 0) return;
    const everyOpen = sections.every((s) => s.open);
    for (const section of sections) {
      section.open = !everyOpen;
    }
  }, [containerRef]);

  return { allExpanded, hasSections, toggle };
}
