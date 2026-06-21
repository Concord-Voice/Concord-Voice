import { useKeyboardShortcutStore } from '@/renderer/stores/keyboardShortcutStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('keyboardShortcutStore', () => {
  beforeEach(() => {
    resetAllStores();
    useKeyboardShortcutStore.setState({
      overlayOpen: false,
      channelSwitcherOpen: false,
    });
  });

  describe('default shortcuts', () => {
    it('has all default shortcuts defined', () => {
      const { shortcuts } = useKeyboardShortcutStore.getState();
      expect(shortcuts).toHaveLength(11);
    });

    it('all shortcuts have required fields', () => {
      const { shortcuts } = useKeyboardShortcutStore.getState();
      for (const shortcut of shortcuts) {
        expect(shortcut).toHaveProperty('id');
        expect(shortcut).toHaveProperty('label');
        expect(shortcut).toHaveProperty('category');
        expect(shortcut).toHaveProperty('defaultCombo');
        expect(shortcut).toHaveProperty('combo');
        expect(typeof shortcut.id).toBe('string');
        expect(typeof shortcut.label).toBe('string');
        expect(typeof shortcut.defaultCombo.key).toBe('string');
        expect(typeof shortcut.combo.key).toBe('string');
      }
    });

    it('all categories are represented', () => {
      const { shortcuts } = useKeyboardShortcutStore.getState();
      const categories = new Set(shortcuts.map((s) => s.category));
      expect(categories).toContain('navigation');
      expect(categories).toContain('messaging');
      expect(categories).toContain('app');
      expect(categories).toContain('voice');
    });

    it('allowInInput shortcuts include channel-switcher and shortcut-overlay', () => {
      const { shortcuts } = useKeyboardShortcutStore.getState();
      const allowInInputIds = shortcuts.filter((s) => s.allowInInput).map((s) => s.id);
      expect(allowInInputIds).toContain('channel-switcher');
      expect(allowInInputIds).toContain('shortcut-overlay');
    });
  });

  describe('overlay', () => {
    it('openOverlay sets overlayOpen to true', () => {
      useKeyboardShortcutStore.getState().openOverlay();
      expect(useKeyboardShortcutStore.getState().overlayOpen).toBe(true);
    });

    it('closeOverlay sets overlayOpen to false', () => {
      useKeyboardShortcutStore.setState({ overlayOpen: true });
      useKeyboardShortcutStore.getState().closeOverlay();
      expect(useKeyboardShortcutStore.getState().overlayOpen).toBe(false);
    });

    it('toggleOverlay toggles overlayOpen', () => {
      expect(useKeyboardShortcutStore.getState().overlayOpen).toBe(false);
      useKeyboardShortcutStore.getState().toggleOverlay();
      expect(useKeyboardShortcutStore.getState().overlayOpen).toBe(true);
      useKeyboardShortcutStore.getState().toggleOverlay();
      expect(useKeyboardShortcutStore.getState().overlayOpen).toBe(false);
    });
  });

  describe('channel switcher', () => {
    it('openChannelSwitcher sets channelSwitcherOpen to true', () => {
      useKeyboardShortcutStore.getState().openChannelSwitcher();
      expect(useKeyboardShortcutStore.getState().channelSwitcherOpen).toBe(true);
    });

    it('closeChannelSwitcher sets channelSwitcherOpen to false', () => {
      useKeyboardShortcutStore.setState({ channelSwitcherOpen: true });
      useKeyboardShortcutStore.getState().closeChannelSwitcher();
      expect(useKeyboardShortcutStore.getState().channelSwitcherOpen).toBe(false);
    });
  });

  describe('getShortcut', () => {
    it('returns correct definition by id', () => {
      const shortcut = useKeyboardShortcutStore.getState().getShortcut('toggle-mute');
      expect(shortcut).toBeDefined();
      expect(shortcut!.id).toBe('toggle-mute');
      expect(shortcut!.label).toBe('Toggle Mute');
      expect(shortcut!.category).toBe('voice');
      expect(shortcut!.combo.key).toBe('m');
      expect(shortcut!.combo.ctrl).toBe(true);
      expect(shortcut!.combo.shift).toBe(true);
    });

    it('returns undefined for unknown id', () => {
      const shortcut = useKeyboardShortcutStore.getState().getShortcut('nonexistent');
      expect(shortcut).toBeUndefined();
    });
  });
});
