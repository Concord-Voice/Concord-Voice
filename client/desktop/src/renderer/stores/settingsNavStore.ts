import { create } from 'zustand';

/**
 * The Settings left-nav sections. Defined here (not in SettingsPage) so the nav
 * store and cross-section deep-links can reference it without a circular import.
 */
export type SettingsSection =
  | 'appearance'
  | 'privacy'
  | 'account'
  | 'notifications'
  | 'voice'
  | 'accessibility'
  | 'about';

export interface SettingsFocusRequest {
  section: SettingsSection;
  controlId: string;
}

interface SettingsNavState {
  /** A pending cross-section "switch pane + focus this control" request, or null. */
  focusRequest: SettingsFocusRequest | null;
  requestFocus: (section: SettingsSection, controlId: string) => void;
  clearFocusRequest: () => void;
}

/**
 * Minimal cross-section navigation primitive (#1644). The locked Appearance font
 * picker's back-link sets a focus request; SettingsPage switches to the target
 * pane and focuses the control once it mounts. Single-purpose by design — not a
 * general settings router.
 */
export const useSettingsNavStore = create<SettingsNavState>((set) => ({
  focusRequest: null,
  requestFocus: (section, controlId) => set({ focusRequest: { section, controlId } }),
  clearFocusRequest: () => set({ focusRequest: null }),
}));
