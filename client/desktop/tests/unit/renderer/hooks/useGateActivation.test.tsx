import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useGateActivation } from '@/renderer/hooks/useGateActivation';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

beforeEach(() => {
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('useGateActivation', () => {
  it('returns a stable describedById', () => {
    const { result, rerender } = renderHook(() => useGateActivation());
    const first = result.current.describedById;
    expect(first).toMatch(/^premium-chip-/);
    rerender();
    expect(result.current.describedById).toBe(first);
  });

  it('routes a click (no event) to the Subscription page', () => {
    const { result } = renderHook(() => useGateActivation('audio-tier'));
    act(() => result.current.onActivate());
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('routes Enter to the Subscription page and prevents default', () => {
    const { result } = renderHook(() => useGateActivation());
    let prevented = false;
    act(() =>
      result.current.onActivate({
        key: 'Enter',
        preventDefault: () => {
          prevented = true;
        },
      } as unknown as React.KeyboardEvent)
    );
    expect(prevented).toBe(true);
    expect(useSettingsNavStore.getState().focusRequest).not.toBeNull();
  });

  it('ignores non-activation keys', () => {
    const { result } = renderHook(() => useGateActivation());
    act(() =>
      result.current.onActivate({
        key: 'Tab',
        preventDefault: () => {},
      } as unknown as React.KeyboardEvent)
    );
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
});
