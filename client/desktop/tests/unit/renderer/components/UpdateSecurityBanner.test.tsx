import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useUpdateStatusStore } from '../../../../src/renderer/stores/updateStatusStore';
import { UpdateSecurityBanner } from '../../../../src/renderer/components/Updates/UpdateSecurityBanner';

describe('UpdateSecurityBanner (#658)', () => {
  const originalElectron = (globalThis as unknown as { electron?: unknown }).electron;

  beforeEach(() => {
    resetAllStores();
  });

  afterEach(() => {
    (globalThis as unknown as { electron?: unknown }).electron = originalElectron;
  });

  it('renders nothing when criticalError is null', () => {
    const { container } = render(<UpdateSecurityBanner />);
    expect(container.firstChild).toBeNull();
  });

  it('renders cert-pin copy when subtype is cert-pin-failure', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'pin miss');
    render(<UpdateSecurityBanner />);
    expect(screen.getByRole('alert')).toBeInTheDocument();
    expect(screen.getByText(/couldn't confirm the identity/i)).toBeInTheDocument();
  });

  it('renders publisher-failure copy when subtype is publisher-failure', () => {
    useUpdateStatusStore.getState().setSecurityError('publisher-failure', 'bad sig');
    render(<UpdateSecurityBanner />);
    expect(screen.getByText(/installer failed publisher verification/i)).toBeInTheDocument();
  });

  it('CTA link points to GitHub releases latest', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    render(<UpdateSecurityBanner />);
    const link = screen.getByRole('link', { name: /download the latest/i });
    expect(link).toHaveAttribute(
      'href',
      'https://github.com/Concord-Voice/Concord-Voice/releases/latest'
    );
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('dismiss requires two-step consenting confirmation', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    render(<UpdateSecurityBanner />);

    // Step 1: dismiss button visible; confirm copy not yet rendered.
    expect(screen.getByRole('button', { name: /^Dismiss$/i })).toBeInTheDocument();
    expect(screen.queryByText(/I understand Concord cannot verify/i)).toBeNull();

    // Click "Dismiss" — confirm UI appears; banner still visible; store not yet updated.
    fireEvent.click(screen.getByRole('button', { name: /^Dismiss$/i }));
    expect(screen.getByText(/I understand Concord cannot verify updates/i)).toBeInTheDocument();
    expect(screen.getByRole('alert')).toBeInTheDocument();
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);

    // Step 2: clicking the consenting confirm button dismisses.
    fireEvent.click(screen.getByRole('button', { name: /I understand — dismiss/i }));
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(true);
  });

  it('is hidden after dismissForSession', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    const { rerender, container } = render(<UpdateSecurityBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Dismiss$/i }));
    fireEvent.click(screen.getByRole('button', { name: /I understand — dismiss/i }));
    rerender(<UpdateSecurityBanner />);
    expect(container.firstChild).toBeNull();
  });

  it('re-renders on next session (after reset) if criticalError is set again', () => {
    // Simulate dismiss-then-relaunch: reset then re-populate error.
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    const { rerender } = render(<UpdateSecurityBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Dismiss$/i }));
    fireEvent.click(screen.getByRole('button', { name: /I understand — dismiss/i }));

    // Simulated "next launch": reset store, then IPC refires the error.
    useUpdateStatusStore.getState().reset();
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'recurring');
    rerender(<UpdateSecurityBanner />);
    expect(screen.getByRole('alert')).toBeInTheDocument();
  });

  // ─── CTA click → preload openExternal (#719 review) ─────────────────────
  it('CTA click routes through globalThis.electron.openExternal when available', () => {
    const openExternal = vi.fn(() => undefined);
    (globalThis as unknown as { electron: { openExternal: typeof openExternal } }).electron = {
      openExternal,
    };
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    render(<UpdateSecurityBanner />);

    const link = screen.getByRole('link', { name: /download the latest/i });
    fireEvent.click(link);

    expect(openExternal).toHaveBeenCalledWith(
      'https://github.com/Concord-Voice/Concord-Voice/releases/latest'
    );
  });

  it('CTA click attaches a .catch when openExternal returns a Promise', () => {
    // Rejected promise to exercise the catch path; swallowed per component contract.
    const openExternal = vi.fn(() => Promise.reject(new Error('bridge dead')));
    (globalThis as unknown as { electron: { openExternal: typeof openExternal } }).electron = {
      openExternal,
    };
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    render(<UpdateSecurityBanner />);

    fireEvent.click(screen.getByRole('link', { name: /download the latest/i }));

    expect(openExternal).toHaveBeenCalledOnce();
    // If the .catch wasn't attached Vitest's unhandled-rejection warning would fire.
  });

  it('CTA click falls through to default anchor when electron bridge is absent', () => {
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    render(<UpdateSecurityBanner />);

    const link = screen.getByRole('link', { name: /download the latest/i });
    // Click without preventDefault means the href takes effect; in jsdom
    // this is a no-op but the default path is exercised. Assert the link
    // still carries the correct href.
    expect(link).toHaveAttribute(
      'href',
      'https://github.com/Concord-Voice/Concord-Voice/releases/latest'
    );
    fireEvent.click(link);
    // No throw; the component's handleCtaClick returned early.
  });

  it('CTA click tolerates an openExternal that returns a non-Promise', () => {
    const openExternal = vi.fn(() => undefined as unknown as void);
    (globalThis as unknown as { electron: { openExternal: typeof openExternal } }).electron = {
      openExternal,
    };
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'x');
    render(<UpdateSecurityBanner />);

    fireEvent.click(screen.getByRole('link', { name: /download the latest/i }));

    expect(openExternal).toHaveBeenCalledOnce();
  });

  // ─── Confirm-prompt reset on new error (#719 review 3) ──────────────────
  it('resets showConfirm when a new subtype arrives mid-confirm', () => {
    // User dismissed error A → confirm UI showing (mid-two-step)
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'first');
    const { rerender } = render(<UpdateSecurityBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Dismiss$/i }));
    expect(screen.getByText(/I understand Concord cannot verify updates/i)).toBeInTheDocument();

    // Error B arrives (different subtype). The two-step gate should re-engage:
    // confirm UI must hide, Dismiss button must re-appear. A single click on
    // the (previously-visible) consent button would otherwise dismiss error B
    // without the user ever seeing its copy.
    useUpdateStatusStore.getState().setSecurityError('publisher-failure', 'second');
    rerender(<UpdateSecurityBanner />);

    expect(screen.queryByText(/I understand Concord cannot verify updates/i)).toBeNull();
    expect(screen.getByRole('button', { name: /^Dismiss$/i })).toBeInTheDocument();
    // Assert publisher-failure copy is now showing (and not pre-dismissed):
    expect(screen.getByText(/installer failed publisher verification/i)).toBeInTheDocument();
    expect(useUpdateStatusStore.getState().dismissedForSession).toBe(false);
  });

  it('resets showConfirm when the message changes (same subtype)', () => {
    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'first message');
    const { rerender } = render(<UpdateSecurityBanner />);
    fireEvent.click(screen.getByRole('button', { name: /^Dismiss$/i }));
    expect(screen.getByText(/I understand Concord cannot verify updates/i)).toBeInTheDocument();

    useUpdateStatusStore.getState().setSecurityError('cert-pin-failure', 'second message');
    rerender(<UpdateSecurityBanner />);

    expect(screen.queryByText(/I understand Concord cannot verify updates/i)).toBeNull();
    expect(screen.getByRole('button', { name: /^Dismiss$/i })).toBeInTheDocument();
  });

  // ─── Subtype fallback ───────────────────────────────────────────────────
  it('falls back to cert-pin copy for an unrecognized subtype', () => {
    useUpdateStatusStore.setState({
      // Cast to bypass the type check; simulates a future subtype value arriving
      // from an older preload bridge that doesn't know the new enum variant.
      criticalError: {
        subtype: 'unknown-future-subtype' as unknown as 'cert-pin-failure',
        message: 'x',
        firstSeenAt: Date.now(),
      },
      dismissedForSession: false,
    });
    render(<UpdateSecurityBanner />);
    expect(screen.getByText(/couldn't confirm the identity/i)).toBeInTheDocument();
  });
});
