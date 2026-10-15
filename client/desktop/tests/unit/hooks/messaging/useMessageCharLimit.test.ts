import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useMessageCharLimit } from '@/renderer/hooks/messaging/useMessageCharLimit';
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { resetAllStores } from '../../../helpers/store-helpers';

const premium = (maxMessageChars = 10240) => ({
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  maxMessageChars,
});

describe('useMessageCharLimit', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('returns the free limit (5120) by default', () => {
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(5120);
  });

  it('returns the premium limit (10240) for a premium entitlement', () => {
    useSubscriptionStore.getState().setEntitlement(premium());
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(10240);
  });

  it('clamps an inflated value to the free ceiling', () => {
    useSubscriptionStore.getState().setEntitlement({ ...FREE_ENTITLEMENT, maxMessageChars: 99999 });
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(5120);
  });

  it('clamps an inflated value to the premium ceiling', () => {
    useSubscriptionStore.getState().setEntitlement(premium(50000));
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(10240);
  });

  it('honours a tier value below its ceiling', () => {
    useSubscriptionStore.getState().setEntitlement(premium(7000));
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(7000);
  });

  it('falls back to the tier ceiling for a non-finite value', () => {
    useSubscriptionStore
      .getState()
      .setEntitlement({ ...FREE_ENTITLEMENT, maxMessageChars: Number.NaN });
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(5120);
  });

  it('follows a tier change without a remount (premium -> free)', () => {
    useSubscriptionStore.getState().setEntitlement(premium());
    const { result } = renderHook(() => useMessageCharLimit());
    expect(result.current).toBe(10240);

    act(() => {
      useSubscriptionStore.getState().setEntitlement(FREE_ENTITLEMENT);
    });
    expect(result.current).toBe(5120);

    act(() => {
      useSubscriptionStore.getState().setEntitlement(premium());
    });
    expect(result.current).toBe(10240);
  });
});
