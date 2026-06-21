import { render, screen, fireEvent } from '../../../test-utils';
import ServerE2eeIndicator from '@/renderer/components/MainView/ServerE2eeIndicator';

// Extracted from MainView in the SC-1 cognitive-complexity cleanup. These tests
// cover the new file directly; MainView.test.tsx still exercises it via the full
// MainView render (the DOM is unchanged by the extraction).
//
// Post-#1647 the indicator renders ONE unconditional always-encrypted state —
// the per-server e2ee_default opt-out was removed (E2EE-everywhere is structural),
// so there is no `server` prop and no unencrypted/"E2EE Disabled" branch.

// A non-zero rect so computeE2eeTooltipPos has realistic geometry. The two rects
// below drive its right-aligned vs left-clamped branches.
const rect = (over: Partial<DOMRect>): DOMRect =>
  ({
    right: 500,
    left: 260,
    bottom: 40,
    top: 20,
    width: 20,
    height: 20,
    x: 260,
    y: 20,
    toJSON: () => ({}),
    ...over,
  }) as DOMRect;

describe('ServerE2eeIndicator', () => {
  it('renders the encrypted lock unconditionally', () => {
    const { container } = render(<ServerE2eeIndicator />);
    const indicator = container.querySelector('.server-e2ee-indicator');
    expect(indicator).toBeInTheDocument();
    expect(indicator).toHaveClass('encrypted');
    expect(indicator).not.toHaveClass('unencrypted');
  });

  it('carries the enabled aria-label', () => {
    const { container } = render(<ServerE2eeIndicator />);
    const indicator = container.querySelector('.server-e2ee-indicator') as HTMLElement;
    expect(indicator).toHaveAttribute('aria-label', 'Server end-to-end encryption: enabled');
  });

  it('shows the enabled tooltip on hover (right-aligned) and hides it on leave', () => {
    const { container } = render(<ServerE2eeIndicator />);
    const indicator = container.querySelector('.server-e2ee-indicator') as HTMLElement;
    indicator.getBoundingClientRect = () => rect({ right: 500, left: 260 });

    fireEvent.mouseEnter(indicator);
    expect(screen.getByText('E2EE Enabled')).toBeInTheDocument();
    // The misleading "E2EE Disabled" copy is gone for good.
    expect(screen.queryByText('E2EE Disabled')).not.toBeInTheDocument();

    fireEvent.mouseLeave(indicator);
    expect(screen.queryByText('E2EE Enabled')).not.toBeInTheDocument();
  });

  it('shows the tooltip clamped when the indicator is near the left edge', () => {
    const { container } = render(<ServerE2eeIndicator />);
    const indicator = container.querySelector('.server-e2ee-indicator') as HTMLElement;
    indicator.getBoundingClientRect = () => rect({ right: 100, left: 10 });

    fireEvent.mouseEnter(indicator);
    // Same always-encrypted copy regardless of position.
    expect(screen.getByText('E2EE Enabled')).toBeInTheDocument();
  });

  it('shows the tooltip on keyboard focus and hides it on blur (a11y parity)', () => {
    const { container } = render(<ServerE2eeIndicator />);
    const indicator = container.querySelector('.server-e2ee-indicator') as HTMLElement;
    indicator.getBoundingClientRect = () => rect({ right: 500, left: 260 });

    fireEvent.focus(indicator);
    expect(screen.getByText('E2EE Enabled')).toBeInTheDocument();

    fireEvent.blur(indicator);
    expect(screen.queryByText('E2EE Enabled')).not.toBeInTheDocument();
  });
});
