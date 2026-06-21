import { useRef, useEffect, useState } from 'react';
import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import ExpandCollapseAllButton from '@/renderer/components/Settings/ExpandCollapseAllButton';

/**
 * Test harness: render the button alongside a controlled container of
 * <details.settings-collapsible> children, and let tests assert on the
 * button's label flips + click side-effects on the container's DOM.
 */
interface HarnessProps {
  initialOpenStates: boolean[];
  /** Optional dynamic re-keying — flip to re-render with different states. */
  controlKey?: number;
}

function Harness({ initialOpenStates }: HarnessProps) {
  const containerRef = useRef<HTMLDivElement>(null);

  // Populate the container imperatively so we exercise the same code path
  // the real CollapsibleSection takes (native <details> in the DOM).
  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    container.innerHTML = '';
    for (let i = 0; i < initialOpenStates.length; i++) {
      const details = document.createElement('details');
      details.className = 'settings-collapsible';
      details.id = `section-${i}`;
      if (initialOpenStates[i]) details.open = true;
      container.appendChild(details);
    }
  }, [initialOpenStates]);

  return (
    <div>
      <div ref={containerRef} data-testid="container" />
      <ExpandCollapseAllButton containerRef={containerRef} />
    </div>
  );
}

describe('ExpandCollapseAllButton', () => {
  it('renders nothing when the container has no collapsible sections', async () => {
    const { container } = render(<Harness initialOpenStates={[]} />);
    await waitFor(() => {
      // Container ref is mounted but the button hook reports hasSections=false.
      expect(container.querySelector('.settings-expand-collapse-all-btn')).toBeNull();
    });
  });

  it('renders "Expand all" when every section is currently closed', async () => {
    render(<Harness initialOpenStates={[false, false, false]} />);
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });
  });

  it('renders "Collapse all" when every section is currently expanded', async () => {
    render(<Harness initialOpenStates={[true, true, true]} />);
    await waitFor(() => {
      expect(screen.getByText('Collapse all')).toBeInTheDocument();
    });
  });

  it('renders "Expand all" in mixed state (per #297 spec)', async () => {
    // Spec: mixed state defaults to "expand all" so the button label
    // matches the action the click will perform.
    render(<Harness initialOpenStates={[true, false, true]} />);
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });
    expect(screen.queryByText('Collapse all')).not.toBeInTheDocument();
  });

  it('click expands every section when starting fully-closed', async () => {
    const { container } = render(<Harness initialOpenStates={[false, false, false]} />);
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Expand all'));

    await waitFor(() => {
      const sections = container.querySelectorAll<HTMLDetailsElement>(
        'details.settings-collapsible'
      );
      expect([...sections].every((s) => s.open)).toBe(true);
    });
    // Label flips to "Collapse all" after the bulk-expand.
    await waitFor(() => {
      expect(screen.getByText('Collapse all')).toBeInTheDocument();
    });
  });

  it('click collapses every section when starting fully-expanded', async () => {
    const { container } = render(<Harness initialOpenStates={[true, true, true]} />);
    await waitFor(() => {
      expect(screen.getByText('Collapse all')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Collapse all'));

    await waitFor(() => {
      const sections = container.querySelectorAll<HTMLDetailsElement>(
        'details.settings-collapsible'
      );
      expect([...sections].every((s) => !s.open)).toBe(true);
    });
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });
  });

  it('click in mixed state expands all remaining (does NOT collapse)', async () => {
    const { container } = render(<Harness initialOpenStates={[true, false, true]} />);
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText('Expand all'));

    await waitFor(() => {
      const sections = container.querySelectorAll<HTMLDetailsElement>(
        'details.settings-collapsible'
      );
      expect([...sections].every((s) => s.open)).toBe(true);
    });
  });

  it('chevron mirrors the action: down when "Expand all", up when "Collapse all"', async () => {
    const { rerender, container } = render(<Harness initialOpenStates={[false, false]} />);
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });
    const expandChevron = container.querySelector('.settings-expand-collapse-all-chevron');
    expect(expandChevron).toHaveAttribute('data-expanded', 'false');

    // Re-render fully expanded so the label flips and the chevron should rotate.
    rerender(<Harness initialOpenStates={[true, true]} />);
    await waitFor(() => {
      expect(screen.getByText('Collapse all')).toBeInTheDocument();
    });
    const collapseChevron = container.querySelector('.settings-expand-collapse-all-chevron');
    expect(collapseChevron).toHaveAttribute('data-expanded', 'true');
  });

  it('hides itself when sections are removed at runtime', async () => {
    function DynamicHarness() {
      const containerRef = useRef<HTMLDivElement>(null);
      const [hasSections, setHasSections] = useState(true);

      useEffect(() => {
        const container = containerRef.current;
        if (!container) return;
        container.innerHTML = '';
        if (hasSections) {
          const details = document.createElement('details');
          details.className = 'settings-collapsible';
          container.appendChild(details);
        }
      }, [hasSections]);

      return (
        <div>
          <button type="button" onClick={() => setHasSections(false)} aria-label="remove">
            remove
          </button>
          <div ref={containerRef} />
          <ExpandCollapseAllButton containerRef={containerRef} />
        </div>
      );
    }

    render(<DynamicHarness />);
    await waitFor(() => {
      expect(screen.getByText('Expand all')).toBeInTheDocument();
    });

    fireEvent.click(screen.getByLabelText('remove'));

    await waitFor(() => {
      expect(screen.queryByText('Expand all')).not.toBeInTheDocument();
      expect(screen.queryByText('Collapse all')).not.toBeInTheDocument();
    });
  });
});
