import { describe, it, expect, beforeEach, vi, type Mock } from 'vitest';
import { useNotificationStore } from '../../../src/renderer/stores/ui/notificationStore';

// Mock the dynamic import of notificationNavigationStore
const mockSetPendingNavigation = vi.fn();
vi.mock('../../../src/renderer/stores/ui/notificationNavigationStore', () => ({
  useNotificationNavigationStore: {
    getState: () => ({
      setPendingNavigation: mockSetPendingNavigation,
    }),
  },
}));

// Mock Notification API
const mockOnClick: { handler: (() => void) | null } = { handler: null };

class MockNotification {
  title: string;
  body: string;
  silent: boolean;

  set onclick(fn: () => void) {
    mockOnClick.handler = fn;
  }

  constructor(title: string, options?: NotificationOptions) {
    this.title = title;
    this.body = options?.body ?? '';
    this.silent = options?.silent ?? false;
    MockNotification.instances.push(this);
  }

  static instances: MockNotification[] = [];
  static clear() {
    MockNotification.instances = [];
  }
}

vi.stubGlobal('Notification', MockNotification);

// Mock electron IPC methods on the existing global (setup.ts defines window.electron)
const mockSetBadgeCount = vi.fn();
const mockFlashFrame = vi.fn();
const mockFocusWindow = vi.fn();

if (globalThis.electron) {
  (globalThis.electron as Record<string, unknown>).setBadgeCount = mockSetBadgeCount;
  (globalThis.electron as Record<string, unknown>).flashFrame = mockFlashFrame;
  (globalThis.electron as Record<string, unknown>).focusWindow = mockFocusWindow;
}

// Import after mocking
const { desktopNotificationService } =
  await import('../../../src/renderer/services/system/desktopNotificationService');

describe('DesktopNotificationService', () => {
  beforeEach(() => {
    // #2403: the service tracks whether a dock flash is outstanding, and that
    // flag is module-singleton state that outlives a test. A test that flashes
    // without regaining focus would leave it set, and the NEXT test's flash
    // would be silently skipped — passing for the wrong reason
    // (`[internal]rules/tests.md` § Vacuity). Dispatching focus clears it through
    // the real listener, before `clearAllMocks` wipes the resulting call.
    globalThis.dispatchEvent(new Event('focus'));

    vi.clearAllMocks();
    MockNotification.clear();
    mockOnClick.handler = null;

    // Reset store to defaults
    useNotificationStore.setState({
      desktopNotificationsEnabled: true,
      desktopNotifyDMs: true,
      desktopNotifyMentions: true,
      desktopNotifyAllMessages: false,
      notificationContent: 'full',
      doNotDisturb: false,
      quietHoursEnabled: false,
      quietHoursStart: '22:00',
      quietHoursEnd: '08:00',
    });

    vi.useRealTimers();
  });

  // ── shouldNotify ──────────────────────────────────────────────────

  describe('shouldNotify', () => {
    it('returns false when desktopNotificationsEnabled is false', () => {
      useNotificationStore.setState({ desktopNotificationsEnabled: false });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(false);
    });

    it('returns false when window is focused and channel is active', () => {
      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: true,
          isActiveChannel: true,
        })
      ).toBe(false);
    });

    it('allows notifications when focused+active if suppressWhenFocused is off', () => {
      useNotificationStore.setState({ suppressWhenFocused: false });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: true,
          isActiveChannel: true,
        })
      ).toBe(true);
    });

    it('returns true when window is NOT focused even if channel is active', () => {
      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: true,
        })
      ).toBe(true);
    });

    it('returns false when doNotDisturb is true', () => {
      useNotificationStore.setState({ doNotDisturb: true });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(false);
    });

    it('returns false during quiet hours', () => {
      vi.useFakeTimers();
      // Set time to 23:00
      vi.setSystemTime(new Date(2026, 3, 2, 23, 0, 0));

      useNotificationStore.setState({
        quietHoursEnabled: true,
        quietHoursStart: '22:00',
        quietHoursEnd: '08:00',
      });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(false);
    });

    it('returns true outside quiet hours', () => {
      vi.useFakeTimers();
      // Set time to 12:00 (noon — outside 22:00-08:00)
      vi.setSystemTime(new Date(2026, 3, 2, 12, 0, 0));

      useNotificationStore.setState({
        quietHoursEnabled: true,
        quietHoursStart: '22:00',
        quietHoursEnd: '08:00',
      });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(true);
    });

    it('respects DM toggle', () => {
      useNotificationStore.setState({ desktopNotifyDMs: false });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(false);

      useNotificationStore.setState({ desktopNotifyDMs: true });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'dm',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(true);
    });

    it('respects mention toggle', () => {
      useNotificationStore.setState({ desktopNotifyMentions: false });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'mention',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(false);

      useNotificationStore.setState({ desktopNotifyMentions: true });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'mention',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(true);
    });

    it('respects allMessages toggle (defaults to false)', () => {
      // Default is false
      expect(
        desktopNotificationService.shouldNotify({
          type: 'message',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(false);

      useNotificationStore.setState({ desktopNotifyAllMessages: true });

      expect(
        desktopNotificationService.shouldNotify({
          type: 'message',
          isWindowFocused: false,
          isActiveChannel: false,
        })
      ).toBe(true);
    });
  });

  // ── notify ────────────────────────────────────────────────────────

  describe('notify', () => {
    it('creates a Notification with original title and body in full mode', () => {
      useNotificationStore.setState({ notificationContent: 'full' });

      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello there!',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      expect(MockNotification.instances[0].title).toBe('Alice');
      expect(MockNotification.instances[0].body).toBe('Hello there!');
    });

    it('shows only sender name in sender_only mode', () => {
      useNotificationStore.setState({ notificationContent: 'sender_only' });

      desktopNotificationService.notify({
        title: 'Alice in #ops',
        senderDisplayName: 'Alice',
        body: 'Hello there!',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      expect(MockNotification.instances[0].title).toBe('Alice');
      expect(MockNotification.instances[0].body).toBe('');
    });

    it('hides friendly media labels in sender_only mode', () => {
      useNotificationStore.setState({ notificationContent: 'sender_only' });

      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'GIF',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      expect(MockNotification.instances[0].title).toBe('Alice');
      expect(MockNotification.instances[0].body).toBe('');
    });

    it('hides title and body content in minimal mode', () => {
      useNotificationStore.setState({ notificationContent: 'minimal' });

      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello there!',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      expect(MockNotification.instances[0].title).toBe('New Message');
      expect(MockNotification.instances[0].body).toBe('');
    });

    it('truncates body to 100 chars', () => {
      const longBody = 'A'.repeat(150);

      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: longBody,
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      const body = MockNotification.instances[0].body;
      expect(body.length).toBe(100);
      expect(body).toBe('A'.repeat(97) + '...');
    });

    it('shows "New encrypted message" fallback for empty body', () => {
      useNotificationStore.setState({ notificationContent: 'full' });

      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: '',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      expect(MockNotification.instances[0].body).toBe('New encrypted message');
    });

    it('sets silent to true', () => {
      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(MockNotification.instances).toHaveLength(1);
      expect(MockNotification.instances[0].silent).toBe(true);
    });

    // Vitest's jsdom environment reports the document FOCUSED, so every case
    // that expects a dock flash must say so explicitly — the #2403 guard
    // suppresses the flash while focused, which makes this fixture load-bearing
    // rather than incidental. (A bare jsdom instance reports unfocused; the
    // difference is the environment, not the library.)
    const unfocusWindow = () => {
      const hasFocus = vi.spyOn(document, 'hasFocus').mockReturnValue(false);
      return () => hasFocus.mockRestore();
    };

    it('calls flashFrame to attract attention', () => {
      const refocus = unfocusWindow();

      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(mockFlashFrame).toHaveBeenCalledWith(true);
      refocus();
    });

    // ── Dock/taskbar flash lifecycle (#2403) ──────────────────────────────
    //
    // `flashFrame(true)` is NOT self-clearing. Electron 31 changed macOS to
    // match Windows/Linux — "flash continuously until `flashFrame(false)` is
    // called" — and this app is on Electron 44. Nothing ever called it with
    // `false`, so every notification started a permanent dock bounce.

    const notifyOnce = (targetId = 'dm-123') =>
      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello',
        targetType: 'dm',
        targetId,
        senderId: 'user-1',
      });

    it('stops the flash when the window regains focus', () => {
      const refocus = unfocusWindow();

      notifyOnce();
      expect(mockFlashFrame).toHaveBeenCalledWith(true);
      expect(mockFlashFrame).not.toHaveBeenCalledWith(false);

      refocus();
      globalThis.dispatchEvent(new Event('focus'));

      expect(mockFlashFrame).toHaveBeenCalledWith(false);
    });

    it('does not flash while the window is already focused', () => {
      // `shouldNotify` suppresses only when focused AND on the active channel,
      // so a notification legitimately fires while focused. Flashing then is
      // unclearable by construction — the `focus` event that would stop it has
      // already happened and will not repeat. No spy needed: the environment
      // already reports focused, which is the state under test.
      expect(document.hasFocus()).toBe(true);

      notifyOnce();

      expect(MockNotification.instances).toHaveLength(1); // the notification still fires
      expect(mockFlashFrame).not.toHaveBeenCalled(); // only the flash is suppressed
    });

    it('a burst registers one flash, and one focus clears it', () => {
      const refocus = unfocusWindow();

      notifyOnce('dm-1');
      notifyOnce('dm-2');
      notifyOnce('dm-3');

      expect(MockNotification.instances).toHaveLength(3);
      expect(mockFlashFrame.mock.calls.filter(([f]) => f === true)).toHaveLength(1);

      refocus();
      globalThis.dispatchEvent(new Event('focus'));

      expect(mockFlashFrame.mock.calls.filter(([f]) => f === false)).toHaveLength(1);
    });

    it('onclick calls focusWindow and sets pending navigation', async () => {
      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello',
        targetType: 'channel',
        targetId: 'ch-456',
        serverId: 'srv-789',
        senderId: 'user-1',
      });

      expect(mockOnClick.handler).toBeDefined();

      // Trigger the click handler
      mockOnClick.handler!();

      // Flush the dynamic import promise (microtask)
      await new Promise((resolve) => setTimeout(resolve, 0));

      expect(mockFocusWindow).toHaveBeenCalled();
      expect(mockSetPendingNavigation).toHaveBeenCalledWith({
        type: 'channel',
        targetId: 'ch-456',
        serverId: 'srv-789',
      });
    });
  });

  // ── badge ─────────────────────────────────────────────────────────

  it('does not touch the OS badge — badgeSync owns it (#2403)', () => {
    const service = desktopNotificationService as unknown as Record<string, unknown>;
    expect(service.incrementBadge).toBeUndefined();
    expect(service.clearBadge).toBeUndefined();
    expect(service.getBadgeCount).toBeUndefined();

    desktopNotificationService.notify({
      title: 'Someone',
      senderDisplayName: 'Someone',
      body: 'hello',
      targetType: 'channel',
      targetId: 'c1',
      senderId: 'user-1',
    });

    expect(mockSetBadgeCount).not.toHaveBeenCalled();
  });

  // ── isInQuietHours ────────────────────────────────────────────────

  describe('isInQuietHours', () => {
    it('handles same-day range (08:00-22:00)', () => {
      vi.useFakeTimers();

      // 12:00 — inside 08:00-22:00
      vi.setSystemTime(new Date(2026, 3, 2, 12, 0, 0));
      expect(desktopNotificationService.isInQuietHours('08:00', '22:00')).toBe(true);

      // 07:00 — outside 08:00-22:00
      vi.setSystemTime(new Date(2026, 3, 2, 7, 0, 0));
      expect(desktopNotificationService.isInQuietHours('08:00', '22:00')).toBe(false);

      // 23:00 — outside 08:00-22:00
      vi.setSystemTime(new Date(2026, 3, 2, 23, 0, 0));
      expect(desktopNotificationService.isInQuietHours('08:00', '22:00')).toBe(false);
    });

    it('handles overnight range (22:00-08:00)', () => {
      vi.useFakeTimers();

      // 23:00 — inside 22:00-08:00
      vi.setSystemTime(new Date(2026, 3, 2, 23, 0, 0));
      expect(desktopNotificationService.isInQuietHours('22:00', '08:00')).toBe(true);

      // 03:00 — inside 22:00-08:00
      vi.setSystemTime(new Date(2026, 3, 2, 3, 0, 0));
      expect(desktopNotificationService.isInQuietHours('22:00', '08:00')).toBe(true);

      // 12:00 — outside 22:00-08:00
      vi.setSystemTime(new Date(2026, 3, 2, 12, 0, 0));
      expect(desktopNotificationService.isInQuietHours('22:00', '08:00')).toBe(false);

      // 08:00 — outside (end is exclusive)
      vi.setSystemTime(new Date(2026, 3, 2, 8, 0, 0));
      expect(desktopNotificationService.isInQuietHours('22:00', '08:00')).toBe(false);
    });
  });
});
