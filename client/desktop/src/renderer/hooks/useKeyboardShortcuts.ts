import { useEffect } from 'react';
import { keyboardShortcutService } from '../services/keyboardShortcutService';
import { useSettingsOverlayStore } from '../stores/settingsOverlayStore';
import { useKeyboardShortcutStore } from '../stores/keyboardShortcutStore';
import { useChannelStore } from '../stores/channelStore';
import { useUnreadStore } from '../stores/unreadStore';
import { useVoiceStore } from '../stores/voiceStore';

/**
 * Registers all global keyboard shortcut handlers.
 * Should be called once at the app's top level (MainView).
 */
export function useKeyboardShortcuts(): void {
  useEffect(() => {
    const service = keyboardShortcutService;
    service.init();

    // Navigation
    service.registerHandler('channel-switcher', () => {
      useKeyboardShortcutStore.getState().openChannelSwitcher();
    });

    service.registerHandler('nav-channel-up', () => {
      const { channels, activeChannelId, setActiveChannel } = useChannelStore.getState();
      const textChannels = channels.filter((c) => c.type === 'text');
      const idx = textChannels.findIndex((c) => c.id === activeChannelId);
      if (idx > 0) setActiveChannel(textChannels[idx - 1].id);
    });

    service.registerHandler('nav-channel-down', () => {
      const { channels, activeChannelId, setActiveChannel } = useChannelStore.getState();
      const textChannels = channels.filter((c) => c.type === 'text');
      const idx = textChannels.findIndex((c) => c.id === activeChannelId);
      if (idx >= 0 && idx < textChannels.length - 1) setActiveChannel(textChannels[idx + 1].id);
    });

    service.registerHandler('nav-unread-up', () => {
      const { channels, activeChannelId, setActiveChannel } = useChannelStore.getState();
      const unreadCounts = useUnreadStore.getState().unreadCounts;
      const textChannels = channels.filter((c) => c.type === 'text');
      const idx = textChannels.findIndex((c) => c.id === activeChannelId);
      // Search backwards for next unread
      for (let i = idx - 1; i >= 0; i--) {
        if ((unreadCounts.get(textChannels[i].id) || 0) > 0) {
          setActiveChannel(textChannels[i].id);
          return;
        }
      }
      // Wrap around from end
      for (let i = textChannels.length - 1; i > idx; i--) {
        if ((unreadCounts.get(textChannels[i].id) || 0) > 0) {
          setActiveChannel(textChannels[i].id);
          return;
        }
      }
    });

    service.registerHandler('nav-unread-down', () => {
      const { channels, activeChannelId, setActiveChannel } = useChannelStore.getState();
      const unreadCounts = useUnreadStore.getState().unreadCounts;
      const textChannels = channels.filter((c) => c.type === 'text');
      const idx = textChannels.findIndex((c) => c.id === activeChannelId);
      // Search forward for next unread
      for (let i = idx + 1; i < textChannels.length; i++) {
        if ((unreadCounts.get(textChannels[i].id) || 0) > 0) {
          setActiveChannel(textChannels[i].id);
          return;
        }
      }
      // Wrap around from start
      for (let i = 0; i < idx; i++) {
        if ((unreadCounts.get(textChannels[i].id) || 0) > 0) {
          setActiveChannel(textChannels[i].id);
          return;
        }
      }
    });

    service.registerHandler('close-modal', () => {
      const store = useKeyboardShortcutStore.getState();
      if (store.overlayOpen) store.closeOverlay();
      if (store.channelSwitcherOpen) store.closeChannelSwitcher();
    });

    // Messaging
    service.registerHandler('search', () => {
      globalThis.dispatchEvent(new CustomEvent('concord:toggle-search'));
    });

    // App
    service.registerHandler('shortcut-overlay', () => {
      useKeyboardShortcutStore.getState().toggleOverlay();
    });

    service.registerHandler('open-settings', () => {
      useSettingsOverlayStore.getState().openSettings('app');
    });

    // Voice
    service.registerHandler('toggle-mute', () => {
      const vs = useVoiceStore.getState();
      if (vs.activeChannelId) vs.setMuted(!vs.isMuted);
    });

    service.registerHandler('toggle-deafen', () => {
      const vs = useVoiceStore.getState();
      if (vs.activeChannelId) vs.setDeafened(!vs.isDeafened);
    });

    return () => {
      service.destroy();
    };
  }, []);
}
