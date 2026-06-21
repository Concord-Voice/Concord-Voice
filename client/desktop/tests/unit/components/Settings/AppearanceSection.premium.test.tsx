import { vi } from 'vitest';
import React from 'react';

// ─── Mocks (before component imports) ───────────────────────────────────────

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

// Heavy child panels are out of scope for the L4 test — stub them.
vi.mock('@/renderer/components/Settings/SettingsPreviewPanel', () => ({ default: () => null }));
vi.mock('@/renderer/components/Settings/FontSection', () => ({ default: () => null }));
vi.mock('@/renderer/components/Settings/ClientBehaviorSection', () => ({
  ClientBehaviorSection: () => null,
}));

// Entitlement: FREE floor (allowCustomScheme false) by default.
const entitlementOverrides: Record<string, unknown> = {};
function freeEntitlement() {
  return { allowCustomScheme: false, ...entitlementOverrides };
}
vi.mock('@/renderer/hooks/useEntitlement', () => ({
  useEntitlement: vi.fn((selector: (e: Record<string, unknown>) => unknown) =>
    selector(freeEntitlement())
  ),
}));

// ─── Imports (after mocks) ──────────────────────────────────────────────────

import { render, screen, fireEvent } from '../../../test-utils';
import { useSettingsNavStore } from '@/renderer/stores/settingsNavStore';
import AppearanceSection from '@/renderer/components/Settings/AppearanceSection';

function setEntitlement(overrides: Record<string, unknown>) {
  for (const k of Object.keys(entitlementOverrides)) delete entitlementOverrides[k];
  Object.assign(entitlementOverrides, overrides);
}

function customAddButton(): HTMLButtonElement {
  return document.querySelector('.color-scheme-circle.custom-add') as HTMLButtonElement;
}

function expandColorSchemeSection() {
  for (const d of document.querySelectorAll('details')) d.open = true;
}

beforeEach(() => {
  vi.clearAllMocks();
  setEntitlement({});
  appearanceState.colorScheme = 'concord';
  appearanceState.customColors = null;
  useSettingsNavStore.getState().clearFocusRequest();
});

// ─── L4: custom color scheme add-circle lock ────────────────────────────────

describe('AppearanceSection — L4 custom-scheme lock', () => {
  it('locked (free): the custom-add circle is wrapped by the premium gate + chip', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();
    const gate = customAddButton().closest('.premium-gate') as HTMLElement;
    expect(gate).not.toBeNull();
    expect(screen.getByText('Premium')).toBeInTheDocument();
    expect(screen.getByLabelText('Premium feature')).toBeInTheDocument();
  });

  it('locked (free): the custom-add circle stays focusable + aria-disabled, never disabled (O1)', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();
    const btn = customAddButton();
    const gate = btn.closest('.premium-gate') as HTMLElement;
    // After the S6819 fix the wrapper is a plain container — no role/tabindex/
    // aria-disabled. The CONTROL (the custom-add button) carries the ARIA and
    // stays focusable (never the HTML disabled attribute).
    expect(gate).not.toHaveAttribute('role');
    expect(gate).not.toHaveAttribute('tabindex');
    expect(gate).not.toHaveAttribute('aria-disabled');
    expect(btn).toHaveAttribute('aria-disabled', 'true');
    expect(btn).toHaveAttribute('aria-describedby');
    expect(btn).not.toBeDisabled();
    expect(gate.style.pointerEvents).not.toBe('none');
  });

  it('locked (free): clicking routes to Subscription, never opens the picker', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();
    fireEvent.click(customAddButton());
    // The custom-colour picker is NOT opened (no draft mutation to 'custom').
    expect(mockSetDraftAppearanceSetting).not.toHaveBeenCalledWith('colorScheme', 'custom');
    expect(useSettingsNavStore.getState().focusRequest).toEqual({
      section: 'account',
      controlId: 'section-subscription',
    });
  });

  it('entitled (premium): clicking opens the custom picker, no chip', () => {
    setEntitlement({ allowCustomScheme: true });
    render(<AppearanceSection />);
    expandColorSchemeSection();
    expect(customAddButton().closest('.premium-gate')).toBeNull();
    expect(screen.queryByText('Premium')).not.toBeInTheDocument();
    fireEvent.click(customAddButton());
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('colorScheme', 'custom');
  });

  it('entitled (premium): the built-in scheme swatches still work', () => {
    setEntitlement({ allowCustomScheme: true });
    render(<AppearanceSection />);
    expandColorSchemeSection();
    // A non-custom swatch (Hacker) selects normally.
    const hacker = screen.getByTitle('Hacker');
    fireEvent.click(hacker);
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('colorScheme', 'hacker');
  });

  it('locked (free): the built-in scheme swatches are NOT gated', () => {
    render(<AppearanceSection />);
    expandColorSchemeSection();
    const hacker = screen.getByTitle('Hacker');
    fireEvent.click(hacker);
    expect(mockSetDraftAppearanceSetting).toHaveBeenCalledWith('colorScheme', 'hacker');
  });
});
