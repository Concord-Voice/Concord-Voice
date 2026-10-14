import { describe, it, expect, beforeEach } from 'vitest';
import { useSettingsNavStore } from '@/renderer/stores/ui/settingsNavStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('settingsNavStore', () => {
  beforeEach(() => resetAllStores());

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
