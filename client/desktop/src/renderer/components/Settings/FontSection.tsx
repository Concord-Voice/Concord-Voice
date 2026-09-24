import React, { useId } from 'react';
import type { AppearanceSettings } from '../../stores/ui/settingsStore';
import { useDraftAppearance, setDraftAppearanceSetting } from '../../hooks/ui/useDraftSettings';
import {
  resolveFontLayers,
  themeBundledFontFor,
  RESOLVER_CONFIG,
  APP_DEFAULT_FONT,
  CONCORD_DEFAULT_FONT,
  headingsMatchingInterface,
  type HeadingsFontId,
  type AppFontId,
  type FontMode,
} from '../../utils/ui/effectiveFont';
import { useSettingsNavStore } from '../../stores/ui/settingsNavStore';
import CollapsibleSection from './CollapsibleSection';
import './FontSection.css';

// Self-hosted, license-cleared application fonts (see public/branding/Concord-Voice/
// fonts/LICENSES.md). `family` drives only the per-option live PREVIEW; selecting an
// option writes to the draft store, and the resolver (utils/ui/effectiveFont.ts) applies
// the result through the settings store's single font sink. 'default' is "Theme Default":
// the theme's bundled body font where it has one (Agency → Atkinson), else SourceSans —
// its preview family is swapped per theme in the component. "Concord Voice Default" is
// the base fonts kept on every theme. #2366: One Font applies the pick everywhere; Font by
// Area sets Messages, Headings, Navigation and Interface separately.
// The three "…Default" options carry a one-line hint, because on every theme but a
// bundling one Theme Default and Concord Voice Default render identically.
const FONT_OPTIONS: FontOption[] = [
  {
    id: 'default',
    label: 'Theme Default',
    hint: 'Follows your theme',
    family: "'SourceSans', sans-serif",
  },
  {
    id: CONCORD_DEFAULT_FONT,
    label: 'Concord Voice Default',
    hint: "Concord's fonts on every theme",
    family: "'SourceSans', sans-serif",
  },
  {
    id: 'system',
    label: 'System Default',
    hint: "Your operating system's font",
    family: 'system-ui, sans-serif',
  },
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
  hint?: string;
}

// Area lists (#2366): each area's own 'default' entry is prepended below. There
// 'sourcesans' is the face alone, "Source Sans": the brand pairing that "Concord Voice
// Default" names belongs to the Interface choice, not to one area.
const AREA_FONT_OPTIONS: FontOption[] = [
  { id: CONCORD_DEFAULT_FONT, label: 'Source Sans', family: "'SourceSans', sans-serif" },
  ...FONT_OPTIONS.filter((f) => f.id !== APP_DEFAULT_FONT && f.id !== CONCORD_DEFAULT_FONT),
];

type AreaKey = 'fontMessages' | 'fontHeadings' | 'fontNavigation';

interface AreaDef {
  key: AreaKey;
  name: string;
  helper: string;
  hint: string;
}

// Every area's 'default' is "Match Interface" (the resolver's `headingsMatchingInterface`
// for Headings; plain inheritance for the two regions).
const MATCH_LABEL = 'Match Interface';
const REGION_HINT = 'Text uses your Interface font; names and headers use your Headings font.';

// Messages first (most used); Interface ("everything else") is rendered last, below.
const AREAS: AreaDef[] = [
  {
    key: 'fontMessages',
    name: 'Messages',
    helper: 'Chat, DMs, and voice text',
    hint: REGION_HINT,
  },
  {
    key: 'fontHeadings',
    name: 'Headings',
    helper: 'Titles, section headers, and buttons',
    hint: "Uses your Interface font. While Interface is Theme Default, headings use the theme's heading font.",
  },
  {
    key: 'fontNavigation',
    name: 'Navigation',
    helper: 'Server bar, folders, channels, and members',
    hint: REGION_HINT,
  },
];

const LOCKED_FAMILY = "'OpenDyslexic', sans-serif";

// The preview face for a resolved Headings value: the brand display face for Concord
// Voice Default, the theme's display face (as it stands on <html>, unaffected by a
// Headings pick) while Interface is Theme Default, else the picked font.
function headingsPreviewFamily(headings: HeadingsFontId): string {
  if (headings === 'concord') return 'var(--font-stack-concord)';
  if (headings === APP_DEFAULT_FONT) return 'var(--font-brand-stack)';
  return FONT_OPTIONS.find((o) => o.id === headings)?.family ?? 'inherit';
}

interface FontOptionListProps {
  legend: string;
  options: FontOption[];
  activeId: AppFontId;
  inUseId: AppFontId | null;
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
  inUseId,
  locked,
  lockNoteId,
  onSelect,
}) => {
  const listId = useId();
  return (
    <fieldset className="font-option-list">
      <legend className="font-option-legend">{legend}</legend>
      {options.map((f) => {
        const hintId = `${listId}-${f.id}-hint`;
        const chipId = `${listId}-${f.id}-in-use`;
        const inUse = inUseId === f.id;
        // The hint and the chip are the option's DESCRIPTION, not its name: both are
        // aria-hidden inside the button (so the name stays the visible label alone) and
        // referenced here, which the accessible-description computation still reads.
        const describedBy = [locked && lockNoteId, f.hint && hintId, inUse && chipId]
          .filter(Boolean)
          .join(' ');
        return (
          <button
            key={f.id}
            type="button"
            className={`font-option ${activeId === f.id ? 'selected' : ''} ${
              locked ? 'font-option--locked' : ''
            }`}
            aria-pressed={activeId === f.id}
            aria-disabled={locked || undefined}
            aria-describedby={describedBy || undefined}
            onClick={() => {
              if (locked) return; // activation guard — never write a font while locked
              onSelect(f.id);
            }}
          >
            <span className="font-option-label">
              {f.label}
              {inUse && (
                <span className="font-option-in-use" id={chipId} aria-hidden="true">
                  Active with the current theme
                </span>
              )}
            </span>
            {f.hint && (
              <span className="font-option-hint" id={hintId} aria-hidden="true">
                {f.hint}
              </span>
            )}
            <span
              className="font-option-sample"
              style={{ fontFamily: f.family }}
              aria-hidden="true"
            >
              The quick brown fox jumps over the lazy dog
            </span>
          </button>
        );
      })}
    </fieldset>
  );
};

// A saved setting, so native radios — not tabs — styled as the settings-mode pills.
// The selected pill is highlighted by SettingsPage.css's
// `.settings-mode-pill:has(.settings-mode-radio:checked)`, so the input must carry that
// class; a class of our own on the <label> styles nothing.
const FontModeToggle: React.FC<{ mode: FontMode; locked: boolean; lockNoteId: string }> = ({
  mode,
  locked,
  lockNoteId,
}) => (
  <fieldset className="settings-mode-toggle font-mode-toggle">
    <legend className="settings-mode-legend">Font mode</legend>
    {(
      [
        ['one', 'One Font'],
        ['area', 'Font by Area'],
      ] as const
    ).map(([value, label]) => (
      <label key={value} className="settings-mode-pill">
        <input
          type="radio"
          name="font-mode"
          className="settings-mode-radio"
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
  // Like the codec list's Preferred / In Use: the selected option is what is saved, and
  // when that choice is dynamic (Theme Default) a chip marks the option actually applied.
  const appliedId = layers.interface === APP_DEFAULT_FONT ? CONCORD_DEFAULT_FONT : layers.interface;
  const inUseId = !locked && appliedId !== appFont ? appliedId : null;
  const lockNoteId = useId();
  const requestFocus = useSettingsNavStore((s) => s.requestFocus);
  const areaPicksSaved = AREAS.some((a) => appearance[a.key] !== APP_DEFAULT_FONT);

  // Each default must preview what it applies. "Theme Default" changes with a bundling
  // theme; Headings' "Match Interface" follows `headingsMatchingInterface`.
  const headingsMatchFamily = headingsPreviewFamily(
    headingsMatchingInterface(appFont, layers.lockReason)
  );
  const themeDefaultFamily =
    FONT_OPTIONS.find((f) => f.id === themeBundledFont)?.family ?? FONT_OPTIONS[0].family;
  const interfaceOptions = FONT_OPTIONS.map((f) =>
    f.id === APP_DEFAULT_FONT ? { ...f, family: themeDefaultFamily } : f
  );

  const interfaceList = (legend: string) => (
    <FontOptionList
      legend={legend}
      options={interfaceOptions}
      activeId={appFont}
      inUseId={inUseId}
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
            const family = area.key === 'fontHeadings' ? headingsMatchFamily : 'inherit';
            const options: FontOption[] = [
              { id: APP_DEFAULT_FONT, label: MATCH_LABEL, family },
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
                  inUseId={null}
                  locked={locked}
                  lockNoteId={lockNoteId}
                  onSelect={(id) => setDraftAppearanceSetting(area.key, id)}
                />
                <p className="font-area-hint">{area.hint}</p>
              </FontAreaRow>
            );
          })}
          <FontAreaRow
            name="Interface"
            helper="Settings, labels, and everything else"
            current={interfaceOptions.find((o) => o.id === appFont) ?? interfaceOptions[0]}
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
