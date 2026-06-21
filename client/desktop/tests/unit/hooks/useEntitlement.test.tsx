import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useEntitlement } from '../../../src/renderer/hooks/useEntitlement';
import {
  useSubscriptionStore,
  FREE_ENTITLEMENT,
} from '../../../src/renderer/stores/subscriptionStore';

describe('useEntitlement', () => {
  beforeEach(() => {
    useSubscriptionStore.setState({ entitlement: FREE_ENTITLEMENT, degraded: false });
  });

  it('returns the selected field', () => {
    const { result } = renderHook(() => useEntitlement((e) => e.maxMessageChars));
    expect(result.current).toBe(5120);
  });

  it('re-renders when the entitlement changes', () => {
    const { result } = renderHook(() => useEntitlement((e) => e.allowMusicMode));
    expect(result.current).toBe(false);
    act(() => {
      useSubscriptionStore.getState().setEntitlement({ ...FREE_ENTITLEMENT, allowMusicMode: true });
    });
    expect(result.current).toBe(true);
  });
});
