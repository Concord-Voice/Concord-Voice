import { useId } from 'react';
import { useWindowFocus } from '../../hooks/ui/useWindowFocus';
import { describeGifPlayback, type GifPlaybackMode } from '../../utils/ui/gifPlayback';
import './GifPlaybackControl.css';

const OPTIONS: { value: GifPlaybackMode; label: string }[] = [
  { value: 'auto', label: 'Auto' },
  { value: 'always', label: 'Always' },
  { value: 'hover', label: 'Hover only' },
];

interface GifPlaybackControlProps {
  readonly mode: GifPlaybackMode;
  readonly reduceAnimations: boolean;
  readonly onChange: (mode: GifPlaybackMode) => void;
}

/**
 * Settings ▸ Accessibility ▸ Display — the GIF playback tri-state (#2369).
 *
 * WHY NATIVE RADIOS RATHER THAN THE `<button>` GROUPS ELSEWHERE IN SETTINGS
 * (`.font-option`, `.theme-option`): neither gives a single choice its semantics,
 * which is a live WCAG 4.1.2 gap in the precedent. `.theme-option` has no group
 * role and no state at all; `.font-option` has a fieldset but marks the pick with
 * `aria-pressed`, which announces independent toggles. The accessibility floor
 * is not a tradeoff input, so this control does not inherit the gap — a
 * `<fieldset>` of real radios gets the group semantics, the checked state and
 * arrow-key navigation from the platform for free. `.font-size-option` was the
 * third group listed here; #2367 rebuilt it as radios when it moved Font Size to
 * Appearance, so two remain. Retrofitting those was deliberately out of scope
 * here (spec §5 residual 5) and this component touches none of them. The
 * drawing is identical either way.
 *
 * The hint is a `role="status"` region referenced by the fieldset's
 * `aria-describedby` because its text changes as a side effect of a DIFFERENT
 * control — toggling Reduce Animations rewrites it while `gifPlayback` is
 * `'auto'`. That is the WCAG 3.2.x surprise case and has to be announced.
 *
 * There is no lock affordance and no `aria-disabled`: nothing here is ever
 * locked. Reduce Animations is FOLLOWED by `'auto'`, never enforced over an
 * explicit pick — see `REDUCE_MOTION_WRITES_GIF_PLAYBACK`. A lock glyph on an
 * unlocked control is a false affordance.
 */
export default function GifPlaybackControl({
  mode,
  reduceAnimations,
  onChange,
}: GifPlaybackControlProps) {
  const hintId = useId();
  // Subscribed rather than read once: the hint's unfocused suffix is live.
  const windowFocused = useWindowFocus();

  return (
    <div className="settings-row">
      <div className="settings-row-info">
        <span className="settings-row-label">GIF Playback</span>
        {/* Describes the RESOLVED behaviour, not the enum. That is what keeps
            'Auto' and 'Always' legible while Reduce Animations is off, where
            the two are behaviourally identical and the enum alone cannot say
            so (spec §5 residual 6). */}
        <output className="settings-row-hint" id={hintId}>
          {describeGifPlayback({ mode, reduceAnimations, windowFocused })}
        </output>
      </div>
      <fieldset className="gif-playback-fieldset" aria-describedby={hintId}>
        <legend className="gif-playback-legend">GIF playback</legend>
        {OPTIONS.map((option) => (
          <label
            key={option.value}
            className={`gif-playback-option ${mode === option.value ? 'selected' : ''}`}
          >
            <input
              type="radio"
              name="gif-playback"
              className="gif-playback-radio"
              value={option.value}
              checked={mode === option.value}
              onChange={() => onChange(option.value)}
            />
            {option.label}
          </label>
        ))}
      </fieldset>
    </div>
  );
}
