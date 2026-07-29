import React from 'react';
import { act, renderHook } from '@testing-library/react';
import { useResizablePanel } from '@/renderer/hooks/useResizablePanel';
import { resetAllStores } from '../../helpers/store-helpers';

describe('useResizablePanel', () => {
  beforeEach(() => {
    resetAllStores();
    localStorage.clear();
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('keeps drag width transient, commits once, and follows controlled width while idle', () => {
    const onWidthCommit = vi.fn();
    const storageSpy = vi.spyOn(Storage.prototype, 'setItem');
    const { result, rerender } = renderHook(
      ({ width }) =>
        useResizablePanel({
          width,
          minWidth: 56,
          maxWidth: 400,
          side: 'left',
          onWidthCommit,
        }),
      { initialProps: { width: 240 } }
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: vi.fn(),
      } as unknown as React.MouseEvent);
    });
    expect(result.current.isResizing).toBe(true);

    act(() => window.dispatchEvent(new MouseEvent('mousemove', { clientX: 300 })));
    expect(result.current.width).toBe(300);
    expect(onWidthCommit).not.toHaveBeenCalled();

    act(() => window.dispatchEvent(new MouseEvent('mouseup')));
    expect(onWidthCommit).toHaveBeenCalledTimes(1);
    expect(onWidthCommit).toHaveBeenCalledWith(300);
    expect(result.current.isResizing).toBe(false);

    act(() => window.dispatchEvent(new MouseEvent('mouseup')));
    expect(onWidthCommit).toHaveBeenCalledTimes(1);

    rerender({ width: 180 });
    expect(result.current.width).toBe(180);
    expect(storageSpy).not.toHaveBeenCalled();
  });

  it('inverts horizontal drag direction for a right-side panel', () => {
    const onWidthCommit = vi.fn();
    const { result } = renderHook(() =>
      useResizablePanel({
        width: 240,
        minWidth: 56,
        maxWidth: 340,
        side: 'right',
        onWidthCommit,
      })
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: vi.fn(),
      } as unknown as React.MouseEvent);
    });
    act(() => window.dispatchEvent(new MouseEvent('mousemove', { clientX: 180 })));

    expect(result.current.width).toBe(300);

    act(() => window.dispatchEvent(new MouseEvent('mouseup')));
    expect(onWidthCommit).toHaveBeenCalledWith(300);
  });

  it('clamps transient drag width to both bounds', () => {
    const onWidthCommit = vi.fn();
    const { result } = renderHook(() =>
      useResizablePanel({
        width: 240,
        minWidth: 56,
        maxWidth: 400,
        side: 'left',
        onWidthCommit,
      })
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: vi.fn(),
      } as unknown as React.MouseEvent);
    });
    act(() => window.dispatchEvent(new MouseEvent('mousemove', { clientX: 800 })));
    expect(result.current.width).toBe(400);

    act(() => window.dispatchEvent(new MouseEvent('mousemove', { clientX: -100 })));
    expect(result.current.width).toBe(56);

    act(() => window.dispatchEvent(new MouseEvent('mouseup')));
    expect(onWidthCommit).toHaveBeenCalledWith(56);
  });

  it('commits normal and Shift+Arrow keyboard steps immediately', () => {
    const onWidthCommit = vi.fn();
    const { result, rerender } = renderHook(
      ({ width }) =>
        useResizablePanel({
          width,
          minWidth: 56,
          maxWidth: 400,
          side: 'left',
          onWidthCommit,
        }),
      { initialProps: { width: 240 } }
    );
    const normalPreventDefault = vi.fn();

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: false,
        preventDefault: normalPreventDefault,
      } as unknown as React.KeyboardEvent);
    });
    expect(normalPreventDefault).toHaveBeenCalledTimes(1);
    expect(onWidthCommit).toHaveBeenLastCalledWith(250);

    rerender({ width: 250 });
    act(() => {
      result.current.onKeyDown({
        key: 'ArrowLeft',
        shiftKey: true,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });
    expect(onWidthCommit).toHaveBeenLastCalledWith(200);
    expect(onWidthCommit).toHaveBeenCalledTimes(2);
  });

  it('inverts keyboard direction on the right and clamps commits', () => {
    const onWidthCommit = vi.fn();
    const { result, rerender } = renderHook(
      ({ width }) =>
        useResizablePanel({
          width,
          minWidth: 56,
          maxWidth: 340,
          side: 'right',
          onWidthCommit,
        }),
      { initialProps: { width: 60 } }
    );

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: false,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });
    expect(onWidthCommit).toHaveBeenLastCalledWith(56);

    rerender({ width: 330 });
    act(() => {
      result.current.onKeyDown({
        key: 'ArrowLeft',
        shiftKey: true,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });
    expect(onWidthCommit).toHaveBeenLastCalledWith(340);
  });

  it('ignores drag and keyboard input while disabled', () => {
    const onWidthCommit = vi.fn();
    const mousePreventDefault = vi.fn();
    const keyPreventDefault = vi.fn();
    const { result } = renderHook(() =>
      useResizablePanel({
        width: 240,
        minWidth: 56,
        maxWidth: 400,
        side: 'left',
        disabled: true,
        onWidthCommit,
      })
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: mousePreventDefault,
      } as unknown as React.MouseEvent);
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: false,
        preventDefault: keyPreventDefault,
      } as unknown as React.KeyboardEvent);
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 300 }));
      window.dispatchEvent(new MouseEvent('mouseup'));
    });

    expect(mousePreventDefault).not.toHaveBeenCalled();
    expect(keyPreventDefault).not.toHaveBeenCalled();
    expect(result.current.width).toBe(240);
    expect(result.current.isResizing).toBe(false);
    expect(onWidthCommit).not.toHaveBeenCalled();
  });

  it('keeps the active draft when the controlled width changes during a drag', () => {
    const onWidthCommit = vi.fn();
    const { result, rerender } = renderHook(
      ({ width }) =>
        useResizablePanel({
          width,
          minWidth: 56,
          maxWidth: 400,
          side: 'left',
          onWidthCommit,
        }),
      { initialProps: { width: 240 } }
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: vi.fn(),
      } as unknown as React.MouseEvent);
    });
    act(() => window.dispatchEvent(new MouseEvent('mousemove', { clientX: 300 })));

    rerender({ width: 180 });
    expect(result.current.width).toBe(300);

    act(() => window.dispatchEvent(new MouseEvent('mouseup')));
    expect(onWidthCommit).toHaveBeenCalledWith(300);
    expect(result.current.width).toBe(180);

    rerender({ width: 210 });
    expect(result.current.width).toBe(210);
  });

  it('uses the latest commit callback when it changes during a drag', () => {
    const firstCommit = vi.fn();
    const latestCommit = vi.fn();
    const { result, rerender } = renderHook(
      ({ onWidthCommit }) =>
        useResizablePanel({
          width: 240,
          minWidth: 56,
          maxWidth: 400,
          side: 'left',
          onWidthCommit,
        }),
      { initialProps: { onWidthCommit: firstCommit } }
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: vi.fn(),
      } as unknown as React.MouseEvent);
    });
    act(() => window.dispatchEvent(new MouseEvent('mousemove', { clientX: 300 })));
    rerender({ onWidthCommit: latestCommit });
    act(() => window.dispatchEvent(new MouseEvent('mouseup')));

    expect(firstCommit).not.toHaveBeenCalled();
    expect(latestCommit).toHaveBeenCalledWith(300);
  });

  it('restores body styles and removes active listeners on unmount', () => {
    const onWidthCommit = vi.fn();
    document.body.style.cursor = 'wait';
    document.body.style.userSelect = 'text';
    const { result, unmount } = renderHook(() =>
      useResizablePanel({
        width: 240,
        minWidth: 56,
        maxWidth: 400,
        side: 'left',
        onWidthCommit,
      })
    );

    act(() => {
      result.current.onMouseDown({
        clientX: 240,
        preventDefault: vi.fn(),
      } as unknown as React.MouseEvent);
    });
    expect(document.body.style.cursor).toBe('col-resize');
    expect(document.body.style.userSelect).toBe('none');

    unmount();
    expect(document.body.style.cursor).toBe('wait');
    expect(document.body.style.userSelect).toBe('text');

    act(() => {
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 300 }));
      window.dispatchEvent(new MouseEvent('mouseup'));
    });
    expect(onWidthCommit).not.toHaveBeenCalled();
  });

  it('ignores non-arrow keyboard input', () => {
    const onWidthCommit = vi.fn();
    const preventDefault = vi.fn();
    const { result } = renderHook(() =>
      useResizablePanel({
        width: 240,
        minWidth: 56,
        maxWidth: 400,
        side: 'left',
        onWidthCommit,
      })
    );

    act(() => {
      result.current.onKeyDown({
        key: 'Enter',
        shiftKey: false,
        preventDefault,
      } as unknown as React.KeyboardEvent);
    });

    expect(preventDefault).not.toHaveBeenCalled();
    expect(onWidthCommit).not.toHaveBeenCalled();
  });
});
