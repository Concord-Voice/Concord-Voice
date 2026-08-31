import { describe, it, expect, beforeEach, vi } from 'vitest';
import { openSubscriptionPage } from '@/renderer/utils/ui/openSubscriptionPage';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/ui/settingsOverlayStore';
import { resetAllStores } from '../../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  useSettingsNavStore.getState().clearFocusRequest();
  useSettingsOverlayStore.setState({ open: null, payload: null });
});

describe('openSubscriptionPage', () => {
  it('opens app settings and navigates to Subscriptions ▸ Current Plan', () => {
    openSubscriptionPage();
    expect(useSettingsOverlayStore.getState().open).toBe('app');
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'subscriptions',
      controlId: 'section-current-plan',
    });
  });

  it('routes through settingsNavStore.requestFocus exactly once', () => {
    const spy = vi.spyOn(useSettingsNavStore.getState(), 'requestFocus');
    openSubscriptionPage('audio-tier');
    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith('subscriptions', 'section-current-plan');
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

  it('does not render a Subscription page itself', () => {
    const result = openSubscriptionPage('video-quality');
    expect(result).toBeUndefined();
  });
});
