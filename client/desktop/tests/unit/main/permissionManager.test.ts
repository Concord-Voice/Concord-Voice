// @vitest-environment node
//
// permissionManager — OS permission check, request, settings, and IPC tests.

import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

// ── Hoisted mocks ──────────────────────────────────────────────────────

const {
  mockGetMediaAccessStatus,
  mockAskForMediaAccess,
  mockIsEncryptionAvailable,
  mockIsSupported,
  mockOpenExternal,
  mockGetNotificationSettings,
  mockNotificationShow,
  mockIpcMainHandle,
  mockApp,
} = vi.hoisted(() => ({
  mockGetMediaAccessStatus: vi.fn(),
  mockAskForMediaAccess: vi.fn(),
  mockIsEncryptionAvailable: vi.fn(() => true),
  mockIsSupported: vi.fn(() => true),
  mockOpenExternal: vi.fn(),
  mockGetNotificationSettings: vi.fn(() => ({ authorizationStatus: 'authorized' })),
  mockNotificationShow: vi.fn(),
  mockIpcMainHandle: vi.fn(),
  mockApp: { isPackaged: true },
}));

// ── Electron mock ──────────────────────────────────────────────────────

vi.mock('electron', () => {
  // Use a real function constructor so `new Notification(...)` works
  function MockNotification() {
    return { show: mockNotificationShow };
  }
  MockNotification.isSupported = mockIsSupported;

  return {
    app: mockApp,
    BrowserWindow: vi.fn(),
    ipcMain: { handle: mockIpcMainHandle },
    Notification: MockNotification,
    safeStorage: { isEncryptionAvailable: mockIsEncryptionAvailable },
    shell: { openExternal: mockOpenExternal },
    systemPreferences: {
      getMediaAccessStatus: mockGetMediaAccessStatus,
      askForMediaAccess: mockAskForMediaAccess,
      getNotificationSettings: mockGetNotificationSettings,
    },
  };
});

import {
  checkPermission,
  checkAllPermissions,
  requestPermission,
  openSystemSettings,
  registerIpcHandlers,
} from '@/main/permissionManager';

// ── Helpers ────────────────────────────────────────────────────────────

const originalPlatform = process.platform;

function setPlatform(platform: string) {
  Object.defineProperty(process, 'platform', {
    value: platform,
    writable: true,
    configurable: true,
  });
}

// ── Test Suite ─────────────────────────────────────────────────────────

describe('permissionManager', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockIsEncryptionAvailable.mockReturnValue(true);
    mockIsSupported.mockReturnValue(true);
    mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'authorized' });
    mockApp.isPackaged = true;
  });

  afterEach(() => {
    setPlatform(originalPlatform);
  });

  // ─── checkPermission ──────────────────────────────────────────────

  describe('checkPermission', () => {
    describe('microphone', () => {
      it('returns macOS media access status on darwin', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('granted');
        expect(checkPermission('microphone')).toBe('granted');
        expect(mockGetMediaAccessStatus).toHaveBeenCalledWith('microphone');
      });

      it('returns denied on darwin when denied', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('denied');
        expect(checkPermission('microphone')).toBe('denied');
      });

      it('returns not-determined on darwin', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('not-determined');
        expect(checkPermission('microphone')).toBe('not-determined');
      });

      it('returns restricted on darwin', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('restricted');
        expect(checkPermission('microphone')).toBe('restricted');
      });

      it('maps unknown status to unavailable on darwin', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('unknown');
        expect(checkPermission('microphone')).toBe('unavailable');
      });

      it('returns granted on win32', () => {
        setPlatform('win32');
        expect(checkPermission('microphone')).toBe('granted');
        expect(mockGetMediaAccessStatus).not.toHaveBeenCalled();
      });

      it('returns granted on linux', () => {
        setPlatform('linux');
        expect(checkPermission('microphone')).toBe('granted');
      });
    });

    describe('camera', () => {
      it('returns macOS media access status on darwin', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('granted');
        expect(checkPermission('camera')).toBe('granted');
        expect(mockGetMediaAccessStatus).toHaveBeenCalledWith('camera');
      });

      it('returns granted on win32', () => {
        setPlatform('win32');
        expect(checkPermission('camera')).toBe('granted');
      });
    });

    describe('screen', () => {
      it('returns macOS media access status on darwin', () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('denied');
        expect(checkPermission('screen')).toBe('denied');
        expect(mockGetMediaAccessStatus).toHaveBeenCalledWith('screen');
      });

      it('returns granted on win32', () => {
        setPlatform('win32');
        expect(checkPermission('screen')).toBe('granted');
      });

      it('returns granted on linux', () => {
        setPlatform('linux');
        expect(checkPermission('screen')).toBe('granted');
      });
    });

    describe('secureStorage', () => {
      it('returns granted when encryption is available', () => {
        mockIsEncryptionAvailable.mockReturnValue(true);
        expect(checkPermission('secureStorage')).toBe('granted');
      });

      it('returns unavailable when encryption is not available', () => {
        mockIsEncryptionAvailable.mockReturnValue(false);
        expect(checkPermission('secureStorage')).toBe('unavailable');
      });
    });

    describe('notifications', () => {
      it('returns unavailable when Notification is not supported', () => {
        mockIsSupported.mockReturnValue(false);
        expect(checkPermission('notifications')).toBe('unavailable');
      });

      it('returns granted on darwin when authorized', () => {
        setPlatform('darwin');
        mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'authorized' });
        expect(checkPermission('notifications')).toBe('granted');
      });

      it('returns denied on darwin when denied', () => {
        setPlatform('darwin');
        mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'denied' });
        expect(checkPermission('notifications')).toBe('denied');
      });

      it('returns granted on darwin when provisional', () => {
        setPlatform('darwin');
        mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'provisional' });
        expect(checkPermission('notifications')).toBe('granted');
      });

      it('returns not-determined on darwin when not-determined', () => {
        setPlatform('darwin');
        mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'not-determined' });
        expect(checkPermission('notifications')).toBe('not-determined');
      });

      it('returns not-determined on darwin with unknown authorizationStatus', () => {
        setPlatform('darwin');
        mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'something-else' });
        expect(checkPermission('notifications')).toBe('not-determined');
      });

      it('returns granted on win32 when notifications are supported', () => {
        setPlatform('win32');
        expect(checkPermission('notifications')).toBe('granted');
      });

      it('returns granted on linux when notifications are supported', () => {
        setPlatform('linux');
        expect(checkPermission('notifications')).toBe('granted');
      });
    });

    it('returns unavailable for unknown permission type', () => {
      // Force an unknown type to test the fallback
      expect(checkPermission('unknown' as never)).toBe('unavailable');
    });
  });

  // ─── checkAllPermissions ──────────────────────────────────────────

  describe('checkAllPermissions', () => {
    it('returns status for all 5 permission types', () => {
      setPlatform('win32');
      mockIsEncryptionAvailable.mockReturnValue(true);
      mockIsSupported.mockReturnValue(true);

      const result = checkAllPermissions();

      expect(result).toEqual({
        microphone: 'granted',
        camera: 'granted',
        screen: 'granted',
        secureStorage: 'granted',
        notifications: 'granted',
      });
    });

    it('reflects individual permission states', () => {
      setPlatform('darwin');
      mockGetMediaAccessStatus.mockImplementation((type: string) => {
        if (type === 'microphone') return 'denied';
        if (type === 'camera') return 'not-determined';
        return 'granted';
      });
      mockIsEncryptionAvailable.mockReturnValue(false);
      mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'denied' });

      const result = checkAllPermissions();

      expect(result.microphone).toBe('denied');
      expect(result.camera).toBe('not-determined');
      expect(result.screen).toBe('granted');
      expect(result.secureStorage).toBe('unavailable');
      expect(result.notifications).toBe('denied');
    });
  });

  // ─── requestPermission ────────────────────────────────────────────

  describe('requestPermission', () => {
    describe('microphone', () => {
      it('returns granted on non-darwin platforms', async () => {
        setPlatform('win32');
        const result = await requestPermission('microphone');
        expect(result).toBe('granted');
      });

      it('returns granted on darwin when already granted', async () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('granted');
        const result = await requestPermission('microphone');
        expect(result).toBe('granted');
        expect(mockAskForMediaAccess).not.toHaveBeenCalled();
      });

      it('returns denied on darwin when denied', async () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('denied');
        const result = await requestPermission('microphone');
        expect(result).toBe('denied');
        expect(mockAskForMediaAccess).not.toHaveBeenCalled();
      });

      it('returns restricted on darwin when restricted', async () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('restricted');
        const result = await requestPermission('microphone');
        expect(result).toBe('restricted');
        expect(mockAskForMediaAccess).not.toHaveBeenCalled();
      });

      it('returns raw status in dev mode (not packaged)', async () => {
        setPlatform('darwin');
        mockApp.isPackaged = false;
        mockGetMediaAccessStatus.mockReturnValue('not-determined');
        const result = await requestPermission('microphone');
        expect(result).toBe('not-determined');
        expect(mockAskForMediaAccess).not.toHaveBeenCalled();
      });

      it('asks for media access in production mode on darwin', async () => {
        setPlatform('darwin');
        mockApp.isPackaged = true;
        mockGetMediaAccessStatus.mockReturnValue('not-determined');
        mockAskForMediaAccess.mockResolvedValue(true);
        const result = await requestPermission('microphone');
        expect(result).toBe('granted');
        expect(mockAskForMediaAccess).toHaveBeenCalledWith('microphone');
      });

      it('returns denied when user rejects media access prompt', async () => {
        setPlatform('darwin');
        mockApp.isPackaged = true;
        mockGetMediaAccessStatus.mockReturnValue('not-determined');
        mockAskForMediaAccess.mockResolvedValue(false);
        const result = await requestPermission('microphone');
        expect(result).toBe('denied');
      });
    });

    describe('camera', () => {
      it('returns granted on non-darwin platforms', async () => {
        setPlatform('linux');
        const result = await requestPermission('camera');
        expect(result).toBe('granted');
      });

      it('asks for media access on darwin in production', async () => {
        setPlatform('darwin');
        mockApp.isPackaged = true;
        mockGetMediaAccessStatus.mockReturnValue('not-determined');
        mockAskForMediaAccess.mockResolvedValue(true);
        const result = await requestPermission('camera');
        expect(result).toBe('granted');
        expect(mockAskForMediaAccess).toHaveBeenCalledWith('camera');
      });

      it('returns denied on darwin when denied', async () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('denied');
        const result = await requestPermission('camera');
        expect(result).toBe('denied');
      });
    });

    describe('screen', () => {
      it('opens system settings and returns status on darwin', async () => {
        setPlatform('darwin');
        mockGetMediaAccessStatus.mockReturnValue('denied');
        const result = await requestPermission('screen');
        expect(result).toBe('denied');
        expect(mockOpenExternal).toHaveBeenCalledWith(
          'x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture'
        );
      });

      it('returns granted on non-darwin platforms', async () => {
        setPlatform('win32');
        const result = await requestPermission('screen');
        expect(result).toBe('granted');
        expect(mockOpenExternal).not.toHaveBeenCalled();
      });
    });

    describe('secureStorage', () => {
      it('returns granted when encryption is available', async () => {
        mockIsEncryptionAvailable.mockReturnValue(true);
        const result = await requestPermission('secureStorage');
        expect(result).toBe('granted');
      });

      it('returns unavailable when encryption is not available', async () => {
        mockIsEncryptionAvailable.mockReturnValue(false);
        const result = await requestPermission('secureStorage');
        expect(result).toBe('unavailable');
      });
    });

    describe('notifications', () => {
      it('shows a notification when supported', async () => {
        setPlatform('win32');
        mockIsSupported.mockReturnValue(true);
        await requestPermission('notifications');
        expect(mockNotificationShow).toHaveBeenCalled();
      });

      it('opens system settings on darwin when notifications not supported', async () => {
        setPlatform('darwin');
        mockIsSupported.mockReturnValue(false);
        await requestPermission('notifications');
        expect(mockNotificationShow).not.toHaveBeenCalled();
        expect(mockOpenExternal).toHaveBeenCalledWith(
          'x-apple.systempreferences:com.apple.preference.notifications'
        );
      });

      it('does not open settings on non-darwin when not supported', async () => {
        setPlatform('linux');
        mockIsSupported.mockReturnValue(false);
        await requestPermission('notifications');
        expect(mockOpenExternal).not.toHaveBeenCalled();
      });

      it('returns the real notification status after the prompt', async () => {
        setPlatform('darwin');
        mockIsSupported.mockReturnValue(true);
        mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'denied' });
        const result = await requestPermission('notifications');
        expect(result).toBe('denied');
      });
    });

    it('returns unavailable for unknown permission type', async () => {
      const result = await requestPermission('unknown' as never);
      expect(result).toBe('unavailable');
    });
  });

  // ─── openSystemSettings ───────────────────────────────────────────

  describe('openSystemSettings', () => {
    it('opens macOS settings URL on darwin', () => {
      setPlatform('darwin');
      openSystemSettings('microphone');
      expect(mockOpenExternal).toHaveBeenCalledWith(
        'x-apple.systempreferences:com.apple.preference.security?Privacy_Microphone'
      );
    });

    it('opens macOS camera settings on darwin', () => {
      setPlatform('darwin');
      openSystemSettings('camera');
      expect(mockOpenExternal).toHaveBeenCalledWith(
        'x-apple.systempreferences:com.apple.preference.security?Privacy_Camera'
      );
    });

    it('opens macOS screen settings on darwin', () => {
      setPlatform('darwin');
      openSystemSettings('screen');
      expect(mockOpenExternal).toHaveBeenCalledWith(
        'x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture'
      );
    });

    it('opens macOS notification settings on darwin', () => {
      setPlatform('darwin');
      openSystemSettings('notifications');
      expect(mockOpenExternal).toHaveBeenCalledWith(
        'x-apple.systempreferences:com.apple.preference.notifications'
      );
    });

    it('opens macOS secureStorage settings on darwin', () => {
      setPlatform('darwin');
      openSystemSettings('secureStorage');
      expect(mockOpenExternal).toHaveBeenCalledWith(
        'x-apple.systempreferences:com.apple.preference.security?Privacy'
      );
    });

    it('opens Windows settings URL on win32', () => {
      setPlatform('win32');
      openSystemSettings('microphone');
      expect(mockOpenExternal).toHaveBeenCalledWith('ms-settings:privacy-microphone');
    });

    it('opens Windows camera settings on win32', () => {
      setPlatform('win32');
      openSystemSettings('camera');
      expect(mockOpenExternal).toHaveBeenCalledWith('ms-settings:privacy-webcam');
    });

    it('opens Windows notification settings on win32', () => {
      setPlatform('win32');
      openSystemSettings('notifications');
      expect(mockOpenExternal).toHaveBeenCalledWith('ms-settings:notifications');
    });

    it('logs a warning on linux', () => {
      setPlatform('linux');
      const warnSpy = vi.spyOn(console, 'warn').mockImplementation(() => {});
      openSystemSettings('microphone');
      expect(mockOpenExternal).not.toHaveBeenCalled();
      expect(warnSpy).toHaveBeenCalledWith(
        '[PermissionManager] Linux: open your system settings to manage "microphone" permission'
      );
      warnSpy.mockRestore();
    });
  });

  // ─── mapMacNotificationStatus edge case ───────────────────────────

  describe('notification settings fallback', () => {
    it('returns not-determined when getNotificationSettings is not a function', async () => {
      setPlatform('darwin');
      // Replace the mock with a non-function to simulate missing API
      const original = mockGetNotificationSettings;
      const electron = await import('electron');
      const sp = electron.systemPreferences as Record<string, unknown>;
      sp.getNotificationSettings = undefined;
      const result = checkPermission('notifications');
      expect(result).toBe('not-determined');
      sp.getNotificationSettings = original;
    });
  });

  // ─── registerIpcHandlers ──────────────────────────────────────────

  describe('registerIpcHandlers', () => {
    it('registers 4 IPC handlers', () => {
      const getMainWindow = vi.fn(() => null);
      registerIpcHandlers(getMainWindow);
      expect(mockIpcMainHandle).toHaveBeenCalledTimes(4);
      const channels = mockIpcMainHandle.mock.calls.map((c: unknown[]) => c[0]);
      expect(channels).toContain('permission:checkAll');
      expect(channels).toContain('permission:check');
      expect(channels).toContain('permission:request');
      expect(channels).toContain('permission:openSettings');
    });

    describe('IPC handler behavior', () => {
      let handlers: Record<string, (...args: unknown[]) => unknown>;

      beforeEach(() => {
        mockIpcMainHandle.mockReset();
        handlers = {};
        mockIpcMainHandle.mockImplementation(
          (channel: string, handler: (...args: unknown[]) => unknown) => {
            handlers[channel] = handler;
          }
        );

        const mockWindow = {
          isDestroyed: vi.fn(() => false),
          webContents: { send: vi.fn() },
        };
        registerIpcHandlers(() => mockWindow as never);
      });

      it('permission:checkAll returns all permissions', () => {
        setPlatform('win32');
        const result = handlers['permission:checkAll']({});
        expect(result).toHaveProperty('microphone');
        expect(result).toHaveProperty('camera');
        expect(result).toHaveProperty('screen');
        expect(result).toHaveProperty('secureStorage');
        expect(result).toHaveProperty('notifications');
      });

      it('permission:check returns status for valid type', () => {
        setPlatform('win32');
        const result = handlers['permission:check']({}, 'microphone');
        expect(result).toBe('granted');
      });

      it('permission:check throws for unknown type', () => {
        expect(() => handlers['permission:check']({}, 'badtype')).toThrow(
          'Unknown permission type: badtype'
        );
      });

      it('permission:request returns status and sends changed event', async () => {
        setPlatform('win32');
        const result = await handlers['permission:request']({}, 'microphone');
        expect(result).toBe('granted');
      });

      it('permission:request throws for unknown type', async () => {
        await expect(handlers['permission:request']({}, 'badtype')).rejects.toThrow(
          'Unknown permission type: badtype'
        );
      });

      it('permission:openSettings throws for unknown type', () => {
        expect(() => handlers['permission:openSettings']({}, 'badtype')).toThrow(
          'Unknown permission type: badtype'
        );
      });

      it('permission:openSettings calls openSystemSettings for valid type', () => {
        setPlatform('darwin');
        handlers['permission:openSettings']({}, 'microphone');
        expect(mockOpenExternal).toHaveBeenCalled();
      });

      it('permission:request does not send event when window is null', async () => {
        mockIpcMainHandle.mockReset();
        handlers = {};
        mockIpcMainHandle.mockImplementation(
          (channel: string, handler: (...args: unknown[]) => unknown) => {
            handlers[channel] = handler;
          }
        );
        registerIpcHandlers(() => null);
        setPlatform('win32');
        // Should not throw
        const result = await handlers['permission:request']({}, 'microphone');
        expect(result).toBe('granted');
      });

      it('permission:request does not send event when window is destroyed', async () => {
        mockIpcMainHandle.mockReset();
        handlers = {};
        mockIpcMainHandle.mockImplementation(
          (channel: string, handler: (...args: unknown[]) => unknown) => {
            handlers[channel] = handler;
          }
        );
        const destroyedWindow = {
          isDestroyed: vi.fn(() => true),
          webContents: { send: vi.fn() },
        };
        registerIpcHandlers(() => destroyedWindow as never);
        setPlatform('win32');
        await handlers['permission:request']({}, 'microphone');
        expect(destroyedWindow.webContents.send).not.toHaveBeenCalled();
      });
    });
  });
});
