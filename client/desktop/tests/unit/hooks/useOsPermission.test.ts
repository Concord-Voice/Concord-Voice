import { act, renderHook, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { useOsPermissionStore, type OsPermissionStatus, type OsPermissionType } from '@/renderer/stores/osPermissionStore';
import { useOsPermission } from '@/renderer/hooks/useOsPermission';
import { resetAllStores } from '../../helpers/store-helpers';

// Reset store state before each test
beforeEach(() => {
  resetAllStores();
  useOsPermissionStore.setState({
    microphone: 'not-determined',
    camera: 'not-determined',
    screen: 'not-determined',
    secureStorage: 'not-determined',
    notifications: 'not-determined',
    isLoaded: false,
  });
});

// ─── Status derivation ────────────────────────────────────────────────────────

describe('useOsPermission — status and derived flags', () => {
  it('reflects the current status from the store', () => {
    useOsPermissionStore.setState({ microphone: 'granted' });
    const { result } = renderHook(() => useOsPermission('microphone'));

    expect(result.current.status).toBe('granted');
  });

  it('isGranted is true when status is "granted"', () => {
    useOsPermissionStore.setState({ microphone: 'granted' });
    const { result } = renderHook(() => useOsPermission('microphone'));
    expect(result.current.isGranted).toBe(true);
  });

  it('isGranted is false when status is "not-determined"', () => {
    const { result } = renderHook(() => useOsPermission('microphone'));
    expect(result.current.isGranted).toBe(false);
  });

  it('isDenied is true when status is "denied"', () => {
    useOsPermissionStore.setState({ camera: 'denied' });
    const { result } = renderHook(() => useOsPermission('camera'));
    expect(result.current.isDenied).toBe(true);
  });

  it('isDenied is true when status is "restricted"', () => {
    useOsPermissionStore.setState({ screen: 'restricted' });
    const { result } = renderHook(() => useOsPermission('screen'));
    expect(result.current.isDenied).toBe(true);
  });

  it('isDenied is false when status is "not-determined"', () => {
    const { result } = renderHook(() => useOsPermission('notifications'));
    expect(result.current.isDenied).toBe(false);
  });

  it('isChecking starts as false', () => {
    const { result } = renderHook(() => useOsPermission('microphone'));
    expect(result.current.isChecking).toBe(false);
  });
});

// ─── Works for all permission types ──────────────────────────────────────────

describe('useOsPermission — all permission types', () => {
  const types: OsPermissionType[] = [
    'microphone',
    'camera',
    'screen',
    'secureStorage',
    'notifications',
  ];
  const statuses: OsPermissionStatus[] = [
    'granted',
    'denied',
    'not-determined',
    'restricted',
    'unavailable',
  ];

  for (const type of types) {
    it(`reads ${type} status correctly`, () => {
      useOsPermissionStore.setState({ [type]: 'granted' });
      const { result } = renderHook(() => useOsPermission(type));
      expect(result.current.status).toBe('granted');
      expect(result.current.isGranted).toBe(true);
    });
  }

  for (const status of statuses) {
    it(`isGranted is ${status === 'granted'} for status "${status}"`, () => {
      useOsPermissionStore.setState({ microphone: status });
      const { result } = renderHook(() => useOsPermission('microphone'));
      expect(result.current.isGranted).toBe(status === 'granted');
    });
  }
});

// ─── check() ─────────────────────────────────────────────────────────────────

describe('useOsPermission — check()', () => {
  it('sets isChecking to true during the check', async () => {
    let resolveCheck!: (v: string) => void;
    const checkPromise = new Promise<string>((res) => {
      resolveCheck = res;
    });

    // Override the store's checkOne to return our controlled promise
    useOsPermissionStore.setState({
      checkOne: vi
        .fn()
        .mockReturnValue(checkPromise) as typeof useOsPermissionStore.getState extends () => infer S
        ? S extends { checkOne: infer C }
          ? C
          : never
        : never,
    } as Parameters<typeof useOsPermissionStore.setState>[0]);

    const { result } = renderHook(() => useOsPermission('microphone'));

    // Start the check
    act(() => {
      void result.current.check();
    });
    expect(result.current.isChecking).toBe(true);

    // Resolve the promise
    await act(async () => {
      resolveCheck('granted');
      await checkPromise;
    });
    expect(result.current.isChecking).toBe(false);
  });

  it('calls checkOne with the correct permission type', async () => {
    const mockCheckOne = vi.fn().mockResolvedValue('granted' as OsPermissionStatus);
    useOsPermissionStore.setState({ checkOne: mockCheckOne } as Parameters<
      typeof useOsPermissionStore.setState
    >[0]);

    const { result } = renderHook(() => useOsPermission('camera'));
    await act(async () => {
      await result.current.check();
    });

    expect(mockCheckOne).toHaveBeenCalledWith('camera');
  });

  it('returns the status from checkOne', async () => {
    useOsPermissionStore.setState({
      checkOne: vi.fn().mockResolvedValue('denied' as OsPermissionStatus),
    } as Parameters<typeof useOsPermissionStore.setState>[0]);

    const { result } = renderHook(() => useOsPermission('microphone'));
    let returnedStatus!: OsPermissionStatus;
    await act(async () => {
      returnedStatus = await result.current.check();
    });

    expect(returnedStatus).toBe('denied');
  });
});

// ─── request() ───────────────────────────────────────────────────────────────

describe('useOsPermission — request()', () => {
  it('calls requestOne with the correct permission type', async () => {
    const mockRequestOne = vi.fn().mockResolvedValue('granted' as OsPermissionStatus);
    useOsPermissionStore.setState({ requestOne: mockRequestOne } as Parameters<
      typeof useOsPermissionStore.setState
    >[0]);

    const { result } = renderHook(() => useOsPermission('notifications'));
    await act(async () => {
      await result.current.request();
    });

    expect(mockRequestOne).toHaveBeenCalledWith('notifications');
  });

  it('returns the status from requestOne', async () => {
    useOsPermissionStore.setState({
      requestOne: vi.fn().mockResolvedValue('granted' as OsPermissionStatus),
    } as Parameters<typeof useOsPermissionStore.setState>[0]);

    const { result } = renderHook(() => useOsPermission('screen'));
    let returnedStatus!: OsPermissionStatus;
    await act(async () => {
      returnedStatus = await result.current.request();
    });

    expect(returnedStatus).toBe('granted');
  });

  it('sets isChecking to false after request resolves', async () => {
    useOsPermissionStore.setState({
      requestOne: vi.fn().mockResolvedValue('granted' as OsPermissionStatus),
    } as Parameters<typeof useOsPermissionStore.setState>[0]);

    const { result } = renderHook(() => useOsPermission('microphone'));
    await act(async () => {
      await result.current.request();
    });

    expect(result.current.isChecking).toBe(false);
  });
});

// ─── openSettings() ──────────────────────────────────────────────────────────

describe('useOsPermission — openSettings()', () => {
  it('calls openSettings action with the correct type', async () => {
    const mockOpenSettings = vi.fn().mockResolvedValue(undefined);
    useOsPermissionStore.setState({ openSettings: mockOpenSettings } as Parameters<
      typeof useOsPermissionStore.setState
    >[0]);

    const { result } = renderHook(() => useOsPermission('microphone'));
    await act(async () => {
      await result.current.openSettings();
    });

    expect(mockOpenSettings).toHaveBeenCalledWith('microphone');
  });
});

// ─── reactivity ──────────────────────────────────────────────────────────────

describe('useOsPermission — reactivity', () => {
  it('updates when the store status changes', async () => {
    useOsPermissionStore.setState({ microphone: 'not-determined' });
    const { result } = renderHook(() => useOsPermission('microphone'));

    expect(result.current.isGranted).toBe(false);

    act(() => {
      useOsPermissionStore.setState({ microphone: 'granted' });
    });

    await waitFor(() => expect(result.current.isGranted).toBe(true));
  });
});
