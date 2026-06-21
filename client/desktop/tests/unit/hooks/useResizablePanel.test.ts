import React from 'react';
import { renderHook, act } from '@testing-library/react';
import { useResizablePanel } from '@/renderer/hooks/useResizablePanel';

describe('useResizablePanel', () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it('returns default width when no stored value', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );
    expect(result.current.width).toBe(250);
  });

  it('returns stored width from localStorage', () => {
    localStorage.setItem('panel-width', '300');
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
        storageKey: 'panel-width',
      })
    );
    expect(result.current.width).toBe(300);
  });

  it('ignores stored value outside min/max range', () => {
    localStorage.setItem('panel-width', '100');
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
        storageKey: 'panel-width',
      })
    );
    expect(result.current.width).toBe(250);
  });

  it('ignores invalid stored value', () => {
    localStorage.setItem('panel-width', 'not-a-number');
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
        storageKey: 'panel-width',
      })
    );
    expect(result.current.width).toBe(250);
  });

  it('provides onMouseDown handler', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );
    expect(result.current.onMouseDown).toBeInstanceOf(Function);
  });

  it('resizes on drag for left side panel', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    // Simulate mouse down
    act(() => {
      result.current.onMouseDown({
        preventDefault: vi.fn(),
        clientX: 250,
      } as unknown as React.MouseEvent);
    });

    // Simulate mouse move
    act(() => {
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 300 }));
    });

    expect(result.current.width).toBe(300); // 250 + (300 - 250) = 300

    // Simulate mouse up
    act(() => {
      window.dispatchEvent(new MouseEvent('mouseup'));
    });
  });

  it('resizes on drag for right side panel (delta is inverted)', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'right',
      })
    );

    act(() => {
      result.current.onMouseDown({
        preventDefault: vi.fn(),
        clientX: 250,
      } as unknown as React.MouseEvent);
    });

    // Moving mouse left for right-side panel = increase width
    act(() => {
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 200 }));
    });

    expect(result.current.width).toBe(300); // 250 - (200 - 250) = 300

    act(() => {
      window.dispatchEvent(new MouseEvent('mouseup'));
    });
  });

  it('clamps width to min/max bounds', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    act(() => {
      result.current.onMouseDown({
        preventDefault: vi.fn(),
        clientX: 250,
      } as unknown as React.MouseEvent);
    });

    // Try to exceed max
    act(() => {
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 700 }));
    });

    expect(result.current.width).toBe(400); // clamped to max

    act(() => {
      window.dispatchEvent(new MouseEvent('mouseup'));
    });
  });

  it('provides onKeyDown handler', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );
    expect(result.current.onKeyDown).toBeInstanceOf(Function);
  });

  it('ArrowRight increases width for left side panel', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: false,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(result.current.width).toBe(260);
  });

  it('ArrowLeft decreases width for left side panel', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowLeft',
        shiftKey: false,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(result.current.width).toBe(240);
  });

  it('ArrowRight decreases width for right side panel', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'right',
      })
    );

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: false,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(result.current.width).toBe(240);
  });

  it('Shift+ArrowRight increases width by 50px for left side panel', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: true,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(result.current.width).toBe(300);
  });

  it('clamps keyboard resize to min/max', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 395,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    // Try to go past max
    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: true,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(result.current.width).toBe(400);

    // Now test min clamping with a fresh hook
    const { result: result2 } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 205,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
      })
    );

    act(() => {
      result2.current.onKeyDown({
        key: 'ArrowLeft',
        shiftKey: true,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(result2.current.width).toBe(200);
  });

  it('persists width to localStorage on keyboard resize', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
        storageKey: 'panel-width',
      })
    );

    act(() => {
      result.current.onKeyDown({
        key: 'ArrowRight',
        shiftKey: false,
        preventDefault: vi.fn(),
      } as unknown as React.KeyboardEvent);
    });

    expect(localStorage.getItem('panel-width')).toBe('260');
  });

  it('persists width to localStorage on mouse up', () => {
    const { result } = renderHook(() =>
      useResizablePanel({
        defaultWidth: 250,
        minWidth: 200,
        maxWidth: 400,
        side: 'left',
        storageKey: 'panel-width',
      })
    );

    act(() => {
      result.current.onMouseDown({
        preventDefault: vi.fn(),
        clientX: 250,
      } as unknown as React.MouseEvent);
    });

    act(() => {
      window.dispatchEvent(new MouseEvent('mousemove', { clientX: 280 }));
    });

    act(() => {
      window.dispatchEvent(new MouseEvent('mouseup'));
    });

    expect(localStorage.getItem('panel-width')).toBe('280');
  });
});
