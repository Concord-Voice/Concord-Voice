import { useSubscriptionStore, type Entitlement } from '../stores/subscriptionStore';

/**
 * Selective subscription to one slice of the entitlement capability set.
 * Reads from subscriptionStore; NEVER triggers a fetch (the store is hydrated
 * at login + updated by the entitlements_changed WS handler). Example:
 *   const maxChars = useEntitlement((e) => e.maxMessageChars);
 */
export function useEntitlement<T>(selector: (e: Entitlement) => T): T {
  return useSubscriptionStore((s) => selector(s.entitlement));
}
