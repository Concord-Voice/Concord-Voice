// @vitest-environment jsdom
import { describe, it, expect, beforeEach, vi } from 'vitest';
import { renderHook } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';
import { useUpdateErrorListener } from '../../../src/renderer/hooks/useUpdateErrorListener';
import { useUpdateStatusStore } from '../../../src/renderer/stores/updateStatusStore';

type UpdateErrorPayload = {
  message: string;
  securityEvent?: boolean;
  subtype?: 'cert-pin-failure' | 'publisher-failure';
};

describe('useUpdateErrorListener (#658)', () => {
  let lastHandler: ((p: UpdateErrorPayload) => void) | null = null;
  let unsubscribeSpy: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    resetAllStores();
    lastHandler = null;
    unsubscribeSpy = vi.fn();
    globalThis.electron = {
      ...(globalThis.electron ?? {}),
      onUpdateError: vi.fn((cb: (p: UpdateErrorPayload) => void) => {
        lastHandler = cb;
        return unsubscribeSpy;
      }),
    } as unknown as typeof globalThis.electron;
  });

  it('routes cert-pin-failure security events to the store', () => {
    renderHook(() => useUpdateErrorListener());
    expect(lastHandler).toBeTruthy();

    lastHandler?.({
      message: 'pin miss',
      securityEvent: true,
      subtype: 'cert-pin-failure',
    });

    const state = useUpdateStatusStore.getState();
    expect(state.criticalError?.subtype).toBe('cert-pin-failure');
    expect(state.criticalError?.message).toBe('pin miss');
  });

  it('routes publisher-failure security events to the store', () => {
    renderHook(() => useUpdateErrorListener());

    lastHandler?.({
      message: 'bad signature',
      securityEvent: true,
      subtype: 'publisher-failure',
    });

    expect(useUpdateStatusStore.getState().criticalError?.subtype).toBe('publisher-failure');
  });

  it('ignores non-security errors (no subtype)', () => {
    renderHook(() => useUpdateErrorListener());

    lastHandler?.({ message: 'network hiccup', securityEvent: false });

    expect(useUpdateStatusStore.getState().criticalError).toBeNull();
  });

  it('ignores security-flagged events with no subtype (defensive)', () => {
    renderHook(() => useUpdateErrorListener());

    lastHandler?.({ message: 'anomaly', securityEvent: true });

    expect(useUpdateStatusStore.getState().criticalError).toBeNull();
  });

  it('unsubscribes on unmount', () => {
    const { unmount } = renderHook(() => useUpdateErrorListener());
    expect(unsubscribeSpy).not.toHaveBeenCalled();
    unmount();
    expect(unsubscribeSpy).toHaveBeenCalledTimes(1);
  });

  it('no-ops when globalThis.electron.onUpdateError is unavailable (dev env)', () => {
    globalThis.electron = undefined as unknown as typeof globalThis.electron;
    expect(() => renderHook(() => useUpdateErrorListener())).not.toThrow();
  });
});
