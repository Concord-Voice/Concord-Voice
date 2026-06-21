import React from 'react';
import type { AppearanceSettings, CustomColors } from '../../stores/settingsStore';
import { useDraftAppearance, setDraftAppearanceSetting } from '../../hooks/useDraftSettings';
import { useLayoutStore } from '../../stores/layoutStore';
import { useEntitlement } from '../../hooks/useEntitlement';
import { isValidHex } from '../../utils/colorUtils';
import CollapsibleSection from './CollapsibleSection';
import { ClientBehaviorSection } from './ClientBehaviorSection';
import ToggleSwitch from './ToggleSwitch';
import PremiumGate from '../common/PremiumGate';
import SettingsPreviewPanel from './SettingsPreviewPanel';
import FontSection from './FontSection';

// Module-scope constants — declared outside the component so they're not
// re-allocated on every render. SCHEME_TUPLES is the flat tuple form produced
// by the dedup refactor — the per-scheme `{ value, label, gradient }` object
// shape gets constructed once below via .map(), avoiding the literal
// duplication that SonarQube flags as the array grows.
const SCHEME_TUPLES: ReadonlyArray<
  readonly [AppearanceSettings['colorScheme'], string, string, string, string]
> = [
  ['concord', 'Concord Voice', '#0d0821', '#fa709a', '#ffe13f'],
  // Defacto's swatch is grey-dominant (canvas → mid-grey → blue accent) so it
  // reads as a NEUTRAL theme in the picker, not a blue one. These stops drive
  // ONLY the swatch gradient; identity accents live in schemeColors.ts.
  ['defacto', 'Defacto', '#1c1c1f', '#3a3a40', '#58a6ff'],
  ['eclipse', 'Eclipse', '#000000', '#cc0000', '#880000'],
  ['morky', '9th Circle', '#0a0a0a', '#e63946', '#ff6b35'],
  ['foxden', 'Fox Den', '#1a0e02', '#ff6d00', '#ff9100'],
  ['spooky', 'Spooky', '#0a0a0a', '#ff6a00', '#8b20aa'],
  ['driftwood', 'Driftwood', '#120e0a', '#c8a46c', '#a07848'],
  ['grassynill', 'Grassy Nill', '#0c0e08', '#6b8e23', '#8b7355'],
  ['hacker', 'Hacker', '#000000', '#00ff41', '#00ee38'],
  ['leviathan', 'Leviathan', '#020c14', '#0ea5e9', '#06b6d4'],
  ['midnightsky', 'Midnight Sky', '#060a18', '#6d8cff', '#a78bfa'],
  ['bardic', 'Bardic', '#120a1e', '#c471ed', '#f64f8e'],
  ['cottoncandy', 'Cotton Candy', '#0a0810', '#ff6ea8', '#40c8ff'],
  ['agency', 'Agency', '#0d294a', '#e0004e', '#017fa4'],
  // Swatch uses rainbow stops (red → green → violet) to read as Pride; the
  // functional accents live in index.css (hot pink + azure).
  ['pride', 'Pride', '#e40303', '#009e44', '#7a1fa2'],
];

const colorSchemes: {
  value: AppearanceSettings['colorScheme'];
  label: string;
  gradient: string;
}[] = SCHEME_TUPLES.map(([value, label, bg, primary, secondary]) => ({
  value,
  label,
  gradient: `linear-gradient(135deg, ${bg}, ${primary}, ${secondary})`,
}));

const themes: { value: AppearanceSettings['theme']; label: string; icon: React.ReactNode }[] = [
  {
    value: 'dark',
    label: 'Dark',
    icon: (
      <svg width="22" height="22" viewBox="0 0 22 22" fill="none">
        <path
          d="M21 12.79A9 9 0 119.21 1a7 7 0 0011.79 11.79z"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    ),
  },
  {
    value: 'light',
    label: 'Light',
    icon: (
      <svg width="22" height="22" viewBox="0 0 22 22" fill="none">
        <circle cx="11" cy="11" r="5" stroke="currentColor" strokeWidth="1.5" />
        <path
          d="M11 1v2M11 19v2M3.22 3.22l1.42 1.42M17.36 17.36l1.42 1.42M1 11h2M19 11h2M3.22 18.78l1.42-1.42M17.36 4.64l1.42-1.42"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
        />
      </svg>
    ),
  },
  {
    value: 'system',
    label: 'System',
    icon: (
      <svg width="22" height="22" viewBox="0 0 22 22" fill="none">
        <rect x="2" y="3" width="18" height="14" rx="2" stroke="currentColor" strokeWidth="1.5" />
        <path d="M8 21h6M11 17v4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
      </svg>
    ),
  },
];

// ─── Layout Section ─────────────────────────────────────────────────────────
// Lock Interface (#188, per markdrogersjr's spec comment). Wired directly to
// layoutStore (immediate-apply, per-device local persist via Zustand `persist`)
// — the same immediate pattern as ClientBehaviorSection below, NOT the
// draft/Save cycle used by Color Scheme / Theme. Placement (Appearance) and copy
// follow the issue comment verbatim. Exported (like ClientBehaviorSection) so it
// can be unit-tested in isolation against the real layoutStore.
export const LayoutSection: React.FC = () => {
  const interfaceLocked = useLayoutStore((s) => s.interfaceLocked);
  const setInterfaceLocked = useLayoutStore((s) => s.setInterfaceLocked);

  return (
    <CollapsibleSection id="section-layout" title="Layout">
      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Lock Interface</span>
          <span className="settings-row-hint">
            Prevent accidental resizing and panel toggling. This setting applies to this device
            only.
          </span>
        </div>
        <ToggleSwitch checked={interfaceLocked} onChange={setInterfaceLocked} />
      </div>
    </CollapsibleSection>
  );
};

const AppearanceSection: React.FC = () => {
  const appearance = useDraftAppearance();
  // L4 (#1301): the custom color scheme add-circle is a binary premium lock. The
  // PremiumGate keeps the button focusable + aria-disabled (O1); a locked click
  // routes to Subscription instead of opening the custom-colour picker.
  const allowCustomScheme = useEntitlement((e) => e.allowCustomScheme);

  return (
    <>
      <SettingsPreviewPanel />

      <CollapsibleSection id="section-color-scheme" title="Color Scheme">
        <p className="settings-section-description">Pick a color palette for the entire app.</p>
        <span className="color-scheme-active-label">
          {appearance.colorScheme === 'custom'
            ? 'Custom'
            : colorSchemes.find((cs) => cs.value === appearance.colorScheme)?.label}
        </span>
        <div className="color-scheme-grid">
          {colorSchemes.map((cs) => (
            <button
              key={cs.value}
              className={`color-scheme-circle ${appearance.colorScheme === cs.value ? 'selected' : ''}`}
              style={{ background: cs.gradient }}
              onClick={() => setDraftAppearanceSetting('colorScheme', cs.value)}
              title={cs.label}
            />
          ))}
          {/* Custom theme circle — L4 premium lock (#1301). */}
          <PremiumGate
            mode="dim"
            entitled={allowCustomScheme}
            feature="customScheme"
            onActivateSection="custom-scheme"
          >
            <button
              className={`color-scheme-circle custom-add ${appearance.colorScheme === 'custom' ? 'selected' : ''}`}
              style={
                appearance.customColors
                  ? {
                      background: `linear-gradient(135deg, ${appearance.customColors.background}, ${appearance.customColors.accentPrimary}, ${appearance.customColors.accentSecondary})`,
                    }
                  : undefined
              }
              onClick={() => {
                if (appearance.customColors) {
                  setDraftAppearanceSetting('customColors', appearance.customColors);
                  setDraftAppearanceSetting('colorScheme', 'custom');
                } else {
                  const defaults: CustomColors = {
                    background: '#0d0821',
                    accentPrimary: '#fa709a',
                    accentSecondary: '#ffe13f',
                  };
                  setDraftAppearanceSetting('customColors', defaults);
                  setDraftAppearanceSetting('colorScheme', 'custom');
                }
              }}
              title="Custom"
            >
              {!appearance.customColors && (
                <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                  <path
                    d="M8 2v12M2 8h12"
                    stroke="currentColor"
                    strokeWidth="2"
                    strokeLinecap="round"
                  />
                </svg>
              )}
            </button>
          </PremiumGate>
        </div>

        {/* Custom theme color picker panel */}
        {appearance.colorScheme === 'custom' && appearance.customColors && (
          <div className="custom-theme-picker">
            {(() => {
              // Capture narrowed value so the map callbacks can reference it
              // without TypeScript losing the narrowing across the closure.
              const customColors = appearance.customColors;
              return [
                { key: 'background' as const, label: 'Background' },
                { key: 'accentPrimary' as const, label: 'Primary Accent' },
                { key: 'accentSecondary' as const, label: 'Secondary Accent' },
              ].map(({ key, label }) => (
                <div key={key} className="custom-theme-picker-row">
                  <label htmlFor={`custom-theme-${key}`} className="custom-theme-picker-label">
                    {label}
                  </label>
                  <div className="custom-theme-picker-input-group">
                    <input
                      id={`custom-theme-${key}`}
                      type="color"
                      value={customColors[key]}
                      onChange={(e) =>
                        setDraftAppearanceSetting('customColors', {
                          ...customColors,
                          [key]: e.target.value,
                        })
                      }
                      className="custom-theme-color-input"
                    />
                    <input
                      type="text"
                      value={customColors[key]}
                      onChange={(e) => {
                        const val = e.target.value;
                        if (isValidHex(val)) {
                          setDraftAppearanceSetting('customColors', {
                            ...customColors,
                            [key]: val,
                          });
                        }
                      }}
                      className="custom-theme-hex-input"
                      maxLength={7}
                      spellCheck={false}
                    />
                  </div>
                </div>
              ));
            })()}
          </div>
        )}
      </CollapsibleSection>

      <CollapsibleSection id="section-theme" title="Theme">
        <p className="settings-section-description">
          Choose how Concord Voice looks to you. Select a theme or match your system settings.
        </p>
        <div className="theme-selector">
          {themes.map((t) => (
            <button
              key={t.value}
              className={`theme-option ${appearance.theme === t.value ? 'selected' : ''}`}
              onClick={() => setDraftAppearanceSetting('theme', t.value)}
            >
              <div className="theme-option-icon">{t.icon}</div>
              <span className="theme-option-label">{t.label}</span>
            </button>
          ))}
        </div>
      </CollapsibleSection>

      <FontSection />

      {/* Display settings (font size, compact mode, reduce animations) +
          UI scale + high contrast moved to Accessibility per #489. They
          live in `<DisplaySection>` inside AccessibilitySection. */}

      <LayoutSection />
      <ClientBehaviorSection />
    </>
  );
};

export default AppearanceSection;
