import React from 'react';
import { LayoutGrid, Focus } from 'lucide-react';
import { useVoiceStore, type VoiceViewMode } from '../../stores/voiceStore';
import './VoiceViewSwitch.css';

const OPTIONS: ReadonlyArray<{ mode: VoiceViewMode; label: string; icon: React.ReactNode }> = [
  { mode: 'tile', label: 'Tile view', icon: <LayoutGrid size={16} /> },
  { mode: 'front-center', label: "Front 'n Center", icon: <Focus size={16} /> },
];

/**
 * Segmented Tile ↔ Front 'n Center layout switch, overlaid in the top-left of
 * the voice area (Zoom/Meet-style, over the content it reorganizes). Both modes
 * are always visible with the active one highlighted, so it reads as *state*
 * rather than a destination-labeled action — no danger color, no swapping label
 * that inverts the meaning (the confusions of the old bar button, #2059-era).
 *
 * VoiceView renders this only while a stream is tuned in — the only time both
 * modes exist (Front 'n Center is stream-only).
 */
const VoiceViewSwitch: React.FC = () => {
  const voiceViewMode = useVoiceStore((s) => s.voiceViewMode);
  const setVoiceViewMode = useVoiceStore((s) => s.setVoiceViewMode);

  return (
    // Native <fieldset> (implicit role="group") + visually-hidden <legend> for
    // the group name, per Sonar S6819 (prefer native semantics over a role attr
    // on a <div>). Mirrors the segmented-control precedent in
    // PresenceSettingsSection; the .css resets the fieldset's default chrome.
    <fieldset className="voice-view-switch">
      <legend className="voice-view-switch__legend">Voice layout</legend>
      {OPTIONS.map(({ mode, label, icon }) => {
        const active = voiceViewMode === mode;
        return (
          <button
            key={mode}
            type="button"
            className={`voice-view-switch__seg${active ? ' voice-view-switch__seg--active' : ''}`}
            aria-pressed={active}
            aria-label={label}
            title={label}
            onClick={() => setVoiceViewMode(mode)}
          >
            {icon}
          </button>
        );
      })}
    </fieldset>
  );
};

export default VoiceViewSwitch;
