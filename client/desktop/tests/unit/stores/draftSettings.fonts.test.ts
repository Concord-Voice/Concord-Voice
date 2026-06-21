import { describe, it, expect, beforeEach } from 'vitest';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { useDraftSettingsStore } from '@/renderer/stores/draftSettingsStore';
import { setDraftAppearanceSetting } from '@/renderer/hooks/useDraftSettings';
import { resetAllStores } from '../../helpers/store-helpers';

// Integration: the draft layer write-throughs via callSetter → setAppFont, and Revert
// restores via restoreAppearanceFromSnapshot → setAppFont. This exercises the
// LOAD-BEARING setter through the REAL draft machinery (no mocks) — the path that
// silently breaks if `setAppFont` is missing or renamed.
beforeEach(() => {
  resetAllStores();
  useSettingsStore.getState().setDyslexicSupport(false);
  useSettingsStore.getState().setAppFont('default');
  delete document.documentElement.dataset.appfont;
});

describe('draft font preview + revert', () => {
  it('draft write-through applies the font live; revert restores the prior font', () => {
    useDraftSettingsStore.getState().initialize(); // snapshot: appFont='default'
    setDraftAppearanceSetting('appFont', 'inter'); // write-through → setAppFont → subscriber
    expect(useSettingsStore.getState().appearance.appFont).toBe('inter');
    expect(document.documentElement.dataset.appfont).toBe('inter');

    useDraftSettingsStore.getState().revert(); // restoreAppearanceFromSnapshot → setAppFont('default')
    expect(useSettingsStore.getState().appearance.appFont).toBe('default');
    expect(document.documentElement.dataset.appfont).toBe('default');
  });

  it('apply persists the draft font selection', () => {
    useDraftSettingsStore.getState().initialize();
    setDraftAppearanceSetting('appFont', 'lexend');
    useDraftSettingsStore.getState().apply();
    expect(useSettingsStore.getState().appearance.appFont).toBe('lexend');
    expect(document.documentElement.dataset.appfont).toBe('lexend');
  });
});
