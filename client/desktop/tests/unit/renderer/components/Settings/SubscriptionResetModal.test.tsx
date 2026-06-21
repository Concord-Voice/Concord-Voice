import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import SubscriptionResetModal from '@/renderer/components/Settings/SubscriptionResetModal';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

beforeEach(() => {
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('SubscriptionResetModal', () => {
  it('does not render when open=false', () => {
    render(<SubscriptionResetModal open={false} onAcknowledge={vi.fn()} />);
    expect(screen.queryByText(/Some features are now part of Premium/i)).not.toBeInTheDocument();
  });

  it('renders the dossier copy when open', () => {
    render(<SubscriptionResetModal open onAcknowledge={vi.fn()} />);
    expect(screen.getByText(/Some features are now part of Premium/i)).toBeInTheDocument();
    expect(screen.getByText(/reset to free defaults/i)).toBeInTheDocument();
  });

  it('is an aria-modal dialog (focus trap via native showModal)', () => {
    const showModalSpy = vi
      .spyOn(HTMLDialogElement.prototype, 'showModal')
      .mockImplementation(function (this: HTMLDialogElement) {
        this.setAttribute('open', '');
      });
    render(<SubscriptionResetModal open onAcknowledge={vi.fn()} />);
    const dialog = document.querySelector('dialog');
    expect(dialog).toHaveAttribute('aria-modal', 'true');
    // .showModal() is the native focus trap (a11y O2).
    expect(showModalSpy).toHaveBeenCalled();
    showModalSpy.mockRestore();
  });

  it('unmounts the dialog when closed (focus returns via native showModal trap)', () => {
    vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (
      this: HTMLDialogElement
    ) {
      this.setAttribute('open', '');
    });
    const { rerender } = render(<SubscriptionResetModal open onAcknowledge={vi.fn()} />);
    expect(document.querySelector('dialog')).toBeInTheDocument();
    // Closing removes the dialog entirely. In production .showModal() trapped
    // focus on open and returns it to the prior element when the dialog leaves
    // the tree; jsdom can't simulate the return, so we assert the teardown.
    rerender(<SubscriptionResetModal open={false} onAcknowledge={vi.fn()} />);
    expect(document.querySelector('dialog')).not.toBeInTheDocument();
    vi.restoreAllMocks();
  });

  it('"Got it" acknowledges (dismiss + persist)', () => {
    const onAcknowledge = vi.fn();
    render(<SubscriptionResetModal open onAcknowledge={onAcknowledge} />);
    fireEvent.click(screen.getByRole('button', { name: /Got it/i }));
    expect(onAcknowledge).toHaveBeenCalledTimes(1);
  });

  it('"See what Premium includes" navigates AND acknowledges', () => {
    const onAcknowledge = vi.fn();
    render(<SubscriptionResetModal open onAcknowledge={onAcknowledge} />);
    fireEvent.click(screen.getByRole('button', { name: /See what Premium includes/i }));
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
    expect(onAcknowledge).toHaveBeenCalledTimes(1);
  });

  it('acknowledges on Escape (jsdom fallback path)', () => {
    const onAcknowledge = vi.fn();
    render(<SubscriptionResetModal open onAcknowledge={onAcknowledge} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onAcknowledge).toHaveBeenCalled();
  });
});
