import { renderHook, act, waitFor } from '@testing-library/react';
import { useRef, useEffect } from 'react';
import { useExpandCollapseAll } from '@/renderer/hooks/useExpandCollapseAll';

/**
 * Test harness: render the hook against a container DOM node we control,
 * pre-populated with N <details.settings-collapsible> children. Returns
 * both the hook result and the container itself for direct DOM mutation.
 */
function setupHookWithContainer(initialOpenStates: boolean[]) {
  const container = document.createElement('div');
  document.body.appendChild(container);
  for (let i = 0; i < initialOpenStates.length; i++) {
    const details = document.createElement('details');
    details.className = 'settings-collapsible';
    details.id = `section-${i}`;
    if (initialOpenStates[i]) details.open = true;
    container.appendChild(details);
  }

  const hookResult = renderHook(() => {
    const ref = useRef<HTMLElement | null>(null);
    // Reassign the ref to our pre-built container after mount so the hook's
    // useEffect sees a non-null container on its first run.
    useEffect(() => {
      ref.current = container;
    }, []);
    return useExpandCollapseAll(ref);
  });

  return {
    container,
    result: hookResult.result,
    rerender: hookResult.rerender,
    cleanup: () => {
      hookResult.unmount();
      container.remove();
    },
  };
}

describe('useExpandCollapseAll', () => {
  it('reports zero sections when the container is empty', async () => {
    const { result, cleanup } = setupHookWithContainer([]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(false);
    });
    expect(result.current.allExpanded).toBe(false);
    cleanup();
  });

  it('reports allExpanded when every section is open', async () => {
    const { result, cleanup } = setupHookWithContainer([true, true, true]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    expect(result.current.allExpanded).toBe(true);
    cleanup();
  });

  it('reports allExpanded=false when every section is closed', async () => {
    const { result, cleanup } = setupHookWithContainer([false, false, false]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    expect(result.current.allExpanded).toBe(false);
    cleanup();
  });

  it('reports allExpanded=false in mixed state', async () => {
    const { result, cleanup } = setupHookWithContainer([true, false, true]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    expect(result.current.allExpanded).toBe(false);
    cleanup();
  });

  it('toggle expands when mixed-state (some closed)', async () => {
    // Per #297 spec: mixed state defaults to "expand all".
    const { container, result, cleanup } = setupHookWithContainer([true, false, true]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    act(() => {
      result.current.toggle();
    });
    const sections = container.querySelectorAll<HTMLDetailsElement>('details.settings-collapsible');
    expect([...sections].every((s) => s.open)).toBe(true);
    cleanup();
  });

  it('toggle collapses only when every section is currently expanded', async () => {
    const { container, result, cleanup } = setupHookWithContainer([true, true, true]);
    await waitFor(() => {
      expect(result.current.allExpanded).toBe(true);
    });
    act(() => {
      result.current.toggle();
    });
    const sections = container.querySelectorAll<HTMLDetailsElement>('details.settings-collapsible');
    expect([...sections].every((s) => !s.open)).toBe(true);
    cleanup();
  });

  it('toggle expands when every section is currently closed', async () => {
    const { container, result, cleanup } = setupHookWithContainer([false, false, false]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    expect(result.current.allExpanded).toBe(false);
    act(() => {
      result.current.toggle();
    });
    const sections = container.querySelectorAll<HTMLDetailsElement>('details.settings-collapsible');
    expect([...sections].every((s) => s.open)).toBe(true);
    cleanup();
  });

  it('toggle is a no-op when there are no sections', async () => {
    const { result, cleanup } = setupHookWithContainer([]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(false);
    });
    // Should not throw or have any side effect.
    act(() => {
      result.current.toggle();
    });
    expect(result.current.hasSections).toBe(false);
    cleanup();
  });

  it('reflects [open] attribute changes made outside the hook (MutationObserver)', async () => {
    // Simulates the user clicking a section header directly: the [open]
    // attribute flips natively, and the hook must observe + re-derive.
    // We open ALL sections (not just one) so the externally-driven change
    // is observable via the only remaining reader, `allExpanded`.
    const { container, result, cleanup } = setupHookWithContainer([false, false, false]);
    await waitFor(() => {
      expect(result.current.allExpanded).toBe(false);
    });
    act(() => {
      const sections = container.querySelectorAll<HTMLDetailsElement>(
        'details.settings-collapsible'
      );
      for (const s of sections) s.open = true;
    });
    await waitFor(() => {
      expect(result.current.allExpanded).toBe(true);
    });
    cleanup();
  });

  it('reflects sections added after mount (childList observer)', async () => {
    const { container, result, cleanup } = setupHookWithContainer([]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(false);
    });
    // Simulate a tab switch that introduces new collapsible sections.
    act(() => {
      const newSection = document.createElement('details');
      newSection.className = 'settings-collapsible';
      newSection.id = 'late-mount';
      container.appendChild(newSection);
    });
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    cleanup();
  });

  it('reflects sections removed after mount (childList observer)', async () => {
    const { container, result, cleanup } = setupHookWithContainer([true, true]);
    await waitFor(() => {
      expect(result.current.hasSections).toBe(true);
    });
    act(() => {
      const sections = container.querySelectorAll('details.settings-collapsible');
      for (const s of sections) s.remove();
    });
    await waitFor(() => {
      expect(result.current.hasSections).toBe(false);
    });
    expect(result.current.allExpanded).toBe(false);
    cleanup();
  });
});
