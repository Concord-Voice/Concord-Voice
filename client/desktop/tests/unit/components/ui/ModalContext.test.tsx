import { renderHook, act } from '@testing-library/react';
import React from 'react';
import { ModalProvider, useModalStack } from '@/renderer/components/ui/ModalContext';

function wrapper({ children }: { children: React.ReactNode }) {
  return <ModalProvider>{children}</ModalProvider>;
}

describe('ModalContext', () => {
  it('throws when used outside ModalProvider', () => {
    expect(() => {
      renderHook(() => useModalStack());
    }).toThrow('useModalStack must be used within a ModalProvider');
  });

  it('registers a modal as topmost', () => {
    const { result } = renderHook(() => useModalStack(), { wrapper });

    act(() => result.current.register('modal-1', 0));

    expect(result.current.isTopmost('modal-1')).toBe(true);
  });

  it('deeper modal is topmost regardless of registration order', () => {
    const { result } = renderHook(() => useModalStack(), { wrapper });

    act(() => {
      result.current.register('inner', 1);
      result.current.register('outer', 0);
    });

    expect(result.current.isTopmost('inner')).toBe(true);
    expect(result.current.isTopmost('outer')).toBe(false);
  });

  it('among same depth, last registered is topmost', () => {
    const { result } = renderHook(() => useModalStack(), { wrapper });

    act(() => {
      result.current.register('modal-a', 0);
      result.current.register('modal-b', 0);
    });

    expect(result.current.isTopmost('modal-a')).toBe(false);
    expect(result.current.isTopmost('modal-b')).toBe(true);
  });

  it('promotes the previous modal when topmost is unregistered', () => {
    const { result } = renderHook(() => useModalStack(), { wrapper });

    act(() => {
      result.current.register('outer', 0);
      result.current.register('inner', 1);
    });

    act(() => result.current.unregister('inner'));

    expect(result.current.isTopmost('outer')).toBe(true);
    expect(result.current.isTopmost('inner')).toBe(false);
  });

  it('returns false for isTopmost when stack is empty', () => {
    const { result } = renderHook(() => useModalStack(), { wrapper });

    expect(result.current.isTopmost('nonexistent')).toBe(false);
  });

  it('handles unregistering a non-existent id gracefully', () => {
    const { result } = renderHook(() => useModalStack(), { wrapper });

    act(() => result.current.register('modal-1', 0));
    act(() => result.current.unregister('nonexistent'));

    expect(result.current.isTopmost('modal-1')).toBe(true);
  });
});
