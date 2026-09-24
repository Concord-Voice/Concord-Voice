import React, { useId } from 'react';
import type { AppearanceSettings } from '../../stores/ui/settingsStore';
import { useDraftAppearance, setDraftAppearanceSetting } from '../../hooks/ui/useDraftSettings';
import {
  resolveFontLayers,
  themeBundledFontFor,
  RESOLVER_CONFIG,
  APP_DEFAULT_FONT,
  type AppFontId,
  type FontMode,
} from '../../utils/ui/effectiveFont';
import { useSettingsNavStore } from '../../stores/ui/settingsNavStore';
import CollapsibleSection from './CollapsibleSection';
import './FontSection.css';

// Self-hosted, license-cleared application fonts (see public/branding/Concord-Voice/
// fonts/LICENSES.md). `family` drives only the per-option live PREVIEW; selecting an
// option writes to the draft store, and the resolver (utils/ui/effectiveFont.ts) applies
// the result through the settings store's single font sink. 'default' previews the base
// body face (SourceSans). #2366: One Font applies the pick everywhere; Font by Area sets
// Messages, Headings, Navigation and Interface separately.
const FONT_OPTIONS: { id: AppearanceSettings['appFont']; label: string; family: string }[] = [
  { id: 'default', label: 'Concord Voice Default', family: "'SourceSans', sans-serif" },
  { id: 'system', label: 'System Default', family: 'system-ui, sans-serif' },
  { id: 'opendyslexic', label: 'OpenDyslexic', family: "'OpenDyslexic', sans-serif" },
  { id: 'inter', label: 'Inter', family: "'Inter', sans-serif" },
  { id: 'lexend', label: 'Lexend', family: "'Lexend', sans-serif" },
  { id: 'lato', label: 'Lato', family: "'Lato', sans-serif" },
  {
    id: 'atkinson',
    label: 'Atkinson Hyperlegible Next',
    family: "'Atkinson Hyperlegible Next', sans-serif",
  },
];

// The discrete size steps. `--font-scale-discrete` for each lives in index.css
// (`[data-fontsize='…']`) and compounds with UI Scale — page zoom on a shell with
// the zoom bridge, `--ui-scale` on an older one (#2367 part 2).
const FONT_SIZE_OPTIONS: { value: AppearanceSettings['fontSize']; label: string }[] = [
  { value: 'small', label: 'Small' },
  { value: 'default', label: 'Default' },
  { value: 'large', label: 'Large' },
];

interface FontOption {
  id: AppFontId;
  label: string;
  family: string;
}

// Area lists (#2366): each area's own 'default' entry is prepended below; 'sourcesans'
// is the explicit brand body face, because 'default' is the no-pick sentinel.
const AREA_FONT_OPTIONS: FontOption[] = [
  { id: 'sourcesans', label: 'Source Sans', family: "'SourceSans', sans-serif" },
  ...FONT_OPTIONS.filter((f) => f.id !== APP_DEFAULT_FONT),
];

type AreaKey = 'fontMessages' | 'fontHeadings' | 'fontNavigation';

interface AreaDef {
  key: AreaKey;
  name: string;
  helper: string;
  defaultLabel: string;
  defaultFamily: string;
  hint?: string;
}

const MATCH_HINT = 'Text follows Interface; names and headers follow Headings.';

// Messages first (most used); Interface ("everything else") is rendered last, below.
const AREAS: AreaDef[] = [
  {
    key: 'fontMessages',
    name: 'Messages',
    helper: 'Chat, DMs, and voice text',
    defaultLabel: 'Match the app',
    defaultFamily: 'inherit',
    hint: MATCH_HINT,
  },
  {
    key: 'fontHeadings',
    name: 'Headings',
    helper: 'Titles, section headers, and buttons',
    defaultLabel: 'Theme default',
    // The theme's display face as it stands on <html> — unaffected by a Headings pick.
    defaultFamily: 'var(--font-brand-stack)',
  },
  {
    key: 'fontNavigation',
    name: 'Navigation',
    helper: 'Server bar, folders, channels, and members',
    defaultLabel: 'Match the app',
    defaultFamily: 'inherit',
    hint: MATCH_HINT,
  },
];

const LOCKED_FAMILY = "'OpenDyslexic', sans-serif";

interface FontOptionListProps {
  legend: string;
  options: FontOption[];
  activeId: AppFontId;
  badgeId: AppFontId | null;
  locked: boolean;
  lockNoteId: string;
  onSelect: (id: AppFontId) => void;
}

// The option list the picker has always rendered, now shared by every list. Options stay
// native <button>s with aria-disabled (never `disabled`) so a locked option is still
// tabbable and announced (WCAG 1.3.1/3.3.2); the activation guard is the real lock
// (#1644 Q2-restore). <fieldset>/<legend> give group semantics without an ARIA role.
const FontOptionList: React.FC<FontOptionListProps> = ({
  legend,
  options,
  activeId,
  badgeId,
  locked,
  lockNoteId,
  onSelect,
}) => (
  <fieldset className="font-option-list">
    <legend className="font-option-legend">{legend}</legend>
    {options.map((f) => (
      <button
        key={f.id}
        type="button"
        className={`font-option ${activeId === f.id ? 'selected' : ''} ${
          locked ? 'font-option--locked' : ''
        }`}
        aria-pressed={activeId === f.id}
        aria-disabled={locked || undefined}
        aria-describedby={locked ? lockNoteId : undefined}
        onClick={() => {
          if (locked) return; // activation guard — never write a font while locked
          onSelect(f.id);
        }}
      >
        <span className="font-option-label">
          {f.label}
          {badgeId === f.id && (
            <span className="font-option-theme-badge">Provided by the active theme</span>
          )}
        </span>
        <span className="font-option-sample" style={{ fontFamily: f.family }} aria-hidden="true">
          The quick brown fox jumps over the lazy dog
        </span>
      </button>
    ))}
  </fieldset>
);

// A saved setting, so native radios — not tabs — styled as the settings-mode pills.
const FontModeToggle: React.FC<{ mode: FontMode; locked: boolean; lockNoteId: string }> = ({
  mode,
  locked,
  lockNoteId,
}) => (
  <fieldset className="settings-mode-toggle font-mode-toggle">
    <legend className="font-option-legend">Font mode</legend>
    {(
      [
        ['one', 'One Font'],
        ['area', 'Font by Area'],
      ] as const
    ).map(([value, label]) => (
      <label key={value} className={`settings-mode-pill ${mode === value ? 'active' : ''}`}>
        <input
          type="radio"
          name="font-mode"
          className="font-mode-radio"
          value={value}
          checked={mode === value}
          aria-disabled={locked || undefined}
          aria-describedby={locked ? lockNoteId : undefined}
          onChange={() => {
            if (locked) return; // activation guard
            setDraftAppearanceSetting('fontMode', value);
          }}
        />
        {label}
      </label>
    ))}
  </fieldset>
);

interface FontAreaRowProps {
  name: string;
  helper: string;
  current: FontOption;
  locked: boolean;
  children: React.ReactNode;
}

// `name` makes the rows an exclusive group natively: opening one closes the others.
const FontAreaRow: React.FC<FontAreaRowProps> = ({ name, helper, current, locked, children }) => (
  <details className="font-area-row" name="font-areas">
    <summary className="font-area-summary">
      <span className="font-area-name">{name}</span>
      <span className="font-area-helper">{helper}</span>
      <span
        className="font-area-current"
        style={{ fontFamily: locked ? LOCKED_FAMILY : current.family }}
      >
        {locked ? 'OpenDyslexic · Locked' : current.label}
      </span>
    </summary>
    <div className="font-area-body">{children}</div>
  </details>
);

const FontSection: React.FC = () => {
  const appearance = useDraftAppearance();
  const { appFont, dyslexicSupport, colorScheme, fontMode } = appearance;

  // The same resolver the settingsStore subscriber runs, over DRAFT state (live preview).
  const themeBundledFont = themeBundledFontFor(colorScheme);
  const layers = resolveFontLayers(
    {
      dyslexicSupport,
      appFont,
      themeBundledFont,
      fontMode,
      fontHeadings: appearance.fontHeadings,
      fontNavigation: appearance.fontNavigation,
      fontMessages: appearance.fontMessages,
    },
    RESOLVER_CONFIG
  );
  // #1644 dyslexic HARD lock: every control is aria-disabled with an activation guard.
  const locked = layers.lockReason === 'dyslexic';
  // Under the theme's soft lock the active option is the theme font; otherwise the pick.
  const interfaceActive = layers.lockReason === 'theme' ? layers.interface : appFont;
  const lockNoteId = useId();
  const requestFocus = useSettingsNavStore((s) => s.requestFocus);
  const areaPicksSaved = AREAS.some((a) => appearance[a.key] !== APP_DEFAULT_FONT);

  const interfaceList = (legend: string) => (
    <FontOptionList
      legend={legend}
      options={FONT_OPTIONS}
      activeId={interfaceActive}
      badgeId={themeBundledFont}
      locked={locked}
      lockNoteId={lockNoteId}
      onSelect={(id) => setDraftAppearanceSetting('appFont', id)}
    />
  );

  return (
    <CollapsibleSection id="section-fonts" title="Application Font">
      <p className="settings-section-description">
        Choose the font used across the app — including OpenDyslexic, designed to improve
        readability for people with dyslexia.
      </p>
      <FontModeToggle mode={fontMode} locked={locked} lockNoteId={lockNoteId} />
      {/* Always mounted so a screen reader announces it when it fills. */}
      <output className="settings-mode-notice font-mode-notice">
        {fontMode === 'one' && areaPicksSaved
          ? 'Your per-area choices are saved — switch back to Font by Area to use them again.'
          : ''}
      </output>

      {fontMode === 'one' ? (
        interfaceList('Application font')
      ) : (
        <div className="font-area-list">
          <p className="font-area-intro">
            An area with its own font uses it for everything inside it, including names and headers.
          </p>
          {AREAS.map((area) => {
            const options: FontOption[] = [
              { id: APP_DEFAULT_FONT, label: area.defaultLabel, family: area.defaultFamily },
              ...AREA_FONT_OPTIONS,
            ];
            const activeId = appearance[area.key];
            const current = options.find((o) => o.id === activeId) ?? options[0];
            return (
              <FontAreaRow
                key={area.key}
                name={area.name}
                helper={area.helper}
                current={current}
                locked={locked}
              >
                <FontOptionList
                  legend={`${area.name} font`}
                  options={options}
                  activeId={activeId}
                  badgeId={
                    area.key === 'fontHeadings' && themeBundledFont ? APP_DEFAULT_FONT : null
                  }
                  locked={locked}
                  lockNoteId={lockNoteId}
                  onSelect={(id) => setDraftAppearanceSetting(area.key, id)}
                />
                {area.hint && <p className="font-area-hint">{area.hint}</p>}
              </FontAreaRow>
            );
          })}
          <FontAreaRow
            name="Interface"
            helper="Settings, labels, and everything else"
            // 'sourcesans' is valid on every layer but offered only in area lists, so a
            // stored or synced one still needs its own label here.
            current={
              [...FONT_OPTIONS, ...AREA_FONT_OPTIONS].find((o) => o.id === interfaceActive) ??
              FONT_OPTIONS[0]
            }
            locked={locked}
          >
            {interfaceList('Interface font')}
          </FontAreaRow>
        </div>
      )}

      {locked && (
        <p id={lockNoteId} className="font-option-lock-note">
          Font selection is managed by <strong>Enable Dyslexic Support</strong> (Accessibility ▸
          Display).{' '}
          <button
            type="button"
            className="font-option-lock-link"
            onClick={() => requestFocus('accessibility', 'toggle-dyslexic-support')}
          >
            Go to Accessibility ▸ Display
          </button>
        </p>
      )}

      {/* Font Size lives beside the typeface because both answer "how does text look"
          (#2367 — it moved to Accessibility with #489; the discrete size is an everyday
          display preference, while the continuous UI Scale slider stays in
          Accessibility). It sits OUTSIDE the font lists above on purpose: Dyslexic
          Support locks the TYPEFACE, and a size has nothing to do with typeface, so
          turning the accommodation on must never lock text size too.

          Native radios in a fieldset rather than the `<button>` group this used to be.
          The buttons carried their selected state in a CSS class alone — no
          `aria-checked`, no group role — the WCAG 4.1.2 gap GifPlaybackControl.tsx
          records for this exact class family. Radios get the checked state, the group
          semantics and arrow-key movement from the platform. */}
      <fieldset className="font-size-fieldset">
        <legend className="form-label">Font Size</legend>
        <div className="font-size-selector">
          {FONT_SIZE_OPTIONS.map((fs) => (
            <label
              key={fs.value}
              className={`font-size-option ${fs.value} ${appearance.fontSize === fs.value ? 'selected' : ''}`}
            >
              <input
                type="radio"
                name="font-size"
                className="font-size-radio"
                value={fs.value}
                checked={appearance.fontSize === fs.value}
                onChange={() => setDraftAppearanceSetting('fontSize', fs.value)}
              />
              {fs.label}
            </label>
          ))}
        </div>
      </fieldset>
    </CollapsibleSection>
  );
};

export default FontSection;
