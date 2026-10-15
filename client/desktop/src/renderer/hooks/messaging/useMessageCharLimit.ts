import { useEntitlement } from '../ui/useEntitlement';
import { clampMessageCharsForTier } from '../../utils/policy/entitlementLimits';

/**
 * The effective per-message character limit (#3857): the live entitlement value clamped by tier,
 * so a downgrade from premium takes effect without a reload and a stale or inflated value never
 * exceeds the tier's ceiling (free 5,120 / premium 10,240).
 *
 * One source for every surface that holds text to the limit — the composer, the syntax-help
 * modal and the inline edit box — so they cannot disagree. Selective subscriptions: it re-renders
 * its caller only when the tier or the raw limit changes.
 */
export function useMessageCharLimit(): number {
  const tier = useEntitlement((e) => e.tier);
  const maxMessageChars = useEntitlement((e) => e.maxMessageChars);
  return clampMessageCharsForTier(tier, maxMessageChars);
}
