import { describe, it, expect, beforeEach } from 'vitest';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';

describe('settingsNavStore', () => {
  beforeEach(() => useSettingsNavStore.getState().clearFocusRequest());

  it('starts with no focus request', () => {
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it('requestFocus sets section + controlId; clearFocusRequest resets', () => {
    useSettingsNavStore.getState().requestFocus('accessibility', 'toggle-dyslexic-support');
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'accessibility',
      controlId: 'toggle-dyslexic-support',
    });
    useSettingsNavStore.getState().clearFocusRequest();
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });
});
