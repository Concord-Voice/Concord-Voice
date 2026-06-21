import { useCallback, useId, useMemo } from 'react';
import { openSubscriptionPage, type SubscriptionDeepLink } from '../utils/openSubscriptionPage';

/**
 * Shared activation wiring for every lock affordance (#1301). Returns a single
 * `onActivate` handler that routes click + keyboard (Enter/Space) to the
 * Subscription page, plus a stable `describedById` to wire a gated control's
 * `aria-describedby` to its inline `<PremiumChip>` (a11y O1 — locked controls
 * stay focusable and explain themselves).
 *
 * `onActivate` is typed loosely (React mouse OR keyboard event, both optional)
 * so the same handler attaches to `onClick` and `onKeyDown` on a host control.
 * Enter/Space are intercepted (with `preventDefault` to suppress the implicit
 * scroll/click) and routed; other keys fall through untouched.
 *
 * @param section best-effort deep-link hint forwarded to `openSubscriptionPage`.
 */
export function useGateActivation(section?: SubscriptionDeepLink): {
  onActivate: (event?: React.MouseEvent | React.KeyboardEvent) => void;
  describedById: string;
} {
  const describedById = `premium-chip-${useId()}`;

  const onActivate = useCallback(
    (event?: React.MouseEvent | React.KeyboardEvent) => {
      // Keyboard path: only Enter / Space activate; everything else is ignored.
      if (event && 'key' in event) {
        if (event.key !== 'Enter' && event.key !== ' ') return;
        event.preventDefault();
      }
      openSubscriptionPage(section);
    },
    [section]
  );

  return useMemo(() => ({ onActivate, describedById }), [onActivate, describedById]);
}
