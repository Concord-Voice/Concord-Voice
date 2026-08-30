import { useCallback, useEffect, useState } from 'react';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import { useSubscriptionStore } from '../../stores/auth/subscriptionStore';

// Subscription status for the Settings subscription page (#1304). Reads
// GET /api/v1/subscriptions/me (the read-only status companion to the
// entitlement set) on mount and exposes tier/status/source/expiry so the
// PlanCard can render the active plan's neutral source and optional expiry.
//
// The entitlementStore (#1297) carries only the resolved `tier`; this hook adds
// the grant's status/source/expiry which the store does not hold. `refetch` is
// called by the RedeemCodeForm after a successful redeem so the status view
// updates immediately (the server ALSO pushes entitlements_changed, which
// updates the store's tier live — this hook re-reads the richer status shape).
//
// Never blocks the page: on any fetch error the hook falls back to the
// subscriptionStore's tier (itself fail-closed to free) with status 'unknown'
// and surfaces `error`, so the page still renders a valid plan state.

export type SubscriptionSource = 'code' | 'stripe';

// Wire shape of GET /api/v1/subscriptions/me (snake_case; mirrors the Go
// StatusDTO). All fields optional to stay resilient to a partial/drifted 200.
interface SubscriptionStatusResponse {
  tier?: string;
  status?: string;
  source?: string;
  current_period_end?: string;
}

export interface SubscriptionInfo {
  tier: string;
  status: string;
  source?: SubscriptionSource;
  expiry?: string; // RFC 3339 UTC, present only when the grant has a period end
  loading: boolean;
  error: boolean;
  refetch: () => void;
}

function normalizeSource(source: string | undefined): SubscriptionSource | undefined {
  return source === 'code' || source === 'stripe' ? source : undefined;
}

export function useSubscription(): SubscriptionInfo {
  const [state, setState] = useState<Omit<SubscriptionInfo, 'refetch'>>({
    tier: 'free',
    status: 'loading',
    loading: true,
    error: false,
  });
  // Read the store tier as the fail-open-safe fallback (it is itself fail-closed
  // to the free floor, so this can never fabricate premium).
  const storeTier = useSubscriptionStore((s) => s.entitlement.tier);

  const load = useCallback(async () => {
    setState((prev) => ({ ...prev, loading: true, error: false }));
    try {
      const res = await apiFetch('/api/v1/subscriptions/me');
      if (!res.ok) throw new Error(`subscriptions/me ${res.status}`);
      const data = await safeJson<SubscriptionStatusResponse>(res);
      setState({
        tier: data?.tier ?? storeTier,
        status: data?.status ?? 'none',
        source: normalizeSource(data?.source),
        expiry: data?.current_period_end,
        loading: false,
        error: false,
      });
    } catch {
      // Never block the page: fall back to the store's (fail-closed) tier.
      setState({
        tier: storeTier,
        status: 'unknown',
        source: undefined,
        expiry: undefined,
        loading: false,
        error: true,
      });
    }
  }, [storeTier]);

  // load is a useCallback keyed on storeTier, so this fires on mount AND whenever
  // the store tier changes live (the entitlements_changed WS push after a redeem
  // updates the store) — refreshing the richer status shape without an explicit
  // refetch. `refetch` remains for the immediate post-redeem re-read.
  useEffect(() => {
    // load() handles all errors internally (its body is a try/catch that resolves
    // to a fail-closed state and never rejects); the .catch is a belt-and-suspenders
    // guard so a floating rejection can never surface as an unhandled promise.
    load().catch(() => {
      /* load() already set the fail-closed error state */
    });
  }, [load]);

  return {
    ...state,
    refetch: () => {
      load().catch(() => {
        /* load() already set the fail-closed error state */
      });
    },
  };
}
