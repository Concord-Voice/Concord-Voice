import { render, act } from '../../../test-utils';
import { vi } from 'vitest';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { openSubscriptionPage } from '@/renderer/utils/openSubscriptionPage';
import SettingsPage from '@/renderer/components/Settings/SettingsPage';

// jsdom lacks scrollIntoView
beforeAll(() => {
  // eslint-disable-next-line @typescript-eslint/no-empty-function
  Element.prototype.scrollIntoView = vi.fn();
});

beforeEach(() => {
  resetAllStores();
  useSettingsNavStore.getState().clearFocusRequest();
  vi.useFakeTimers();
});
afterEach(() => vi.useRealTimers());

describe('SettingsPage cross-section focus (#1644)', () => {
  it('focuses the Current Plan summary when a subscription gate opens Settings', () => {
    render(<SettingsPage />);

    act(() => openSubscriptionPage());
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const summary = document.querySelector('#section-current-plan > summary');
    expect(summary).toHaveTextContent('Current Plan');
    expect(summary).toHaveFocus();
  });

  it('a focus request switches to the accessibility pane and focuses the toggle', () => {
    render(<SettingsPage />);
    // default pane is appearance; the dyslexic toggle is not mounted yet
    expect(document.getElementById('toggle-dyslexic-support')).toBeNull();

    act(() => {
      useSettingsNavStore.getState().requestFocus('accessibility', 'toggle-dyslexic-support');
    });
    // pane switches (effect re-runs), then the deferred 50ms timer focuses the control
    act(() => {
      vi.advanceTimersByTime(60);
    });

    const toggle = document.getElementById('toggle-dyslexic-support');
    expect(toggle).not.toBeNull();
    expect(document.activeElement).toBe(toggle);
    // request is cleared after handling
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it('clears a pending focus request when Settings unmounts before the timer fires', () => {
    // Reproduces the stale-request bug (Gitar/code-review): the nav store outlives
    // SettingsPage, so a request left pending at unmount would re-fire (spurious
    // pane-jump) on the next open. The effect cleanup must drop it.
    const { unmount } = render(<SettingsPage />);
    act(() => {
      useSettingsNavStore.getState().requestFocus('accessibility', 'toggle-dyslexic-support');
    });
    // close Settings (unmount) BEFORE advancing the 50ms focus timer
    act(() => {
      unmount();
    });
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
});
