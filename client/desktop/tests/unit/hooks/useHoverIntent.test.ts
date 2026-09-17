import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useHoverIntent } from '../../../src/renderer/hooks/ui/useHoverIntent';

describe('useHoverIntent', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  const advance = (ms: number) => act(() => void vi.advanceTimersByTime(ms));

  it('starts cold', () => {
    const { result } = renderHook(() => useHoverIntent());
    expect(result.current.warm).toBe(false);
    expect(result.current.groupProps['data-warm']).toBe('false');
  });

  // The behaviour this hook exists for: a pointer crossing the row must arm nothing.
  it('does NOT warm when the pointer passes straight through', () => {
    const { result } = renderHook(() => useHoverIntent({ warmUpMs: 450 }));
    act(() => result.current.groupProps.onPointerEnter());
    advance(200); // still inside the gate
    act(() => result.current.groupProps.onPointerLeave());
    advance(2000); // let every timer drain
    expect(result.current.warm).toBe(false);
  });

  it('warms once the pointer has dwelt in the row', () => {
    const { result } = renderHook(() => useHoverIntent({ warmUpMs: 450 }));
    act(() => result.current.groupProps.onPointerEnter());
    advance(449);
    expect(result.current.warm).toBe(false); // not a millisecond early
    advance(1);
    expect(result.current.warm).toBe(true);
    expect(result.current.groupProps['data-warm']).toBe('true');
  });

  it('stays warm across a brief exit, so travel within the row is not re-gated', () => {
    const { result } = renderHook(() => useHoverIntent({ warmUpMs: 450, coolDownMs: 600 }));
    act(() => result.current.groupProps.onPointerEnter());
    advance(500);
    expect(result.current.warm).toBe(true);

    act(() => result.current.groupProps.onPointerLeave());
    advance(300); // inside the grace window
    act(() => result.current.groupProps.onPointerEnter());
    advance(300); // would have been past cool-down had re-entry not cancelled it
    expect(result.current.warm).toBe(true);
  });

  it('cools down after the pointer stays away', () => {
    const { result } = renderHook(() => useHoverIntent({ warmUpMs: 450, coolDownMs: 600 }));
    act(() => result.current.groupProps.onPointerEnter());
    advance(500);
    expect(result.current.warm).toBe(true);

    act(() => result.current.groupProps.onPointerLeave());
    advance(599);
    expect(result.current.warm).toBe(true); // grace still holding
    advance(1);
    expect(result.current.warm).toBe(false);
  });

  // The header unmounts on every channel/conversation switch, so a live timer here is
  // routine rather than exotic.
  //
  // Asserts the TIMER IS GONE, not that advancing the clock does not throw. React 19 does
  // not throw — nor even warn — on a setState after unmount, so the throw-based assertion
  // this replaced passed identically with the cleanup effect deleted. The toBe(1) below is
  // the control: without it, toBe(0) would also pass against a hook that never armed one.
  it('cancels a pending timer on unmount', () => {
    const { result, unmount } = renderHook(() => useHoverIntent({ warmUpMs: 450 }));
    act(() => result.current.groupProps.onPointerEnter());
    expect(vi.getTimerCount()).toBe(1);
    unmount();
    expect(vi.getTimerCount()).toBe(0);
  });
});
