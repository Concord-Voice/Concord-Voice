import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, act } from '@testing-library/react';
import { SpaFallbackOverlay } from '../../../src/renderer/components/SpaFallbackOverlay/SpaFallbackOverlay';
import { resetAllStores } from '../../helpers/store-helpers';

describe('SpaFallbackOverlay', () => {
  const originalElectron = (globalThis as unknown as { electron?: unknown }).electron;
  let configFetchFailedHandler: ((data: { reason: string }) => void) | null = null;

  beforeEach(() => {
    // Required since #2401: the component reads chatStore, so a leaked
    // `isConnected: true` would seed its latch and suppress the banner these
    // cases assert on — failing with a cause that points at the component
    // rather than at leaked store state ([internal]rules/tests.md).
    resetAllStores();
    configFetchFailedHandler = null;
    (globalThis as unknown as { electron: unknown }).electron = {
      onConfigFetchFailed: (handler: (data: { reason: string }) => void) => {
        configFetchFailedHandler = handler;
        return () => {
          configFetchFailedHandler = null;
        };
      },
    };
  });

  afterEach(() => {
    (globalThis as unknown as { electron?: unknown }).electron = originalElectron;
  });

  it('does not render any banner before the IPC event fires', () => {
    render(<SpaFallbackOverlay />);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('renders the banner with the reason string when the IPC event fires', () => {
    render(<SpaFallbackOverlay />);
    act(() => {
      configFetchFailedHandler?.({ reason: 'Could not reach Concord servers' });
    });
    expect(screen.getByRole('alert')).toHaveTextContent('Could not reach Concord servers');
  });

  it('dismisses the banner when the user clicks the close button', () => {
    render(<SpaFallbackOverlay />);
    act(() => {
      configFetchFailedHandler?.({ reason: 'Could not reach Concord servers' });
    });
    fireEvent.click(screen.getByRole('button', { name: /dismiss/i }));
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('handles missing electron.onConfigFetchFailed gracefully (renders nothing)', () => {
    (globalThis as unknown as { electron?: unknown }).electron = undefined;
    render(<SpaFallbackOverlay />);
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('unsubscribes from the IPC event on unmount', () => {
    let unsubscribed = false;
    (globalThis as unknown as { electron: unknown }).electron = {
      onConfigFetchFailed: () => () => {
        unsubscribed = true;
      },
    };
    const { unmount } = render(<SpaFallbackOverlay />);
    expect(unsubscribed).toBe(false);
    unmount();
    expect(unsubscribed).toBe(true);
  });
});
