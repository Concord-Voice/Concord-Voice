import { vi } from 'vitest';
import { useOsPermissionStore, ensureOsPermission } from '@/renderer/stores/osPermissionStore';

function mockBrowserNotification(
  permission: NotificationPermission,
  requestPermission: (() => Promise<NotificationPermission>) | undefined = vi
    .fn()
    .mockResolvedValue(permission)
): void {
  Object.defineProperty(globalThis, 'Notification', {
    value: {
      permission,
      requestPermission,
    },
    configurable: true,
  });
}

beforeEach(() => {
  Object.defineProperty(globalThis, 'Notification', {
    value: undefined,
    configurable: true,
  });
  useOsPermissionStore.setState({
    microphone: 'not-determined',
    camera: 'not-determined',
    screen: 'not-determined',
    secureStorage: 'not-determined',
    notifications: 'not-determined',
    isLoaded: false,
  });
});

describe('osPermissionStore', () => {
  it('has correct initial state', () => {
    const state = useOsPermissionStore.getState();
    expect(state.microphone).toBe('not-determined');
    expect(state.camera).toBe('not-determined');
    expect(state.screen).toBe('not-determined');
    expect(state.secureStorage).toBe('not-determined');
    expect(state.notifications).toBe('not-determined');
    expect(state.isLoaded).toBe(false);
  });

  it('fetchAll updates all permissions from electron', async () => {
    window.electron.checkAllPermissions = vi.fn().mockResolvedValue({
      microphone: 'granted',
      camera: 'denied',
      screen: 'not-determined',
      secureStorage: 'granted',
      notifications: 'granted',
    });

    await useOsPermissionStore.getState().fetchAll();
    const state = useOsPermissionStore.getState();
    expect(state.microphone).toBe('granted');
    expect(state.camera).toBe('denied');
    expect(state.screen).toBe('not-determined');
    expect(state.secureStorage).toBe('granted');
    expect(state.notifications).toBe('granted');
    expect(state.isLoaded).toBe(true);
  });

  it('fetchAll sets unavailable when electron API missing', async () => {
    const original = window.electron.checkAllPermissions;
    window.electron.checkAllPermissions = undefined as unknown as typeof original;

    await useOsPermissionStore.getState().fetchAll();
    const state = useOsPermissionStore.getState();
    expect(state.microphone).toBe('unavailable');
    expect(state.isLoaded).toBe(true);

    window.electron.checkAllPermissions = original;
  });

  it('checkOne updates a single permission', async () => {
    window.electron.checkPermission = vi.fn().mockResolvedValue('granted');

    const result = await useOsPermissionStore.getState().checkOne('microphone');
    expect(result).toBe('granted');
    expect(useOsPermissionStore.getState().microphone).toBe('granted');
    expect(window.electron.checkPermission).toHaveBeenCalledWith('microphone');
  });

  it('requestOne triggers OS permission prompt', async () => {
    window.electron.requestPermission = vi.fn().mockResolvedValue('granted');

    const result = await useOsPermissionStore.getState().requestOne('camera');
    expect(result).toBe('granted');
    expect(useOsPermissionStore.getState().camera).toBe('granted');
    expect(window.electron.requestPermission).toHaveBeenCalledWith('camera');
  });

  it('fetchAll uses browser-granted notifications when Electron reports not-determined (#1948)', async () => {
    mockBrowserNotification('granted');
    window.electron.checkAllPermissions = vi.fn().mockResolvedValue({
      microphone: 'granted',
      camera: 'granted',
      screen: 'granted',
      secureStorage: 'granted',
      notifications: 'not-determined',
    });

    await useOsPermissionStore.getState().fetchAll();

    expect(useOsPermissionStore.getState().notifications).toBe('granted');
  });

  it('fetchAll preserves concrete Electron notification denial over browser grant (#1948)', async () => {
    mockBrowserNotification('granted');
    window.electron.checkAllPermissions = vi.fn().mockResolvedValue({
      microphone: 'granted',
      camera: 'granted',
      screen: 'granted',
      secureStorage: 'granted',
      notifications: 'denied',
    });

    await useOsPermissionStore.getState().fetchAll();

    expect(useOsPermissionStore.getState().notifications).toBe('denied');
  });

  it('requestOne refreshes notification permission from the browser request result (#1948)', async () => {
    mockBrowserNotification('granted');
    window.electron.requestPermission = vi.fn().mockResolvedValue('not-determined');

    const result = await useOsPermissionStore.getState().requestOne('notifications');

    expect(result).toBe('granted');
    expect(useOsPermissionStore.getState().notifications).toBe('granted');
    expect(globalThis.Notification.requestPermission).toHaveBeenCalled();
  });

  it('requestOne preserves concrete Electron notification denial over browser grant (#1948)', async () => {
    mockBrowserNotification('granted');
    window.electron.requestPermission = vi.fn().mockResolvedValue('denied');

    const result = await useOsPermissionStore.getState().requestOne('notifications');

    expect(result).toBe('denied');
    expect(useOsPermissionStore.getState().notifications).toBe('denied');
    expect(globalThis.Notification.requestPermission).toHaveBeenCalled();
    expect(window.electron.requestPermission).toHaveBeenCalledWith('notifications');
  });

  it('checkOne uses browser notification grant when Electron status is stale (#1948)', async () => {
    mockBrowserNotification('granted');
    window.electron.checkPermission = vi.fn().mockResolvedValue('not-determined');

    const result = await useOsPermissionStore.getState().checkOne('notifications');

    expect(result).toBe('granted');
    expect(useOsPermissionStore.getState().notifications).toBe('granted');
  });

  it('checkOne preserves concrete Electron notification denial over browser grant (#1948)', async () => {
    mockBrowserNotification('granted');
    window.electron.checkPermission = vi.fn().mockResolvedValue('denied');

    const result = await useOsPermissionStore.getState().checkOne('notifications');

    expect(result).toBe('denied');
    expect(useOsPermissionStore.getState().notifications).toBe('denied');
  });

  it('requestOne preserves denied browser notification permission (#1948)', async () => {
    mockBrowserNotification('denied');
    window.electron.requestPermission = vi.fn().mockResolvedValue('granted');

    const result = await useOsPermissionStore.getState().requestOne('notifications');

    expect(result).toBe('denied');
    expect(useOsPermissionStore.getState().notifications).toBe('denied');
    expect(window.electron.requestPermission).not.toHaveBeenCalled();
  });

  it('requestOne falls back to Electron when the browser remains default (#1948)', async () => {
    mockBrowserNotification('default');
    window.electron.requestPermission = vi.fn().mockResolvedValue('granted');

    const result = await useOsPermissionStore.getState().requestOne('notifications');

    expect(result).toBe('granted');
    expect(useOsPermissionStore.getState().notifications).toBe('granted');
    expect(globalThis.Notification.requestPermission).toHaveBeenCalled();
    expect(window.electron.requestPermission).toHaveBeenCalledWith('notifications');
  });

  it('requestOne uses current browser status when notification request throws (#1948)', async () => {
    const requestPermission = vi.fn().mockRejectedValue(new Error('request failed'));
    mockBrowserNotification('denied', requestPermission);
    window.electron.requestPermission = vi.fn().mockResolvedValue('granted');

    const result = await useOsPermissionStore.getState().requestOne('notifications');

    expect(result).toBe('denied');
    expect(useOsPermissionStore.getState().notifications).toBe('denied');
    expect(requestPermission).toHaveBeenCalled();
    expect(window.electron.requestPermission).not.toHaveBeenCalled();
  });

  it('requestOne reads browser status when requestPermission is unavailable (#1948)', async () => {
    mockBrowserNotification('granted');
    Object.assign(globalThis.Notification, { requestPermission: undefined });
    window.electron.requestPermission = vi.fn().mockResolvedValue('not-determined');

    const result = await useOsPermissionStore.getState().requestOne('notifications');

    expect(result).toBe('granted');
    expect(useOsPermissionStore.getState().notifications).toBe('granted');
    expect(globalThis.Notification.requestPermission).toBeUndefined();
    expect(window.electron.requestPermission).toHaveBeenCalledWith('notifications');
  });

  it('updateStatus directly sets a permission', () => {
    useOsPermissionStore.getState().updateStatus('microphone', 'denied');
    expect(useOsPermissionStore.getState().microphone).toBe('denied');
  });

  it('openSettings calls electron API', async () => {
    window.electron.openPermissionSettings = vi.fn().mockResolvedValue(undefined);

    await useOsPermissionStore.getState().openSettings('microphone');
    expect(window.electron.openPermissionSettings).toHaveBeenCalledWith('microphone');
  });

  it('fetchAll logs error and sets isLoaded when checkAllPermissions throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    window.electron.checkAllPermissions = vi.fn().mockRejectedValue(new Error('IPC failure'));

    await useOsPermissionStore.getState().fetchAll();

    expect(consoleSpy).toHaveBeenCalledWith('[osPermissionStore] fetchAll failed:', 'IPC failure');
    expect(useOsPermissionStore.getState().isLoaded).toBe(true);
    consoleSpy.mockRestore();
  });

  it('checkOne logs error and returns current status when checkPermission throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useOsPermissionStore.setState({ microphone: 'denied' });
    window.electron.checkPermission = vi.fn().mockRejectedValue(new Error('check failed'));

    const result = await useOsPermissionStore.getState().checkOne('microphone');

    expect(consoleSpy).toHaveBeenCalledWith(
      '[osPermissionStore] checkOne failed:',
      'microphone',
      'check failed'
    );
    expect(result).toBe('denied');
    consoleSpy.mockRestore();
  });

  it('requestOne logs error and returns current status when requestPermission throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    useOsPermissionStore.setState({ camera: 'not-determined' });
    window.electron.requestPermission = vi.fn().mockRejectedValue(new Error('request failed'));

    const result = await useOsPermissionStore.getState().requestOne('camera');

    expect(consoleSpy).toHaveBeenCalledWith(
      '[osPermissionStore] requestOne failed:',
      'camera',
      'request failed'
    );
    expect(result).toBe('not-determined');
    consoleSpy.mockRestore();
  });

  it('openSettings logs error when openPermissionSettings throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    window.electron.openPermissionSettings = vi
      .fn()
      .mockRejectedValue(new Error('settings failed'));

    await useOsPermissionStore.getState().openSettings('microphone');

    expect(consoleSpy).toHaveBeenCalledWith(
      '[osPermissionStore] openSettings failed:',
      'microphone',
      'settings failed'
    );
    consoleSpy.mockRestore();
  });
});

describe('ensureOsPermission', () => {
  it('returns granted when check returns granted (no request needed)', async () => {
    window.electron.checkPermission = vi.fn().mockResolvedValue('granted');
    window.electron.requestPermission = vi.fn().mockResolvedValue('granted');

    const result = await ensureOsPermission('microphone');
    expect(result).toBe('granted');
    // checkOne always calls checkPermission; requestOne is NOT called since status != not-determined
    expect(window.electron.checkPermission).toHaveBeenCalledWith('microphone');
  });

  it('requests permission if not-determined', async () => {
    window.electron.checkPermission = vi.fn().mockResolvedValue('not-determined');
    window.electron.requestPermission = vi.fn().mockResolvedValue('granted');

    const result = await ensureOsPermission('camera');
    expect(result).toBe('granted');
    expect(window.electron.requestPermission).toHaveBeenCalledWith('camera');
  });
});
