import { FriendRequestPrivacyMode } from '../../stores/ui/privacyStore';

/**
 * Most restrictive on the LEFT, matching the shipped DMPrivacyControls axis
 * ('No One' at index 0). The slider index is presentation; the enum is the
 * value, and setMode is always called with the enum.
 */
const FR_MODES: readonly FriendRequestPrivacyMode[] = ['nobody', 'mutual_servers', 'everyone'];

const FR_TITLES: Record<FriendRequestPrivacyMode, string> = {
  nobody: 'No One',
  mutual_servers: 'Mutual Servers',
  everyone: 'Everyone',
};

/**
 * Straight register, deliberately. The adjacent DM descriptions are playful
 * ("Hermit mode", "Bold move"); this control is reached after an unwanted-contact
 * event, where that voice would land badly. The tonal seam is an accepted cost.
 */
const FR_DESCRIPTIONS: Record<FriendRequestPrivacyMode, string> = {
  nobody: 'No one can send you a friend request.',
  mutual_servers: 'Only people who share a server with you can send you a friend request.',
  everyone: 'Anyone can send you a friend request.',
};

const FR_LABEL_ID = 'friend-request-privacy-label';

interface FriendRequestPrivacyControlsProps {
  localMode: FriendRequestPrivacyMode;
  setMode: (mode: FriendRequestPrivacyMode) => void;
  saveError: string | null;
  /**
   * The server has confirmed the user's settings at least once. Until then the
   * value shown would be the store's permissive default, not the user's choice,
   * and presenting a wrong-and-more-permissive mode on a privacy control is a
   * misrepresentation — briefly if the fetch is in flight, and indefinitely if
   * it failed.
   */
  isLoaded: boolean;
}

const FriendRequestPrivacyControls = ({
  localMode,
  setMode,
  saveError,
  isLoaded,
}: FriendRequestPrivacyControlsProps) => {
  // Defence in depth: the store narrows the wire value, so -1 should be
  // unreachable. If it ever is reached, clamping keeps the thumb, the tick and
  // aria-valuetext describing the SAME mode rather than disagreeing.
  const rawIndex = FR_MODES.indexOf(localMode);
  const index = rawIndex === -1 ? FR_MODES.indexOf('everyone') : rawIndex;
  const displayMode = FR_MODES[index];
  // Until the server confirms, NOTHING may be presented as selected. The choice
  // labels still render — they are the options, not an assertion about the
  // user — but marking one active would advertise the store's permissive
  // default as though it were the user's own setting.
  const activeIndex = isLoaded ? index : -1;

  return (
    // Native <fieldset> rather than <div role="group"> (S6819). Deliberately no
    // <legend>: the group is named by aria-label, so the visible label stays the
    // single text node in the DOM and the legend never has to participate in the
    // flex layout. A visible legend would be a bet on whether <legend> is a flex
    // item in the shipped Chromium (float is inert if it is, spacing is lost if
    // it is not), and a hidden one would duplicate the label text.
    <fieldset
      className="settings-tier-slider-container"
      aria-label="Who Can Send You Friend Requests"
      aria-busy={!isLoaded}
    >
      <span className="settings-row-label" id={FR_LABEL_ID}>
        Who Can Send You Friend Requests
      </span>

      <div className="settings-tier-labels">
        {FR_MODES.map((mode, i) => (
          <button
            type="button"
            key={mode}
            className={`settings-tier-label ${activeIndex === i ? 'active' : ''}`}
            aria-pressed={activeIndex === i}
            disabled={!isLoaded}
            onClick={() => setMode(mode)}
          >
            {FR_TITLES[mode]}
          </button>
        ))}
      </div>

      <div className="settings-tier-track friend-request-track">
        <div className="settings-tier-ticks">
          {FR_MODES.map((mode, i) => (
            <span
              key={`fr-tick-${mode}`}
              className={`settings-tier-tick ${activeIndex === i ? 'active' : ''}`}
            />
          ))}
        </div>
        {/*
          aria-labelledby AND aria-valuetext are both required, and neither is
          copied from the precedent. Without the former the range has no
          accessible name at all (the WCAG 4.1.2 failure DMPrivacyControls
          ships); without the latter it announces its value as "1".
        */}
        <input
          type="range"
          className="settings-tier-slider"
          min={0}
          max={FR_MODES.length - 1}
          step={1}
          value={index}
          disabled={!isLoaded}
          aria-labelledby={FR_LABEL_ID}
          aria-valuetext={isLoaded ? FR_TITLES[displayMode] : 'Loading'}
          onChange={(e) => setMode(FR_MODES[Number(e.target.value)])}
        />
      </div>

      <div className="settings-tier-description" aria-live="polite">
        {/* No value until the server has confirmed one. Rendering the store
            default here would state, with the authority of the UI, a more
            permissive setting than the user may actually hold. */}
        <span>{isLoaded ? FR_TITLES[displayMode] : 'Loading your setting…'}</span>
        <span>
          {isLoaded ? FR_DESCRIPTIONS[displayMode] : 'Your current setting has not loaded yet.'}
        </span>
        {isLoaded && displayMode === 'nobody' && (
          <span className="settings-row-hint">
            Requests already in your inbox aren&apos;t affected — you can still accept them. This
            only blocks new ones.
          </span>
        )}
      </div>

      {saveError && (
        <span className="settings-row-hint" role="alert">
          {saveError}
        </span>
      )}
    </fieldset>
  );
};

export default FriendRequestPrivacyControls;
