import { useEffect } from 'react';
import {
  useBillingStore,
  type BillingMe,
  type SeatInfo,
  type SeatInvite,
  type StripeRow,
} from '../../stores/auth/billingStore';
import { useSubscriptionStore } from '../../stores/auth/subscriptionStore';

// Subscription status for Settings ▸ Account ▸ Subscription: a thin selector
// over billingStore's `GET /api/v1/subscriptions/me` read (#2033 PR-4).
//
// The entitlement store (#1297) carries only the resolved `tier`. This adds the
// grant's status, source and expiry, the instance's billing mode, and the live
// Stripe and code rows, and the Wing seat side. The read runs on mount and again whenever the store's
// tier changes (an entitlements_changed push), so a redeem or a purchase shows
// without an explicit refetch; `refetch` is the Retry and post-redeem re-read.
//
// Never blocks the page: when the read fails or returns a partial body the hook
// falls back to the entitlement store's tier (itself fail-closed to free) with
// status 'unknown' and `error` set. Cached access is never shown as verified.

export type SubscriptionSource = 'code' | 'stripe' | 'seat';
export type BillingMode = BillingMe['billing'];

export interface SubscriptionInfo {
  tier: string;
  status: string;
  source?: SubscriptionSource;
  expiry?: string; // RFC 3339 UTC, present only when the grant has a period end
  billing: BillingMode;
  trialAvailable?: boolean;
  stripe?: StripeRow;
  codeExpiry?: string | null; // undefined: no live code row; null: a code row that never lapses
  seat?: SeatInfo; // the caller's Wing seat side (#2033 PR-5)
  invites: SeatInvite[]; // open Wing invites the caller may answer
  loading: boolean;
  error: boolean;
  refetch: () => void;
}

export function useSubscription(): SubscriptionInfo {
  const me = useBillingStore((s) => s.me);
  const meError = useBillingStore((s) => s.meError);
  const fetchMe = useBillingStore((s) => s.fetchMe);
  // The fail-closed fallback tier, and the signal that a push changed access.
  const storeTier = useSubscriptionStore((s) => s.entitlement.tier);

  useEffect(() => {
    // fetchMe resolves its own failures into `meError` and never rejects.
    void fetchMe();
  }, [fetchMe, storeTier]);

  const refetch = (): void => {
    void fetchMe();
  };

  // A failed refresh keeps showing the last good read; the error state is
  // only for when no read has succeeded yet.
  if (me === null) {
    return {
      tier: storeTier,
      status: 'unknown',
      billing: 'not_configured',
      invites: [],
      loading: !meError,
      error: meError,
      refetch,
    };
  }

  return {
    tier: me.tier,
    status: me.status,
    source: me.source,
    expiry: me.current_period_end,
    billing: me.billing,
    trialAvailable: me.trial_available,
    stripe: me.stripe,
    codeExpiry: me.code ? me.code.current_period_end : undefined,
    seat: me.seat,
    invites: me.invites,
    loading: false,
    error: false,
    refetch,
  };
}
