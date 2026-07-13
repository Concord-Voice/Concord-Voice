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

  it('keeps only the topmost overlay interactive and restores the root when the stack empties', () => {
    const root = document.createElement('div');
    root.id = 'root';
    const outerOverlay = document.createElement('div');
    const innerOverlay = document.createElement('div');
    document.body.append(root, outerOverlay, innerOverlay);
    const provider = renderHook(() => useModalStack(), { wrapper });

    try {
      act(() => provider.result.current.register('outer', 0, outerOverlay));
      expect(root).toHaveAttribute('inert');
      expect(outerOverlay).not.toHaveAttribute('inert');
      expect(outerOverlay).toHaveStyle({ zIndex: '1000' });

      act(() => provider.result.current.register('inner', 1, innerOverlay));
      expect(outerOverlay).toHaveAttribute('inert');
      expect(innerOverlay).not.toHaveAttribute('inert');
      expect(innerOverlay).toHaveStyle({ zIndex: '1001' });

      act(() => provider.result.current.unregister('inner'));
      expect(outerOverlay).not.toHaveAttribute('inert');

      act(() => provider.result.current.unregister('outer'));
      expect(root).not.toHaveAttribute('inert');
    } finally {
      provider.unmount();
      root.remove();
      outerOverlay.remove();
      innerOverlay.remove();
    }
  });

  it('does not clear native-host inert state owned by another provider', () => {
    const firstHost = document.createElement('dialog');
    firstHost.setAttribute('open', '');
    const firstPanel = document.createElement('div');
    const firstOverlay = document.createElement('div');
    firstHost.append(firstPanel, firstOverlay);

    const secondHost = document.createElement('dialog');
    secondHost.setAttribute('open', '');
    const secondPanel = document.createElement('div');
    const secondOverlay = document.createElement('div');
    secondHost.append(secondPanel, secondOverlay);
    document.body.append(firstHost, secondHost);

    const firstProvider = renderHook(() => useModalStack(), { wrapper });
    const secondProvider = renderHook(() => useModalStack(), { wrapper });

    try {
      act(() => firstProvider.result.current.register('first', 0, firstOverlay));
      expect(firstPanel).toHaveAttribute('inert');

      act(() => secondProvider.result.current.register('second', 0, secondOverlay));

      expect(firstPanel).toHaveAttribute('inert');
      expect(secondPanel).toHaveAttribute('inert');

      act(() => secondProvider.result.current.unregister('second'));
      expect(firstPanel).toHaveAttribute('inert');
      expect(secondPanel).not.toHaveAttribute('inert');
    } finally {
      firstProvider.unmount();
      secondProvider.unmount();
      firstHost.remove();
      secondHost.remove();
    }
  });
});
