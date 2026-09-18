import { useNotificationStore } from '../../stores/ui/notificationStore';
import { readWindowFocus } from '../../hooks/ui/useWindowFocus';

export type NotificationType = 'dm' | 'mention' | 'message';

interface NotifyOptions {
  title: string;
  senderDisplayName: string;
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

/**
 * Check if current time falls within quiet hours.
 * Handles midnight wrap (e.g., 22:00 - 08:00).
 * Shared with notificationSoundService so popups and chat sounds agree (#1029).
 */
export function isInQuietHours(start: string, end: string): boolean {
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

export function applyContentPrivacy(options: NotifyOptions): { title: string; body: string } {
  const mode = useNotificationStore.getState().notificationContent;

  switch (mode) {
    case 'minimal':
      return { title: 'New Message', body: '' };
    case 'sender_only':
      return { title: options.senderDisplayName || 'New Message', body: '' };
    case 'full':
    default:
      return {
        title: options.title,
        body: options.body || 'New encrypted message',
      };
  }
}

class DesktopNotificationService {
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
    const content = applyContentPrivacy(options);
    let body = content.body;

    // Truncate body to 100 chars
    if (body.length > 100) {
      body = body.slice(0, 97) + '...';
    }

    try {
      const notification = new Notification(content.title, {
        body,
        silent: true, // We handle sounds separately via notificationSoundService
      });

      notification.onclick = () => {
        this.handleClick(options.targetType, options.targetId, options.serverId);
      };

      // Flash the taskbar/dock to draw attention
      this.flashForAttention();
    } catch (err) {
      // This catch spans flashForAttention() as well as the Notification
      // constructor, so "Notification API not available" stopped being the only
      // thing it could hide. Report what was actually caught rather than
      // asserting a cause nothing checked.
      console.warn('desktop notification failed:', err instanceof Error ? err.message : 'unknown');
    }
  }

  private flashActive = false;

  /**
   * Stop the flash, if it is running and the window is genuinely back.
   *
   * A stable bound reference so `removeEventListener` can find it, and
   * deliberately NOT `{ once: true }`: it returns early while the window is
   * still in the background, so it has to be able to fire again.
   */
  private readonly releaseFlash = (): void => {
    if (!this.flashActive) return;
    // `visibilitychange` also fires when the window becomes HIDDEN. Releasing
    // then would stop a flash that is still doing its job.
    if (!readWindowFocus()) return;

    this.flashActive = false;
    globalThis.removeEventListener('focus', this.releaseFlash);
    document.removeEventListener('visibilitychange', this.releaseFlash);
    globalThis.electron?.flashFrame?.(false);
  };

  /**
   * Bounce the dock / flash the taskbar, and arrange for it to STOP (#2403).
   *
   * `flashFrame(true)` is not self-clearing. Electron's 31.0 breaking change
   * made it "flash continuously until `flashFrame(false)` is called" on macOS
   * too, bringing it to parity with Windows and Linux — and this app is on
   * Electron 44. Nothing in the renderer has ever called `flashFrame(false)`,
   * so every notification started a dock bounce that never stopped. Electron's
   * own taskbar tutorial gives the pairing verbatim:
   * `win.once('focus', () => win.flashFrame(false))`.
   *
   * The focus GUARD is the second half, and it is not an optimisation.
   * `shouldNotify` suppresses only when the window is focused AND the message
   * is for the active channel — so a notification legitimately fires while the
   * window is focused. Flashing then is unclearable by construction: the
   * `focus` event that would stop it has already happened and will not repeat.
   * A dock bounce demanding attention from a window the user is looking at is
   * also just noise.
   *
   * One listener at a time: a burst of notifications must not accumulate
   * handlers, which is what the `flashActive` latch buys.
   */
  private flashForAttention(): void {
    // Already focused — see above. Nothing to draw attention to, and nothing
    // would ever clear it.
    if (readWindowFocus()) return;
    if (this.flashActive) return;

    this.flashActive = true;
    // Read the bridge once and call the method on it, so `this` is preserved.
    const bridge = globalThis.electron;
    const started = bridge?.flashFrame?.(true);
    started?.catch?.(() => {
      // The flash never started. Leaving the latch set would suppress every
      // later notification's flash for the rest of the session — a worse
      // outcome than the failed flash itself.
      this.flashActive = false;
      globalThis.removeEventListener('focus', this.releaseFlash);
      document.removeEventListener('visibilitychange', this.releaseFlash);
    });

    // TWO signals, because the GATE above reads two. `readWindowFocus()` is
    // `document.hasFocus() && !document.hidden`, and `useWindowFocus` subscribes
    // to focus, blur AND visibilitychange for exactly that reason.
    //
    // Watching `focus` alone left a reachable latch: a window occluded or
    // minimised while STILL HOLDING OS FOCUS has `hidden === true`, so the gate
    // lets the flash start — and restoring it fires `visibilitychange` and not
    // `focus`, because focus was never lost. The one-shot listener then never
    // ran, `flashActive` stayed true for the process lifetime, the dock bounced
    // forever (the very bug above), and every later notification hit the
    // `flashActive` guard and was silently denied its flash.
    //
    // Bubble phase, deliberately: `focus` does not bubble but it DOES capture,
    // so a capture-phase window listener fires for every inner element that
    // takes focus and would stop the flash on a stray tab-press while the app
    // is still in the background. Same reasoning as `useWindowFocus`.
    globalThis.addEventListener('focus', this.releaseFlash);
    document.addEventListener('visibilitychange', this.releaseFlash);
  }

  /**
   * Handle notification click — focus window and navigate to target.
   */
  private handleClick(targetType: 'channel' | 'dm', targetId: string, serverId?: string): void {
    // Import dynamically to avoid circular deps
    import('../../stores/ui/notificationNavigationStore').then(
      ({ useNotificationNavigationStore }) => {
        useNotificationNavigationStore.getState().setPendingNavigation({
          type: targetType,
          targetId,
          serverId,
        });
      }
    );

    // Focus the app window
    globalThis.electron?.focusWindow?.();
  }

  /**
   * Check if current time falls within quiet hours.
   * Handles midnight wrap (e.g., 22:00 - 08:00).
   */
  isInQuietHours(start: string, end: string): boolean {
    return isInQuietHours(start, end);
  }
}

export const desktopNotificationService = new DesktopNotificationService();
