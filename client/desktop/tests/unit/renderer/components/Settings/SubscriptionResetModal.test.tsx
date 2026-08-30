import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent } from '../../../../test-utils';
import SubscriptionResetModal from '@/renderer/components/Settings/SubscriptionResetModal';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { resetAllStores } from '../../../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  useSettingsNavStore.getState().clearFocusRequest();
  useSettingsOverlayStore.setState({ open: null, payload: null });
  vi.restoreAllMocks();
});

describe('SubscriptionResetModal', () => {
  it('does not render when open=false', () => {
    render(<SubscriptionResetModal open={false} onAcknowledge={vi.fn()} />);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('renders neutral reset copy when open', () => {
    render(<SubscriptionResetModal open onAcknowledge={vi.fn()} />);

    const dialog = screen.getByRole('dialog');
    expect(dialog).toHaveTextContent(/reset to free defaults/i);
    expect(dialog).not.toHaveTextContent(/premium/i);
  });

  it('is an aria-modal dialog with the native showModal focus trap', () => {
    const showModalSpy = vi
      .spyOn(HTMLDialogElement.prototype, 'showModal')
      .mockImplementation(function (this: HTMLDialogElement) {
        this.setAttribute('open', '');
      });
    render(<SubscriptionResetModal open onAcknowledge={vi.fn()} />);

    expect(screen.getByRole('dialog')).toHaveAttribute('aria-modal', 'true');
    expect(showModalSpy).toHaveBeenCalled();
  });

  it('unmounts the dialog when closed so the native trap can restore focus', () => {
    vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (
      this: HTMLDialogElement
    ) {
      this.setAttribute('open', '');
    });
    const { rerender } = render(<SubscriptionResetModal open onAcknowledge={vi.fn()} />);
    expect(screen.getByRole('dialog')).toBeInTheDocument();

    rerender(<SubscriptionResetModal open={false} onAcknowledge={vi.fn()} />);

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('acknowledges from the primary action', async () => {
    const user = userEvent.setup();
    const onAcknowledge = vi.fn();
    render(<SubscriptionResetModal open onAcknowledge={onAcknowledge} />);

    await user.click(screen.getByRole('button', { name: 'Got it' }));

    expect(onAcknowledge).toHaveBeenCalledTimes(1);
  });

  it('acknowledges before opening planned subscriptions at the stable target', async () => {
    const user = userEvent.setup();
    let overlayDuringAcknowledge: string | null = 'unobserved';
    let focusDuringAcknowledge = useSettingsNavStore.getState().focusRequest;
    const onAcknowledge = vi.fn(() => {
      overlayDuringAcknowledge = useSettingsOverlayStore.getState().open;
      focusDuringAcknowledge = useSettingsNavStore.getState().focusRequest;
    });
    render(<SubscriptionResetModal open onAcknowledge={onAcknowledge} />);

    await user.click(screen.getByRole('button', { name: 'View planned subscriptions' }));

    expect(onAcknowledge).toHaveBeenCalledTimes(1);
    expect(overlayDuringAcknowledge).toBeNull();
    expect(focusDuringAcknowledge).toBeNull();
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'subscriptions',
      controlId: 'section-current-plan',
    });
  });

  it('acknowledges on Escape in the jsdom fallback path', async () => {
    const user = userEvent.setup();
    const onAcknowledge = vi.fn();
    render(<SubscriptionResetModal open onAcknowledge={onAcknowledge} />);

    await user.keyboard('{Escape}');

    expect(onAcknowledge).toHaveBeenCalledTimes(1);
  });
});
