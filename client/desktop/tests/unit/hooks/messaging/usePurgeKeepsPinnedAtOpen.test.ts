import { describe, it, expect, beforeEach, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { usePurgeKeepsPinnedAtOpen } from '@/renderer/hooks/messaging/usePurgeKeepsPinnedAtOpen';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';
import { resetAllStores } from '../../../helpers/store-helpers';

// Each call stores a new object, which is what a capability answer does.
function answer(keepsPinned: boolean): void {
  useClientConfigStore.setState({
    serverCapabilities: {
      auth: { oauthProviders: [] },
      features: { purgeKeepsPinned: keepsPinned },
    },
  });
}

// A failed capability fetch stores null (setCapabilityError).
function failCapabilityFetch(): void {
  useClientConfigStore.setState({ serverCapabilities: null });
}

// #3458: a dialog samples the capability once per open, so a refresh that
// lands while it is open cannot change what it offers or sends, except a
// server that answers without the capability, which would ignore the choice.
// #3552: the sample is offered only once an answer after the open confirms it.
describe('usePurgeKeepsPinnedAtOpen', () => {
  let refresh: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    resetAllStores();
    answer(false);
    refresh = vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockResolvedValue();
    refresh.mockClear();
  });

  const openHook = (open: boolean) =>
    renderHook(({ isOpen }) => usePurgeKeepsPinnedAtOpen(isOpen), {
      initialProps: { isOpen: open },
    });

  // A cached value can predate a rollback, and a rolled-back server deletes
  // pins it was asked to keep (#3552 review).
  it('makes no claim until an answer after the open confirms the sample', () => {
    answer(true);
    const { result } = openHook(true);
    expect(result.current).toBe(false);

    act(() => answer(true));
    expect(result.current).toBe(true);
  });

  it('stays without the claim when the open refresh fails', () => {
    answer(true);
    const { result } = openHook(true);
    act(() => failCapabilityFetch());
    expect(result.current).toBe(false);
  });

  it('holds a confirmed value through a later failed fetch', () => {
    const { result, rerender } = openHook(false);
    act(() => answer(true));
    rerender({ isOpen: true });
    act(() => answer(true));
    expect(result.current).toBe(true);

    act(() => failCapabilityFetch());
    expect(result.current).toBe(true);
  });

  it('drops to false when the server answers without the capability', () => {
    answer(true);
    const { result } = openHook(true);
    act(() => answer(true));
    expect(result.current).toBe(true);

    act(() => answer(false));
    expect(result.current).toBe(false);
  });

  it('a withdrawal holds until close, even through a later failed fetch', () => {
    answer(true);
    const { result, rerender } = openHook(true);
    act(() => answer(false));
    act(() => failCapabilityFetch());
    expect(result.current).toBe(false);

    rerender({ isOpen: false });
    act(() => answer(true));
    rerender({ isOpen: true });
    act(() => answer(true));
    expect(result.current).toBe(true);
  });

  it('refreshes the capability once each time the dialog opens', () => {
    const { rerender } = openHook(false);
    expect(refresh).not.toHaveBeenCalled();

    rerender({ isOpen: true });
    rerender({ isOpen: true });
    expect(refresh).toHaveBeenCalledTimes(1);

    rerender({ isOpen: false });
    rerender({ isOpen: true });
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('does not offer a capability that appears while open', () => {
    const { result } = openHook(true);
    act(() => answer(true));
    expect(result.current).toBe(false);
  });

  it('samples afresh on the next open', () => {
    const { result, rerender } = openHook(true);
    expect(result.current).toBe(false);

    rerender({ isOpen: false });
    act(() => answer(true));
    rerender({ isOpen: true });
    act(() => answer(true));
    expect(result.current).toBe(true);
  });
});
