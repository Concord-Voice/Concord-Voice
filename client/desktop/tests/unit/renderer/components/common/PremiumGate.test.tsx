import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import PremiumGate from '@/renderer/components/common/PremiumGate';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

beforeEach(() => {
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('PremiumGate — entitled passthrough', () => {
  it('renders children untouched when entitled (no chip, no wrapper aria)', () => {
    render(
      <PremiumGate mode="dim" entitled>
        <button type="button">Custom theme</button>
      </PremiumGate>
    );
    const control = screen.getByRole('button', { name: 'Custom theme' });
    expect(control).toBeInTheDocument();
    expect(control).not.toHaveAttribute('aria-disabled');
    expect(screen.queryByText('Premium')).not.toBeInTheDocument();
  });
});

describe('PremiumGate — locked state (a11y O1)', () => {
  function renderLocked(section?: undefined) {
    void section;
    return render(
      <PremiumGate mode="dim" entitled={false} feature="customScheme">
        <button type="button">Custom theme</button>
      </PremiumGate>
    );
  }

  it('keeps the gated control focusable, marks IT aria-disabled, and renders the chip', () => {
    renderLocked();
    // The wrapped control is still in the document, focusable (native button),
    // and NOT given the HTML disabled attribute.
    const control = screen.getByRole('button', { name: 'Custom theme' });
    expect(control).toBeInTheDocument();
    expect(control).not.toBeDisabled();
    // After #1301's S6819 fix, aria-disabled is carried by the CONTROL itself,
    // not the wrapper — keeping the control focusable while announcing dormancy.
    expect(control).toHaveAttribute('aria-disabled', 'true');
    // Inline chip present (the word "Premium" + glyph) — it is the focusable
    // upgrade affordance (a native <button> via onActivate).
    expect(screen.getByText('Premium')).toBeInTheDocument();
    expect(screen.getByLabelText('Premium feature')).toBeInTheDocument();
  });

  it("points the control's aria-describedby at the chip button", () => {
    renderLocked();
    const control = screen.getByRole('button', { name: 'Custom theme' });
    const describedBy = control.getAttribute('aria-describedby');
    expect(describedBy).toBeTruthy();
    // The chip carries the same id and is itself a focusable native button.
    const chip = document.getElementById(describedBy as string) as HTMLElement;
    expect(chip).toBeInTheDocument();
    expect(chip.tagName).toBe('BUTTON');
    expect(chip).toHaveTextContent('Premium');
  });

  it('the wrapper span is NOT interactive — no role, no tabIndex (S6819/S6845)', () => {
    const { container } = renderLocked();
    const group = container.querySelector('.premium-gate') as HTMLElement;
    // The wrapper must not be the interactive/focusable element.
    expect(group).not.toHaveAttribute('role');
    expect(group).not.toHaveAttribute('tabindex');
    // aria-disabled belongs on the control, not the wrapper.
    expect(group).not.toHaveAttribute('aria-disabled');
  });

  it('NEVER applies the HTML disabled attribute to the gated control (O1)', () => {
    const { container } = renderLocked();
    expect(container.querySelector('[disabled]')).toBeNull();
    expect(container.querySelector('.premium-gate')).not.toHaveAttribute('disabled');
  });

  it('NEVER uses pointer-events:none on the gate container (O1)', () => {
    const { container } = renderLocked();
    const group = container.querySelector('.premium-gate') as HTMLElement;
    // Inline style must not disable pointer events.
    expect(group.style.pointerEvents).not.toBe('none');
    // Class-based guard: the gate must not reuse the disabled-row class.
    expect(group.className).not.toContain('settings-row-disabled');
  });

  it('routes click to the Subscription page instead of the control action', async () => {
    const controlClick = vi.fn();
    render(
      <PremiumGate mode="option" entitled={false}>
        <button type="button" onClick={controlClick}>
          4K · 120fps
        </button>
      </PremiumGate>
    );
    await userEvent.click(screen.getByRole('button', { name: '4K · 120fps' }));
    // The underlying control action did NOT fire (gate intercepted it)…
    expect(controlClick).not.toHaveBeenCalled();
    // …and the Subscription navigation DID.
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('routes Enter/Space activation of the control to the Subscription page', async () => {
    const controlClick = vi.fn();
    render(
      <PremiumGate mode="clamp" entitled={false}>
        <button type="button" onClick={controlClick}>
          1440p slider
        </button>
      </PremiumGate>
    );
    // The gated control is itself focusable (O1). Keyboard-activating it is
    // intercepted in the capture phase and routed to Subscription.
    const control = screen.getByRole('button', { name: '1440p slider' });
    control.focus();
    await userEvent.keyboard('{Enter}');
    expect(controlClick).not.toHaveBeenCalled();
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('activating the chip button itself also routes to the Subscription page', async () => {
    renderLocked();
    // The chip is the dedicated upgrade affordance — a focusable native button.
    const chip = screen.getByRole('button', { name: /Premium/ });
    await userEvent.click(chip);
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('applies the mode modifier class', () => {
    const { container } = render(
      <PremiumGate mode="clamp" entitled={false}>
        <span>x</span>
      </PremiumGate>
    );
    expect(container.querySelector('.premium-gate--clamp')).toBeInTheDocument();
  });
});
