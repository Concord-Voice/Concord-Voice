import React from 'react';
import { MonitorUp, MonitorOff } from 'lucide-react';
import './ShareSegmentedControl.css';

interface ShareSegmentedControlProps {
  /** Pick a different screen or window without dropping the share. */
  onSwitch: () => void;
  /** End the share. */
  onStop: () => void;
}

/**
 * Switch and Stop act on the same live screen share, but the bar used to render
 * them as two unrelated pills with Chat sitting between them — a Proximity
 * failure, since nothing said they were related and the two most-confusable
 * share actions were the furthest apart.
 *
 * They are one object here. Only the outer corners round and a seam divides the
 * halves (Uniform Connectedness), so the pair reads as a single control whose
 * right half is destructive.
 *
 * TWO REAL BUTTONS, deliberately not a radiogroup with roving tabindex. Neither
 * half is a selected *state* — both are actions — and roving tabindex would make
 * the second half unreachable by Tab, which is a real accessibility cost paid for
 * a semantic that does not apply.
 *
 * Rendered only while a share is live, so neither half is ever disabled: there is
 * nothing to switch or stop otherwise, and the caller renders a plain Screen
 * button in that state instead.
 */
export const ShareSegmentedControl: React.FC<ShareSegmentedControlProps> = ({
  onSwitch,
  onStop,
}) => (
  // <fieldset>, not a div with role="group" (sonar typescript:S6819). The native
  // element carries an implicit group role with better assistive-technology
  // support than the ARIA attribute, and buttons are form controls, so a set of
  // two acting on one share is what a fieldset is for. Named by aria-label rather
  // than a <legend>: a legend renders as visible text, and this control's meaning
  // is already carried by the two button labels. Its UA border/margin/padding are
  // reset in the stylesheet.
  <fieldset className="share-segmented" aria-label="Screen share">
    <button
      type="button"
      className="voice-controls__btn share-segmented__half share-segmented__half--switch"
      onClick={onSwitch}
      title="Switch to a different screen or window without stopping"
    >
      <MonitorUp size={18} />
      <span className="voice-controls__btn-label">Switch</span>
    </button>
    <button
      type="button"
      className="voice-controls__btn voice-controls__btn--danger-soft share-segmented__half share-segmented__half--stop"
      onClick={onStop}
      title="Stop Sharing"
    >
      <MonitorOff size={18} />
      <span className="voice-controls__btn-label">Stop</span>
    </button>
  </fieldset>
);
