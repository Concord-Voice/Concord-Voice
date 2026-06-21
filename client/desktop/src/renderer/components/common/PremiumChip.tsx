import React from 'react';
import './PremiumChip.css';

export interface PremiumChipProps {
  /**
   * The trailing label after the 🔒 glyph + the word "Premium". Defaults to
   * nothing — the chip always renders the literal word "Premium" (a11y P2:
   * locked-ness is conveyed by the glyph AND the word, never colour alone), so
   * `label` is for an OPTIONAL extra hint (e.g. "High/Hi-Fi/Studio").
   */
  label?: string;
  /**
   * Whether the affordance represents a locked state. Drives the 🔒 glyph; the
   * chip is always "Premium"-labelled. Defaults to `true` (a chip is a lock
   * affordance — there is no unlocked variant of it).
   */
  locked?: boolean;
  /**
   * When provided, the chip is an interactive native `<button>` that calls this
   * on click + keyboard (the host wires Enter/Space). When omitted, the chip is
   * a non-interactive `<span>` (decorative/inline label only). Never a
   * role+tabindex span (Sonar S6819 / a11y).
   */
  onActivate?: (event?: React.MouseEvent | React.KeyboardEvent) => void;
  /** Optional id so a gated control's `aria-describedby` can point at this chip. */
  id?: string;
  /** Extra class for host-specific positioning (e.g. overlay vs inline). */
  className?: string;
}

/** The lock glyph + accessible name, shared by both the button and span shapes. */
function ChipBody({
  locked,
  label,
}: Readonly<{ locked: boolean; label?: string }>): React.JSX.Element {
  return (
    <>
      {locked && (
        <span className="premium-chip__glyph" aria-label="Premium feature" role="img">
          {'\u{1F512}'}
        </span>
      )}
      <span className="premium-chip__word">Premium</span>
      {label ? <span className="premium-chip__label">{label}</span> : null}
    </>
  );
}

/**
 * The universal lock affordance (#1301). A yellow pill extending
 * `.settings-quality-premium-badge`, carrying a 🔒 glyph + the literal word
 * "Premium". Interactive (native `<button>`) when `onActivate` is supplied —
 * routing to the Subscription page — otherwise a static inline `<span>`.
 */
const PremiumChip: React.FC<PremiumChipProps> = ({
  label,
  locked = true,
  onActivate,
  id,
  className,
}) => {
  const extraClass = className ? ` ${className}` : '';
  const rootClass = `premium-chip${extraClass}`;

  if (onActivate) {
    return (
      <button
        type="button"
        id={id}
        className={`${rootClass} premium-chip--interactive`}
        onClick={(e) => onActivate(e)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' || e.key === ' ') onActivate(e);
        }}
      >
        <ChipBody locked={locked} label={label} />
      </button>
    );
  }

  return (
    <span id={id} className={rootClass}>
      <ChipBody locked={locked} label={label} />
    </span>
  );
};

export default PremiumChip;
