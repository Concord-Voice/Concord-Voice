import React from 'react';
import { useEntitlement } from '../../../hooks/ui/useEntitlement';
import type { Entitlement } from '../../../stores/auth/subscriptionStore';

// The Supersonic (premium) feature list — entitlement-backed premium
// capabilities, anchored on the entitlement matrix §1 "User axis"
// (docs/design/entitlements/entitlement-matrix.md) plus other premium
// entitlement fields the server enforces. Custom colour schemes are NOT listed
// here: #1971 made them free for all users, so they are no longer a Supersonic
// upsell (do not re-add an allowCustomScheme-gated row; see the matrix §5
// "Custom colour schemes" note). Server creation and message search are free
// and unlimited for everyone too (#3855), so they have no row either. Content
// is a static list cited here — NOT duplicated; each row's ✓/lock is computed live
// from the current entitlement set via a predicate over the SAME fields the
// server enforces, so the grid can never claim a capability the user lacks.
//
// A row is "unlocked" when the user's entitlement already satisfies it (i.e. the
// free floor does not). This makes the grid a truthful reflection of the live
// tier rather than a hardcoded free-vs-premium table.

interface FeatureRow {
  label: string;
  // Returns true when the current entitlement grants this feature.
  granted: (e: Entitlement) => boolean;
}

// A negative height is the "native" sentinel (ServerLimitUnlimited). Free
// values are the floor in FREE_ENTITLEMENT.
const FEATURES: readonly FeatureRow[] = [
  {
    label: 'Portable audio quality (High · Hi-Fi · Studio) in DMs and group calls',
    granted: (e) => e.allowedAudioTiers.includes('studio'),
  },
  {
    label: '256 MB file uploads (up from 32 MB), everywhere',
    granted: (e) => e.maxAttachmentBytes > 33_554_432,
  },
  {
    label: 'Native-resolution webcam on every server',
    granted: (e) => e.cameraMaxHeight < 0 || e.cameraMaxHeight > 720,
  },
  {
    label: 'Animated GIF avatar & banner',
    granted: (e) => e.allowAnimatedProfile,
  },
];

const FeatureGrid: React.FC = () => {
  const entitlement = useEntitlement((e) => e);

  return (
    <ul className="subscription-feature-grid" aria-label="Supersonic features">
      {FEATURES.map((f) => {
        const granted = f.granted(entitlement);
        return (
          <li
            key={f.label}
            className={`subscription-feature-row ${granted ? 'is-granted' : 'is-locked'}`}
          >
            <span
              className="subscription-feature-mark"
              aria-hidden="true"
              data-state={granted ? 'granted' : 'locked'}
            >
              {granted ? '✓' : '🔒'}
            </span>
            <span className="subscription-feature-label">{f.label}</span>
            <span className="subscription-feature-status">
              {granted ? 'Included' : 'Supersonic'}
            </span>
          </li>
        );
      })}
    </ul>
  );
};

export default FeatureGrid;
