import { useNotificationStore } from '../stores/notificationStore';

export type NotificationType = 'dm' | 'mention' | 'message';

interface NotifyOptions {
  title: string;
  body: string;
  targetType: 'channel' | 'dm';
  targetId: string;
  serverId?: string;
  senderId: string;
}

interface ShouldNotifyOptions {
  type: NotificationType;
  isWindowFocused: boolean;
  isActiveChannel: boolean;
}

class DesktopNotificationService {
  private badgeCount = 0;

  /**
   * Check if a notification should fire based on all settings and conditions.
   */
  shouldNotify(options: ShouldNotifyOptions): boolean {
    const state = useNotificationStore.getState();

    // Master toggle
    if (!state.desktopNotificationsEnabled) return false;

    // Don't notify if window is focused AND viewing the active channel (when enabled)
    if (state.suppressWhenFocused && options.isWindowFocused && options.isActiveChannel)
      return false;

    // DND
    if (state.doNotDisturb) return false;

    // Quiet hours
    if (state.quietHoursEnabled && this.isInQuietHours(state.quietHoursStart, state.quietHoursEnd))
      return false;

    // Per-type toggles
    switch (options.type) {
      case 'dm':
        return state.desktopNotifyDMs;
      case 'mention':
        return state.desktopNotifyMentions;
      case 'message':
        return state.desktopNotifyAllMessages;
      default:
        return false;
    }
  }

  /**
   * Show a desktop notification.
   */
  notify(options: NotifyOptions): void {
    // Determine body content
    let body = options.body;
    if (!body) {
      body = 'New encrypted message';
    }
    // Truncate body to 100 chars
    if (body.length > 100) {
      body = body.slice(0, 97) + '...';
    }

    try {
      const notification = new Notification(options.title, {
        body: body || 'New message',
        silent: true, // We handle sounds separately via notificationSoundService
      });

      notification.onclick = () => {
        this.handleClick(options.targetType, options.targetId, options.serverId);
      };

      // Flash the taskbar/dock to draw attention
      globalThis.electron?.flashFrame?.(true);
    } catch {
      // Notification API not available
    }
  }

  /**
   * Handle notification click — focus window and navigate to target.
   */
  private handleClick(targetType: 'channel' | 'dm', targetId: string, serverId?: string): void {
    // Import dynamically to avoid circular deps
    import('../stores/notificationNavigationStore').then(({ useNotificationNavigationStore }) => {
      useNotificationNavigationStore.getState().setPendingNavigation({
        type: targetType,
        targetId,
        serverId,
      });
    });

    // Focus the app window
    globalThis.electron?.focusWindow?.();
  }

  /**
   * Check if current time falls within quiet hours.
   * Handles midnight wrap (e.g., 22:00 - 08:00).
   */
  isInQuietHours(start: string, end: string): boolean {
    const now = new Date();
    const currentMinutes = now.getHours() * 60 + now.getMinutes();

    const [startH, startM] = start.split(':').map(Number);
    const [endH, endM] = end.split(':').map(Number);
    const startMinutes = startH * 60 + startM;
    const endMinutes = endH * 60 + endM;

    if (startMinutes <= endMinutes) {
      // Same-day range (e.g., 08:00 - 22:00)
      return currentMinutes >= startMinutes && currentMinutes < endMinutes;
    } else {
      // Overnight range (e.g., 22:00 - 08:00)
      return currentMinutes >= startMinutes || currentMinutes < endMinutes;
    }
  }

  /**
   * Increment the dock/taskbar badge count.
   */
  incrementBadge(): void {
    this.badgeCount++;
    globalThis.electron?.setBadgeCount?.(this.badgeCount);
  }

  /**
   * Clear the dock/taskbar badge.
   */
  clearBadge(): void {
    this.badgeCount = 0;
    globalThis.electron?.setBadgeCount?.(0);
  }

  /**
   * Get the current badge count.
   */
  getBadgeCount(): number {
    return this.badgeCount;
  }
}

export const desktopNotificationService = new DesktopNotificationService();
