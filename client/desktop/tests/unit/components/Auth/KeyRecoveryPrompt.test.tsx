import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, it, expect, vi, beforeEach } from 'vitest';
import KeyRecoveryPrompt from '@/renderer/components/Auth/KeyRecoveryPrompt';
import { resetAllStores } from '../../../helpers/store-helpers';

describe('KeyRecoveryPrompt', () => {
  beforeEach(() => resetAllStores());

  it('disables Reset until data-loss is acknowledged', async () => {
    const user = userEvent.setup();
    render(<KeyRecoveryPrompt onReset={vi.fn()} onCancel={vi.fn()} />);
    const reset = screen.getByRole('button', { name: /reset and continue/i });
    expect(reset).toBeDisabled();
    await user.click(screen.getByRole('checkbox'));
    expect(reset).toBeEnabled();
  });

  it('calls onReset only after acknowledge', async () => {
    const onReset = vi.fn();
    const user = userEvent.setup();
    render(<KeyRecoveryPrompt onReset={onReset} onCancel={vi.fn()} />);
    await user.click(screen.getByRole('checkbox'));
    await user.click(screen.getByRole('button', { name: /reset and continue/i }));
    expect(onReset).toHaveBeenCalledTimes(1);
  });

  // A second activation issues a second PUT /users/me/keys; that concurrent
  // destructive flow is exactly what makes the server omit the continuation
  // pair (#2415), so the UI must not be able to manufacture it.
  it('calls onReset exactly once when consent is activated twice', async () => {
    const onReset = vi.fn();
    const user = userEvent.setup();
    render(<KeyRecoveryPrompt onReset={onReset} onCancel={vi.fn()} />);
    await user.click(screen.getByRole('checkbox'));
    const confirm = screen.getByRole('button', { name: /reset and continue/i });
    await user.click(confirm);
    await user.click(confirm);
    expect(onReset).toHaveBeenCalledTimes(1);
    expect(confirm).toBeDisabled();
    expect(screen.getByRole('button', { name: /cancel/i })).toBeDisabled();
  });

  // Escape routes through the native <dialog> 'cancel' event, which is NOT
  // gated on submitting — otherwise the focus trap would hold the user.
  it('keeps Escape-to-cancel reachable after the reset is submitted', async () => {
    const onCancel = vi.fn();
    const user = userEvent.setup();
    const { container } = render(<KeyRecoveryPrompt onReset={vi.fn()} onCancel={onCancel} />);
    await user.click(screen.getByRole('checkbox'));
    await user.click(screen.getByRole('button', { name: /reset and continue/i }));

    const dialog = container.querySelector('dialog');
    expect(dialog).not.toBeNull();
    dialog?.dispatchEvent(new Event('cancel', { cancelable: true }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('calls onCancel from the Cancel button', async () => {
    const onCancel = vi.fn();
    const user = userEvent.setup();
    render(<KeyRecoveryPrompt onReset={vi.fn()} onCancel={onCancel} />);
    await user.click(screen.getByRole('button', { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it('in MFA mode, requires a code and passes it to onReset', async () => {
    const onReset = vi.fn();
    const user = userEvent.setup();
    render(<KeyRecoveryPrompt mfaRequired onReset={onReset} onCancel={vi.fn()} />);
    // No acknowledge checkbox in MFA mode; the verify button is gated on a code.
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
    const verify = screen.getByRole('button', { name: /verify and reset/i });
    expect(verify).toBeDisabled();
    await user.type(screen.getByRole('textbox'), '123456');
    expect(verify).toBeEnabled();
    await user.click(verify);
    expect(onReset).toHaveBeenCalledWith('123456');
  });
});
