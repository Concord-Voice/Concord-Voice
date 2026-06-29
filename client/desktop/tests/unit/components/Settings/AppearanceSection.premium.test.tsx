import { vi } from 'vitest';
import React from 'react';

const mockSetDraftAppearanceSetting = vi.fn();
const appearanceState: Record<string, unknown> = {
  theme: 'dark',
  colorScheme: 'concord',
  customColors: null,
};

vi.mock('@/renderer/hooks/useDraftSettings', () => ({
  useDraftAppearance: vi.fn(() => appearanceState),
  setDraftAppearanceSetting: (...args: unknown[]) => mockSetDraftAppearanceSetting(...args),
}));

vi.mock('@/renderer/components/Settings/SettingsPreviewPanel', () => ({ default: () => null }));
vi.mock('@/renderer/components/Settings/FontSection', () => ({ default: () => null }));
vi.mock('@/renderer/components/Settings/ClientBehaviorSection', () => ({
  ClientBehaviorSection: () => null,
}));

import { render, screen, fireEvent } from '../../../test-utils';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import AppearanceSection from '@/renderer/components/Settings/AppearanceSection';

function customAddButton(): HTMLButtonElement {
  return document.querySelector('.color-scheme-circle.custom-add') as HTMLButtonElement;
}

function expandColorSchemeSection() {
  for (const d of document.querySelectorAll('details')) d.open = true;
}

beforeEach(() => {
  vi.clearAllMocks();
  appearanceState.colorScheme = 'concord';
  appearanceState.customColors = null;
  useSettingsNavStore.getState().clearFocusRequest();
});

describe('AppearanceSection - custom scheme access', () => {
  it('free users see the custom-add circle without a premium gate or chip', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();

    expect(customAddButton().closest('.premium-gate')).toBeNull();
    expect(screen.queryByText('Premium')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Premium feature')).not.toBeInTheDocument();
  });

  it('free users can click the custom-add circle to open the picker with defaults', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();

    fireEvent.click(customAddButton());

    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('customColors', {
      background: '#0d0821',
      accentPrimary: '#fa709a',
      accentSecondary: '#ffe13f',
    });
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('colorScheme', 'custom');
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it('free users can reselect an existing custom scheme', () => {
    const customColors = {
      background: '#111111',
      accentPrimary: '#222222',
      accentSecondary: '#333333',
    };
    appearanceState.customColors = customColors;

    render(<AppearanceSection />);
    expandColorSchemeSection();
    fireEvent.click(customAddButton());

    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('customColors', customColors);
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('colorScheme', 'custom');
    expect(useSettingsNavStore.getState().focusRequest).toBeNull();
  });

  it('built-in scheme swatches still work', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();

    fireEvent.click(screen.getByTitle('Hacker'));

    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('colorScheme', 'hacker');
  });
});
