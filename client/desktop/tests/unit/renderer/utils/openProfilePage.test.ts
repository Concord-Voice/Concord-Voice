import { describe, it, expect, beforeEach, vi } from 'vitest';
import { openProfilePage } from '@/renderer/utils/openProfilePage';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import { useSettingsOverlayStore } from '@/renderer/stores/settingsOverlayStore';

beforeEach(() => {
  useSettingsNavStore.getState().clearFocusRequest();
  useSettingsOverlayStore.getState().close();
});

describe('openProfilePage', () => {
  it('opens the app-settings overlay', () => {
    openProfilePage();
    expect(useSettingsOverlayStore.getState().open).toBe('app');
  });

  it('requests focus on Account ▸ section-profile', () => {
    openProfilePage();
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-profile',
    });
  });

  it('routes through both stores exactly once', () => {
    const openSpy = vi.spyOn(useSettingsOverlayStore.getState(), 'openSettings');
    const focusSpy = vi.spyOn(useSettingsNavStore.getState(), 'requestFocus');
    openProfilePage();
    expect(openSpy).toHaveBeenCalledTimes(1);
    expect(openSpy).toHaveBeenCalledWith('app');
    expect(focusSpy).toHaveBeenCalledTimes(1);
    expect(focusSpy).toHaveBeenCalledWith('account', 'section-profile');
    openSpy.mockRestore();
    focusSpy.mockRestore();
  });

  it('returns undefined (navigation only)', () => {
    expect(openProfilePage()).toBeUndefined();
  });
});
