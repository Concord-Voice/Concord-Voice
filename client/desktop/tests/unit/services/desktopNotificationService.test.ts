import { describe, it, expect, beforeEach, vi, type Mock } from 'vitest';
import { useNotificationStore } from '../../../src/renderer/stores/notificationStore';

// Mock the dynamic import of notificationNavigationStore
const mockSetPendingNavigation = vi.fn();
vi.mock('../../../src/renderer/stores/notificationNavigationStore', () => ({
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
  await import('../../../src/renderer/services/desktopNotificationService');

describe('DesktopNotificationService', () => {
  beforeEach(() => {
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

    // Reset badge count
    desktopNotificationService.clearBadge();
    vi.clearAllMocks(); // Clear the setBadgeCount call from clearBadge
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

    it('calls flashFrame to attract attention', () => {
      desktopNotificationService.notify({
        title: 'Alice',
        senderDisplayName: 'Alice',
        body: 'Hello',
        targetType: 'dm',
        targetId: 'dm-123',
        senderId: 'user-1',
      });

      expect(mockFlashFrame).toHaveBeenCalledWith(true);
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

  describe('incrementBadge and clearBadge', () => {
    it('increments badge count and calls setBadgeCount', () => {
      desktopNotificationService.incrementBadge();
      expect(desktopNotificationService.getBadgeCount()).toBe(1);
      expect(mockSetBadgeCount).toHaveBeenCalledWith(1);

      desktopNotificationService.incrementBadge();
      expect(desktopNotificationService.getBadgeCount()).toBe(2);
      expect(mockSetBadgeCount).toHaveBeenCalledWith(2);
    });

    it('clears badge count and calls setBadgeCount with 0', () => {
      desktopNotificationService.incrementBadge();
      desktopNotificationService.incrementBadge();
      desktopNotificationService.clearBadge();

      expect(desktopNotificationService.getBadgeCount()).toBe(0);
      expect(mockSetBadgeCount).toHaveBeenCalledWith(0);
    });
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
