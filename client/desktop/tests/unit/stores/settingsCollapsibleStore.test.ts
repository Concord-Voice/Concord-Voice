import { describe, it, expect, vi, beforeEach } from 'vitest';
import { useSettingsCollapsibleStore } from '@/renderer/stores/ui/settingsCollapsibleStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('settingsCollapsibleStore (#2365)', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('S1 starts with no remembered sections', () => {
    expect(useSettingsCollapsibleStore.getInitialState().openSections).toEqual({});
    expect(useSettingsCollapsibleStore.getState().openSections).toEqual({});
  });

  it('S2 records open and closed per id without touching other ids', () => {
    const { setSectionOpen } = useSettingsCollapsibleStore.getState();

    setSectionOpen('section-a', true);
    setSectionOpen('section-b', true);
    expect(useSettingsCollapsibleStore.getState().openSections).toEqual({
      'section-a': true,
      'section-b': true,
    });

    setSectionOpen('section-a', false);
    expect(useSettingsCollapsibleStore.getState().openSections).toEqual({
      'section-a': false,
      'section-b': true,
    });
  });

  it('S3 a write matching current state does not notify', () => {
    const { setSectionOpen } = useSettingsCollapsibleStore.getState();
    const listener = vi.fn();
    const unsubscribe = useSettingsCollapsibleStore.subscribe(listener);

    try {
      // Positive control: a real change notifies, so the silent cases below cannot be
      // silent merely because the subscription is dead.
      setSectionOpen('section-a', true);
      expect(listener).toHaveBeenCalledTimes(1);

      // Same value again: guard must return the same state object.
      setSectionOpen('section-a', true);
      expect(listener).toHaveBeenCalledTimes(1);

      // Closing a never-seen id is a no-op (absent behaves exactly like closed).
      setSectionOpen('never-seen', false);
      expect(listener).toHaveBeenCalledTimes(1);
      expect(useSettingsCollapsibleStore.getState().openSections).toEqual({ 'section-a': true });

      // A genuine close notifies, and repeating it is silent again.
      setSectionOpen('section-a', false);
      expect(listener).toHaveBeenCalledTimes(2);
      setSectionOpen('section-a', false);
      expect(listener).toHaveBeenCalledTimes(2);
    } finally {
      unsubscribe();
    }
  });
});
