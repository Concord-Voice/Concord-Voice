import React from 'react';
import PremiumChip from './PremiumChip';
import { useGateActivation } from '../../hooks/useGateActivation';
import type { SubscriptionDeepLink } from '../../utils/openSubscriptionPage';
import './PremiumGate.css';

export type PremiumGateMode = 'dim' | 'clamp' | 'option';

export interface PremiumGateProps {
  /** Optional feature name, surfaced for callers/tests; not rendered. */
  feature?: string;
  /** Visual treatment of the dormant control (spec §3). All keep it focusable. */
  mode: PremiumGateMode;
  /** Whether the caller's entitlement grants this feature. */
  entitled: boolean;
  /** The control being gated. */
  children: React.ReactNode;
  /** Best-effort deep-link hint forwarded to the Subscription page (#1304). */
  onActivateSection?: SubscriptionDeepLink;
}

/**
 * Wraps a control with the premium lock UX (#1301).
 *
 *  - **Entitled** → renders `children` untouched (zero overhead, no wrapper
 *    semantics). The gate must be invisible to a premium user.
 *  - **Not entitled** → renders the dormant control inside a PLAIN, non-focusable
 *    container `<span>` (no `role`, no `tabIndex`). The gated control ITSELF
 *    carries `aria-disabled="true"` + `aria-describedby` → the inline
 *    `<PremiumChip>`, which is a focusable native `<button>` (the upgrade
 *    affordance). Activation of the control (Enter / Space / click) is
 *    intercepted in the capture phase and routed to the Subscription page.
 *
 * HARD a11y rule (acceptance O1): the gated control stays FOCUSABLE — by its own
 * natural focusability, and the upgrade button (the chip) is always focusable.
 * The wrapper NEVER applies the HTML `disabled` attribute and its CSS NEVER uses
 * `pointer-events:none`. The wrapper span is NOT itself interactive — no `role`
 * and no `tabIndex` — so it triggers neither S6845 (tabIndex on a non-interactive
 * element) nor S6819 (role="button" on a non-button); the chip button carries the
 * interactive semantics instead. Locked-ness is conveyed by the 🔒 glyph + the
 * word "Premium" + a full-opacity muted token — never opacity/colour alone.
 *
 * Activation is intercepted in the CAPTURE phase so the gate routes to
 * Subscription BEFORE the underlying control's own handler can fire — the
 * locked control must not perform its native action.
 */
const PremiumGate: React.FC<PremiumGateProps> = ({
  feature,
  mode,
  entitled,
  children,
  onActivateSection,
}) => {
  const { onActivate, describedById } = useGateActivation(onActivateSection);

  if (entitled) {
    return <>{children}</>;
  }

  const handleClickCapture = (e: React.MouseEvent): void => {
    // Stop the underlying control from performing its native action; route to
    // the upsell instead. The locked control is "visible-but-dormant".
    e.preventDefault();
    e.stopPropagation();
    onActivate(e);
  };

  const handleKeyDownCapture = (e: React.KeyboardEvent): void => {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.preventDefault();
    e.stopPropagation();
    onActivate(e);
  };

  // Inject the locked-state ARIA onto the gated control ITSELF (not the wrapper),
  // keeping it focusable: aria-disabled announces the dormant state and
  // aria-describedby points the AT user at the explanatory chip. We never add the
  // HTML `disabled` attribute (O1) — that would remove the control from the tab
  // order. Cloning the child to inject ARIA (rather than wrapping it in an
  // interactive span) is the mechanism of the S6819 fix: the control stays
  // focusable without the wrapper becoming a non-button "button". The child is
  // always a single settings control (button / ToggleSwitch / select). When
  // `children` is NOT a single valid element we fall back to a semantically-
  // neutral wrapping <span> that carries the ARIA (still no tabIndex/role on it —
  // the chip button owns interactivity).
  const cloneAriaIntoChild = (el: React.ReactElement): React.ReactElement =>
    // eslint-disable-next-line @eslint-react/no-clone-element -- intentional: see the block comment above.
    React.cloneElement(el as React.ReactElement<Record<string, unknown>>, {
      'aria-disabled': true,
      'aria-describedby': describedById,
    });

  const gatedControl = React.isValidElement(children) ? (
    cloneAriaIntoChild(children)
  ) : (
    <span className="premium-gate__control" aria-disabled="true" aria-describedby={describedById}>
      {children}
    </span>
  );

  return (
    // Plain, NON-interactive container: no role, no tabIndex. It only groups the
    // dormant control with its chip and capture-intercepts activation so the
    // control's native action is replaced by the upsell route. Capture-phase
    // handlers work regardless of whether the wrapper is focusable.
    <span
      className={`premium-gate premium-gate--${mode}`}
      data-feature={feature}
      onClickCapture={handleClickCapture}
      onKeyDownCapture={handleKeyDownCapture}
    >
      {gatedControl}
      <PremiumChip id={describedById} className="premium-gate__chip" onActivate={onActivate} />
    </span>
  );
};

export default PremiumGate;
