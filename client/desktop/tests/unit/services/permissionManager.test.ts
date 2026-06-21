// @vitest-environment node
import { vi, describe, it, expect, beforeEach } from 'vitest';

const mockGetMediaAccessStatus = vi.fn().mockReturnValue('granted');
const mockAskForMediaAccess = vi.fn().mockResolvedValue(true);
const mockGetNotificationSettings = vi.fn().mockReturnValue({
  authorizationStatus: 'authorized',
});
const mockIsSupported = vi.fn().mockReturnValue(true);
const mockNotificationShow = vi.fn();
const mockIsEncryptionAvailable = vi.fn().mockReturnValue(true);
const mockOpenExternal = vi.fn();
const mockHandle = vi.fn();
const mockSend = vi.fn();
const mockIsPackaged = vi.fn().mockReturnValue(true);

vi.mock('electron', () => ({
  app: {
    get isPackaged() {
      return mockIsPackaged();
    },
  },
  BrowserWindow: vi.fn(),
  ipcMain: {
    handle: (...args: unknown[]) => mockHandle(...args),
  },
  Notification: Object.assign(
    function MockNotification() {
      return { show: mockNotificationShow };
    },
    { isSupported: () => mockIsSupported() }
  ),
  safeStorage: {
    isEncryptionAvailable: () => mockIsEncryptionAvailable(),
  },
  shell: {
    openExternal: (...args: unknown[]) => mockOpenExternal(...args),
  },
  systemPreferences: {
    getMediaAccessStatus: (...args: unknown[]) => mockGetMediaAccessStatus(...args),
    askForMediaAccess: (...args: unknown[]) => mockAskForMediaAccess(...args),
    getNotificationSettings: (...args: unknown[]) => mockGetNotificationSettings(...args),
  },
}));

import {
  checkPermission,
  checkAllPermissions,
  requestPermission,
  openSystemSettings,
  registerIpcHandlers,
  type OsPermissionType,
} from '@/main/permissionManager';

describe('permissionManager', () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe('checkPermission', () => {
    it('microphone: granted on macOS', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('granted');
      expect(checkPermission('microphone')).toBe('granted');
    });

    it('microphone: denied on macOS', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('denied');
      expect(checkPermission('microphone')).toBe('denied');
    });

    it('microphone: restricted on macOS', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('restricted');
      expect(checkPermission('microphone')).toBe('restricted');
    });

    it('microphone: not-determined on macOS', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      expect(checkPermission('microphone')).toBe('not-determined');
    });

    it('microphone: unavailable for unknown macOS status', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('unknown');
      expect(checkPermission('microphone')).toBe('unavailable');
    });

    it('microphone: granted on Windows', () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      expect(checkPermission('microphone')).toBe('granted');
    });

    it('camera: uses macOS media access', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('granted');
      expect(checkPermission('camera')).toBe('granted');
    });

    it('camera: granted on non-macOS', () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      expect(checkPermission('camera')).toBe('granted');
    });

    it('screen: uses macOS media access', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('denied');
      expect(checkPermission('screen')).toBe('denied');
    });

    it('screen: granted on Windows', () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      expect(checkPermission('screen')).toBe('granted');
    });

    it('secureStorage: granted when available', () => {
      mockIsEncryptionAvailable.mockReturnValue(true);
      expect(checkPermission('secureStorage')).toBe('granted');
    });

    it('secureStorage: unavailable when not available', () => {
      mockIsEncryptionAvailable.mockReturnValue(false);
      expect(checkPermission('secureStorage')).toBe('unavailable');
    });

    it('notifications: unavailable when not supported', () => {
      mockIsSupported.mockReturnValue(false);
      expect(checkPermission('notifications')).toBe('unavailable');
    });

    it('notifications: granted on macOS when authorized', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(true);
      mockGetNotificationSettings.mockReturnValueOnce({ authorizationStatus: 'authorized' });
      expect(checkPermission('notifications')).toBe('granted');
    });

    it('notifications: denied on macOS', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(true);
      mockGetNotificationSettings.mockReturnValueOnce({ authorizationStatus: 'denied' });
      expect(checkPermission('notifications')).toBe('denied');
    });

    it('notifications: provisional treated as granted', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(true);
      mockGetNotificationSettings.mockReturnValueOnce({ authorizationStatus: 'provisional' });
      expect(checkPermission('notifications')).toBe('granted');
    });

    it('notifications: granted on Windows', () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      mockIsSupported.mockReturnValue(true);
      expect(checkPermission('notifications')).toBe('granted');
    });

    it('notifications: granted in dev mode (unpackaged) on macOS', () => {
      // macOS caches notification authorization per-process, and the dev
      // Electron binary isn't a registered notification provider — so we
      // short-circuit to 'granted' in dev mode regardless of the OS state.
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(true);
      mockIsPackaged.mockReturnValueOnce(false);
      // Deliberately return 'denied' from the OS to prove we don't ask:
      mockGetNotificationSettings.mockReturnValueOnce({ authorizationStatus: 'denied' });
      expect(checkPermission('notifications')).toBe('granted');
    });

    it('notifications: falls through to macOS check in packaged mode', () => {
      // Sanity check the opposite branch: when packaged, we DO call the
      // real macOS API and respect its answer.
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(true);
      mockIsPackaged.mockReturnValueOnce(true);
      mockGetNotificationSettings.mockReturnValueOnce({ authorizationStatus: 'denied' });
      expect(checkPermission('notifications')).toBe('denied');
    });

    it('unknown type: unavailable', () => {
      expect(checkPermission('unknown' as OsPermissionType)).toBe('unavailable');
    });
  });

  describe('checkAllPermissions', () => {
    it('returns all permission types', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValue('granted');
      mockIsSupported.mockReturnValue(true);
      mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'authorized' });
      const result = checkAllPermissions();
      expect(result).toHaveProperty('microphone');
      expect(result).toHaveProperty('camera');
      expect(result).toHaveProperty('screen');
      expect(result).toHaveProperty('secureStorage');
      expect(result).toHaveProperty('notifications');
    });
  });

  describe('requestPermission', () => {
    it('microphone: granted on macOS when already granted', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('granted');
      expect(await requestPermission('microphone')).toBe('granted');
    });

    it('microphone: asks via askForMediaAccess in packaged build', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(true);
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      mockAskForMediaAccess.mockResolvedValueOnce(true);
      expect(await requestPermission('microphone')).toBe('granted');
      expect(mockAskForMediaAccess).toHaveBeenCalledWith('microphone');
    });

    it('microphone: denied via askForMediaAccess in packaged build', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(true);
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      mockAskForMediaAccess.mockResolvedValueOnce(false);
      expect(await requestPermission('microphone')).toBe('denied');
    });

    it('microphone: returns not-determined in dev mode (skips askForMediaAccess)', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(false);
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      expect(await requestPermission('microphone')).toBe('not-determined');
      expect(mockAskForMediaAccess).not.toHaveBeenCalled();
    });

    it('microphone: already denied skips askForMediaAccess', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(true);
      mockGetMediaAccessStatus.mockReturnValueOnce('denied');
      expect(await requestPermission('microphone')).toBe('denied');
      expect(mockAskForMediaAccess).not.toHaveBeenCalled();
    });

    it('camera: already denied skips askForMediaAccess', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(true);
      mockGetMediaAccessStatus.mockReturnValueOnce('denied');
      expect(await requestPermission('camera')).toBe('denied');
      expect(mockAskForMediaAccess).not.toHaveBeenCalled();
    });

    it('microphone: granted on Windows', async () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      expect(await requestPermission('microphone')).toBe('granted');
    });

    it('camera: granted on macOS when already granted', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('granted');
      expect(await requestPermission('camera')).toBe('granted');
    });

    it('camera: asks via askForMediaAccess in packaged build', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(true);
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      mockAskForMediaAccess.mockResolvedValueOnce(true);
      expect(await requestPermission('camera')).toBe('granted');
      expect(mockAskForMediaAccess).toHaveBeenCalledWith('camera');
    });

    it('camera: returns not-determined in dev mode', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsPackaged.mockReturnValue(false);
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      expect(await requestPermission('camera')).toBe('not-determined');
      expect(mockAskForMediaAccess).not.toHaveBeenCalled();
    });

    it('screen: opens settings on macOS', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockGetMediaAccessStatus.mockReturnValueOnce('not-determined');
      await requestPermission('screen');
      expect(mockOpenExternal).toHaveBeenCalledWith(expect.stringContaining('ScreenCapture'));
    });

    it('screen: granted on non-macOS', async () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      expect(await requestPermission('screen')).toBe('granted');
    });

    it('secureStorage: returns check result', async () => {
      mockIsEncryptionAvailable.mockReturnValue(true);
      expect(await requestPermission('secureStorage')).toBe('granted');
    });

    it('notifications: shows notification on macOS', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(true);
      mockGetNotificationSettings.mockReturnValue({ authorizationStatus: 'authorized' });
      await requestPermission('notifications');
      expect(mockNotificationShow).toHaveBeenCalled();
    });

    it('notifications: opens settings when not supported on macOS', async () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      mockIsSupported.mockReturnValue(false);
      await requestPermission('notifications');
      expect(mockOpenExternal).toHaveBeenCalledWith(expect.stringContaining('notifications'));
    });

    it('unknown type: unavailable', async () => {
      expect(await requestPermission('unknown' as OsPermissionType)).toBe('unavailable');
    });
  });

  describe('openSystemSettings', () => {
    it('macOS microphone', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      openSystemSettings('microphone');
      expect(mockOpenExternal).toHaveBeenCalledWith(expect.stringContaining('Privacy_Microphone'));
    });

    it('macOS camera', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      openSystemSettings('camera');
      expect(mockOpenExternal).toHaveBeenCalledWith(expect.stringContaining('Privacy_Camera'));
    });

    it('macOS screen', () => {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      openSystemSettings('screen');
      expect(mockOpenExternal).toHaveBeenCalledWith(expect.stringContaining('ScreenCapture'));
    });

    it('Windows microphone', () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      openSystemSettings('microphone');
      expect(mockOpenExternal).toHaveBeenCalledWith('ms-settings:privacy-microphone');
    });

    it('Windows notifications', () => {
      Object.defineProperty(process, 'platform', { value: 'win32', configurable: true });
      openSystemSettings('notifications');
      expect(mockOpenExternal).toHaveBeenCalledWith('ms-settings:notifications');
    });

    it('Linux logs warning', () => {
      Object.defineProperty(process, 'platform', { value: 'linux', configurable: true });
      vi.spyOn(console, 'warn').mockImplementation(() => {});
      openSystemSettings('microphone');
      expect(mockOpenExternal).not.toHaveBeenCalled();
    });
  });

  describe('registerIpcHandlers', () => {
    it('registers all handlers', () => {
      registerIpcHandlers(vi.fn().mockReturnValue(null));
      const channels = mockHandle.mock.calls.map((c: unknown[]) => c[0]);
      expect(channels).toContain('permission:checkAll');
      expect(channels).toContain('permission:check');
      expect(channels).toContain('permission:request');
      expect(channels).toContain('permission:openSettings');
    });

    it('check handler validates type', () => {
      registerIpcHandlers(vi.fn().mockReturnValue(null));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:check'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      expect(() => handler({}, 'invalid')).toThrow('Unknown permission type');
    });

    it('check handler returns status', () => {
      registerIpcHandlers(vi.fn().mockReturnValue(null));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:check'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      mockIsEncryptionAvailable.mockReturnValue(true);
      expect(handler({}, 'secureStorage')).toBe('granted');
    });

    it('request handler notifies renderer', async () => {
      const win = { isDestroyed: vi.fn().mockReturnValue(false), webContents: { send: mockSend } };
      registerIpcHandlers(vi.fn().mockReturnValue(win));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:request'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      mockIsEncryptionAvailable.mockReturnValue(true);
      await handler({}, 'secureStorage');
      expect(mockSend).toHaveBeenCalledWith('permission:changed', {
        type: 'secureStorage',
        status: 'granted',
      });
    });

    it('request handler skips destroyed window', async () => {
      const win = { isDestroyed: vi.fn().mockReturnValue(true), webContents: { send: mockSend } };
      registerIpcHandlers(vi.fn().mockReturnValue(win));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:request'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      mockIsEncryptionAvailable.mockReturnValue(true);
      await handler({}, 'secureStorage');
      expect(mockSend).not.toHaveBeenCalled();
    });

    it('request handler skips null window', async () => {
      registerIpcHandlers(vi.fn().mockReturnValue(null));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:request'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      mockIsEncryptionAvailable.mockReturnValue(true);
      await handler({}, 'secureStorage');
      expect(mockSend).not.toHaveBeenCalled();
    });

    it('request handler throws for invalid type', async () => {
      registerIpcHandlers(vi.fn().mockReturnValue(null));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:request'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      await expect(handler({}, 'bogus')).rejects.toThrow('Unknown permission type');
    });

    it('openSettings handler throws for invalid type', () => {
      registerIpcHandlers(vi.fn().mockReturnValue(null));
      const call = mockHandle.mock.calls.find(
        (c: unknown[]) => (c as string[])[0] === 'permission:openSettings'
      );
      const handler = (call as [string, (...args: unknown[]) => unknown])[1];
      expect(() => handler({}, 'bogus')).toThrow('Unknown permission type');
    });
  });
});
