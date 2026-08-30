import { useEffect } from 'react';
import { keyboardShortcutService } from '../services/keyboardShortcutService';
import { useSettingsOverlayStore } from '../stores/ui/settingsOverlayStore';
import { useKeyboardShortcutStore } from '../stores/ui/keyboardShortcutStore';
import { useChannelStore } from '../stores/chat/channelStore';
import { useUnreadStore } from '../stores/chat/unreadStore';
import { useVoiceStore } from '../stores/voice/voiceStore';
import { isChannelMuted } from '../stores/ui/notificationPrefsStore';
import type { Channel } from '../types/chat';

/**
 * Prev/Next Unread must honor mutes. A muted channel keeps its count in the
 * unread store (so un-muting reveals the badge without a refetch) but renders
 * no unread indicator, so unread navigation must skip it, mirroring the
 * visibility gate ChannelList applies to badges (#84 / epic #1029 close audit,
 * P2 review follow-up). Without this, Prev/Next Unread can jump to a muted
 * channel that shows nothing.
 */
function channelHasVisibleUnread(channel: Channel, unreadCounts: Map<string, number>): boolean {
  return (unreadCounts.get(channel.id) || 0) > 0 && !isChannelMuted(channel.id, channel.server_id);
}

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
        if (channelHasVisibleUnread(textChannels[i], unreadCounts)) {
          setActiveChannel(textChannels[i].id);
          return;
        }
      }
      // Wrap around from end
      for (let i = textChannels.length - 1; i > idx; i--) {
        if (channelHasVisibleUnread(textChannels[i], unreadCounts)) {
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
        if (channelHasVisibleUnread(textChannels[i], unreadCounts)) {
          setActiveChannel(textChannels[i].id);
          return;
        }
      }
      // Wrap around from start
      for (let i = 0; i < idx; i++) {
        if (channelHasVisibleUnread(textChannels[i], unreadCounts)) {
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
