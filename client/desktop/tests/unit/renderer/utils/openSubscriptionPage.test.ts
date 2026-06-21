import { describe, it, expect, beforeEach, vi } from 'vitest';
import { openSubscriptionPage } from '@/renderer/utils/openSubscriptionPage';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

beforeEach(() => {
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('openSubscriptionPage', () => {
  it('navigates to Account ▸ section-subscription', () => {
    openSubscriptionPage();
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('routes through settingsNavStore.requestFocus exactly once', () => {
    const spy = vi.spyOn(useSettingsNavStore.getState(), 'requestFocus');
    openSubscriptionPage('audio-tier');
    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith('account', 'section-subscription');
    spy.mockRestore();
  });

  it('ignores the deep-link hint today (always the same destination)', () => {
    openSubscriptionPage('custom-scheme');
    const first = useSettingsNavStore.getState().focusRequest;
    useSettingsNavStore.getState().clearFocusRequest();
    openSubscriptionPage('upload-size');
    const second = useSettingsNavStore.getState().focusRequest;
    expect(first).toEqual(second);
  });

  it('only navigates — it does not render a Subscription page (page is #1304)', () => {
    // Boundary test (spec §8): the function has no return value / no DOM side
    // effect beyond the focus request.
    const result = openSubscriptionPage('video-quality');
    expect(result).toBeUndefined();
  });
});
