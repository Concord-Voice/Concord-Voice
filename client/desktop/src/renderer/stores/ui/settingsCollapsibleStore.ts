import { createStore } from '../../utils/runtime/createStore';

/**
 * Settings Collapsible Store (#2365)
 *
 * Remembers main and nested disclosure expansion throughout a Settings visit and
 * for 30 minutes after leaving. Real overlay transitions call beginVisit/endVisit;
 * pane switches and React effect replay do not end a visit. Native toggle events
 * capture clicks, bulk actions, and intentional navigation through the same binding.
 *
 * Deliberately in-memory: a renderer reload starts collapsed. An absolute close
 * deadline is checked again on entry because sleeping or throttled renderers can
 * delay the purge timer. Returning before expiry cancels the timer; an active
 * visit never expires. This is layout memory, not account or draft persistence.
 *
 * Keys are `CollapsibleSection` ids — one global namespace across every surface that
 * renders one (today, only the app Settings panes). Two sections sharing an id would
 * share memory; `CollapsibleSection.test.tsx` pins that every id is a unique literal.
 */
export interface SettingsCollapsibleState {
  /** Section id → last observed open state this renderer session. Absent ⇒ collapsed. */
  openSections: Readonly<Record<string, boolean>>;
  retentionDeadline: number | null;
  visitActive: boolean;
  beginVisit: () => void;
  endVisit: () => void;
  /**
   * Record a section's open state. A write that matches the current state returns the
   * same state object, so Zustand skips notifying subscribers — closing a section the
   * store has never seen is a silent no-op, not a re-render of that section.
   */
  setSectionOpen: (id: string, open: boolean) => void;
}

export const SETTINGS_EXPANSION_RETENTION_MS = 30 * 60 * 1000;

let retentionTimer: ReturnType<typeof setTimeout> | null = null;

export const useSettingsCollapsibleStore = createStore<SettingsCollapsibleState>()((set, get) => ({
  openSections: {},
  retentionDeadline: null,
  visitActive: false,
  beginVisit: () => {
    if (retentionTimer !== null) clearTimeout(retentionTimer);
    retentionTimer = null;
    set((state) => {
      if (state.visitActive) return state;
      const expired = state.retentionDeadline !== null && Date.now() >= state.retentionDeadline;
      return {
        visitActive: true,
        retentionDeadline: null,
        openSections: expired ? {} : state.openSections,
      };
    });
  },
  endVisit: () => {
    if (!get().visitActive) return;
    if (retentionTimer !== null) clearTimeout(retentionTimer);
    const deadline = Date.now() + SETTINGS_EXPANSION_RETENTION_MS;
    set({ visitActive: false, retentionDeadline: deadline });
    retentionTimer = setTimeout(() => {
      retentionTimer = null;
      set((state) =>
        !state.visitActive && state.retentionDeadline === deadline && Date.now() >= deadline
          ? { openSections: {}, retentionDeadline: null }
          : state
      );
    }, SETTINGS_EXPANSION_RETENTION_MS);
  },
  setSectionOpen: (id, open) =>
    set((state) =>
      // `=== true` rather than `?? false`: an absent entry behaves exactly like `false`,
      // and an inherited Object.prototype key can never read as open.
      (state.openSections[id] === true) === open
        ? state
        : { openSections: { ...state.openSections, [id]: open } }
    ),
}));
