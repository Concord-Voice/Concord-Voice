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
