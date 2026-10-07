import { createStore } from '../../utils/runtime/createStore';

/**
 * Settings Collapsible Store (#2365)
 *
 * Remembers, for the life of the renderer, which Settings `CollapsibleSection`s the
 * user has expanded. Every section is collapsed on first visit; a section the user
 * expands — or that a script-driven opener expands for them (Expand all, the sub-nav
 * scroll, a focus request, the update indicator) — stays expanded across pane switches
 * and Settings close/reopen, because `CollapsibleSection` reads its `open` state from
 * here and writes every change back from the native `toggle` event.
 *
 * Deliberately in-memory. The reporter asked for "at least the current session", and
 * `persist` is reserved for state that must survive an app restart (frontend.md
 * § State Management), so a renderer reload starts collapsed again. Nothing resets this
 * store in production: section layout is UI chrome, not account data, so it stays out
 * of `resetService`; tests reset it through `getInitialState()`.
 *
 * Keys are `CollapsibleSection` ids — one global namespace across every surface that
 * renders one (today, only the app Settings panes). Two sections sharing an id would
 * share memory; `CollapsibleSection.test.tsx` pins that every id is a unique literal.
 */
export interface SettingsCollapsibleState {
  /** Section id → last observed open state this renderer session. Absent ⇒ collapsed. */
  openSections: Readonly<Record<string, boolean>>;
  /**
   * Record a section's open state. A write that matches the current state returns the
   * same state object, so Zustand skips notifying subscribers — closing a section the
   * store has never seen is a silent no-op, not a re-render of that section.
   */
  setSectionOpen: (id: string, open: boolean) => void;
}

export const useSettingsCollapsibleStore = createStore<SettingsCollapsibleState>()((set) => ({
  openSections: {},
  setSectionOpen: (id, open) =>
    set((state) =>
      // `=== true` rather than `?? false`: an absent entry behaves exactly like `false`,
      // and an inherited Object.prototype key can never read as open.
      (state.openSections[id] === true) === open
        ? state
        : { openSections: { ...state.openSections, [id]: open } }
    ),
}));
