import React, { useEffect, useId, useState } from 'react';
import { TRAY_ICON_STYLES, type TrayIconStyle } from '../../../shared/trayStatus';
import { useDraftAppearance, setDraftAppearanceSetting } from '../../hooks/ui/useDraftSettings';
import CollapsibleSection from './CollapsibleSection';
import templatePreview from '../../assets/tray-preview/template.png';
import whitePreview from '../../assets/tray-preview/white.png';
import blackPreview from '../../assets/tray-preview/black.png';
import colourPreview from '../../assets/tray-preview/colour.png';
import './TrayIconSection.css';

interface StyleOption {
  value: TrayIconStyle;
  label: string;
  preview: string;
  /** Neutral swatch behind the glyph so each preview reads (template is black on clear). */
  swatch: string;
}

const OPTIONS: readonly StyleOption[] = [
  { value: 'auto', label: 'Automatic', preview: templatePreview, swatch: '#8a8a90' },
  { value: 'light', label: 'Light', preview: whitePreview, swatch: '#2b2b30' },
  { value: 'dark', label: 'Dark', preview: blackPreview, swatch: '#e6e6ea' },
  { value: 'colour', label: 'Colour', preview: colourPreview, swatch: '#8a8a90' },
];

function isTrayIconStyle(v: unknown): v is TrayIconStyle {
  return typeof v === 'string' && (TRAY_ICON_STYLES as readonly string[]).includes(v);
}

/**
 * Settings ▸ Appearance ▸ Tray Icon / Menu Bar Icon (tray status spec §2a).
 * Native radios in a fieldset: checked state, group semantics and arrow keys come from the
 * platform. Each preview image is decorative (`alt=""`); the label is the accessible name.
 */
const TrayIconSection: React.FC = () => {
  const appearance = useDraftAppearance();
  const [isMac, setIsMac] = useState(false);
  const baseId = useId();

  useEffect(() => {
    let mounted = true;
    void globalThis.electron
      ?.getPlatform?.()
      .then((platform) => {
        if (mounted) setIsMac(platform === 'darwin');
      })
      .catch(() => {
        if (mounted) setIsMac(false);
      });
    return () => {
      mounted = false;
    };
  }, []);

  const active: TrayIconStyle = isTrayIconStyle(appearance.trayIconStyle)
    ? appearance.trayIconStyle
    : 'auto';
  const where = isMac ? 'menu bar' : 'system tray';
  const autoHelp = isMac ? 'Matches your menu bar and shows status colours.' : 'Full colour.';

  return (
    <CollapsibleSection id="section-tray-icon" title={isMac ? 'Menu Bar Icon' : 'Tray Icon'}>
      <p className="settings-section-description">
        {`Choose how the Concord Voice icon looks in your ${where}.`}
      </p>
      <fieldset className="tray-icon-fieldset">
        <legend className="tray-icon-legend">
          {isMac ? 'Menu bar icon style' : 'Tray icon style'}
        </legend>
        <div className="tray-icon-options">
          {OPTIONS.map((o) => (
            <label
              key={o.value}
              className={`tray-icon-option ${active === o.value ? 'selected' : ''}`}
            >
              <input
                type="radio"
                name="tray-icon-style"
                className="tray-icon-radio"
                value={o.value}
                aria-labelledby={`${baseId}-${o.value}-label`}
                aria-describedby={o.value === 'auto' ? `${baseId}-auto-help` : undefined}
                checked={active === o.value}
                onChange={() => setDraftAppearanceSetting('trayIconStyle', o.value)}
              />
              <span className="tray-icon-swatch" style={{ background: o.swatch }}>
                <img src={o.preview} alt="" width={32} height={32} />
              </span>
              <span className="tray-icon-label" id={`${baseId}-${o.value}-label`}>
                {o.label}
              </span>
              {o.value === 'auto' && (
                <span className="tray-icon-help" id={`${baseId}-auto-help`}>
                  {autoHelp}
                </span>
              )}
            </label>
          ))}
        </div>
      </fieldset>
    </CollapsibleSection>
  );
};

export default TrayIconSection;
