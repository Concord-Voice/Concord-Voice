import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

export interface KeyCombo {
  key: string; // e.g. 'k', 'ArrowUp', '/', 'Escape', ','
  ctrl?: boolean; // Ctrl on Win/Linux, Cmd on macOS
  shift?: boolean;
  alt?: boolean; // Alt on Win/Linux, Option on macOS
}

export interface ShortcutDefinition {
  id: string;
  label: string;
  category: 'navigation' | 'messaging' | 'app' | 'voice';
  defaultCombo: KeyCombo;
  combo: KeyCombo; // current binding (same as defaultCombo for now)
  description?: string;
  allowInInput?: boolean; // If true, fires even in text inputs
}

const DEFAULT_SHORTCUTS: ShortcutDefinition[] = [
  // Navigation
  {
    id: 'channel-switcher',
    label: 'Quick Channel Switcher',
    category: 'navigation',
    defaultCombo: { key: 'k', ctrl: true },
    combo: { key: 'k', ctrl: true },
    allowInInput: true,
  },
  {
    id: 'nav-channel-up',
    label: 'Previous Channel',
    category: 'navigation',
    defaultCombo: { key: 'ArrowUp', alt: true },
    combo: { key: 'ArrowUp', alt: true },
  },
  {
    id: 'nav-channel-down',
    label: 'Next Channel',
    category: 'navigation',
    defaultCombo: { key: 'ArrowDown', alt: true },
    combo: { key: 'ArrowDown', alt: true },
  },
  {
    id: 'nav-unread-up',
    label: 'Previous Unread Channel',
    category: 'navigation',
    defaultCombo: { key: 'ArrowUp', alt: true, shift: true },
    combo: { key: 'ArrowUp', alt: true, shift: true },
  },
  {
    id: 'nav-unread-down',
    label: 'Next Unread Channel',
    category: 'navigation',
    defaultCombo: { key: 'ArrowDown', alt: true, shift: true },
    combo: { key: 'ArrowDown', alt: true, shift: true },
  },
  {
    id: 'close-modal',
    label: 'Close Modal/Overlay',
    category: 'navigation',
    defaultCombo: { key: 'Escape' },
    combo: { key: 'Escape' },
  },

  // Messaging
  {
    id: 'search',
    label: 'Search Messages',
    category: 'messaging',
    defaultCombo: { key: 'f', ctrl: true, shift: true },
    combo: { key: 'f', ctrl: true, shift: true },
    allowInInput: true,
  },

  // App
  {
    id: 'shortcut-overlay',
    label: 'Show Keyboard Shortcuts',
    category: 'app',
    defaultCombo: { key: '/', ctrl: true },
    combo: { key: '/', ctrl: true },
    allowInInput: true,
  },
  {
    id: 'open-settings',
    label: 'Open Settings',
    category: 'app',
    defaultCombo: { key: ',', ctrl: true },
    combo: { key: ',', ctrl: true },
    allowInInput: true,
  },

  // Voice
  {
    id: 'toggle-mute',
    label: 'Toggle Mute',
    category: 'voice',
    defaultCombo: { key: 'm', ctrl: true, shift: true },
    combo: { key: 'm', ctrl: true, shift: true },
    allowInInput: true,
  },
  {
    id: 'toggle-deafen',
    label: 'Toggle Deafen',
    category: 'voice',
    defaultCombo: { key: 'd', ctrl: true, shift: true },
    combo: { key: 'd', ctrl: true, shift: true },
    allowInInput: true,
  },
];

interface KeyboardShortcutState {
  shortcuts: ShortcutDefinition[];
  overlayOpen: boolean;
  channelSwitcherOpen: boolean;

  openOverlay: () => void;
  closeOverlay: () => void;
  toggleOverlay: () => void;
  openChannelSwitcher: () => void;
  closeChannelSwitcher: () => void;
  getShortcut: (id: string) => ShortcutDefinition | undefined;
}

export const useKeyboardShortcutStore = wrapStore(create<KeyboardShortcutState>()(
  devtools(
    (set, get) => ({
      shortcuts: DEFAULT_SHORTCUTS,
      overlayOpen: false,
      channelSwitcherOpen: false,

      openOverlay: () => set({ overlayOpen: true }),
      closeOverlay: () => set({ overlayOpen: false }),
      toggleOverlay: () => set((s) => ({ overlayOpen: !s.overlayOpen })),
      openChannelSwitcher: () => set({ channelSwitcherOpen: true }),
      closeChannelSwitcher: () => set({ channelSwitcherOpen: false }),
      getShortcut: (id) => get().shortcuts.find((s) => s.id === id),
    }),
    { name: 'KeyboardShortcutStore' }
  )
));
