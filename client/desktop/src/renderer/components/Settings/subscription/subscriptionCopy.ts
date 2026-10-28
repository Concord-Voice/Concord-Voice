import type { BillingNotice } from '../../../stores/auth/billingStore';

// Marketing tier names + display copy for Account's Subscription section.
// The WIRE tier is 'free' | 'premium' (the entitlement contract); the USER sees
// the marketing names Sonic / Supersonic. This map is the single translation
// point — never render the raw wire tier in the UI.

// Wire tier → marketing display name. Unknown tiers never establish free access.
const TIER_DISPLAY_NAME: Record<string, string> = {
  free: 'Sonic',
  premium: 'Supersonic',
};

export function tierDisplayName(tier: string): string {
  return Object.hasOwn(TIER_DISPLAY_NAME, tier) ? TIER_DISPLAY_NAME[tier] : 'Plan unavailable';
}

// Wire subscription source → human sentence for the PlanCard "how you got this".
const SOURCE_DISPLAY: Record<string, string> = {
  code: 'Redeemed via code',
  stripe: 'Active subscription',
  seat: "Included with a friend's Wing",
};

export function sourceDisplay(source: string | undefined): string | undefined {
  return source ? SOURCE_DISPLAY[source] : undefined;
}

// Format an RFC 3339 UTC expiry into a short human date. Returns undefined for a
// missing/unparseable value so the caller simply omits the "expires" line.
export function formatExpiry(iso: string | undefined): string | undefined {
  if (!iso) return undefined;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return undefined;
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'long', day: 'numeric' });
}

// ── Paid plans (#2033 PR-4, spec §13) ────────────────────────────────────

// Sold plan id → display name. Both are user-axis plans; Wing pays for one
// Supersonic seat for the payer plus one for a friend.
const PLAN_DISPLAY_NAME: Record<string, string> = {
  supersonic: 'Supersonic',
  wing: 'Wing',
};

export function planDisplayName(plan: string | null | undefined): string | undefined {
  return plan && Object.hasOwn(PLAN_DISPLAY_NAME, plan) ? PLAN_DISPLAY_NAME[plan] : undefined;
}

export const PLAN_VALUE_LINE: Record<'supersonic' | 'wing', string> = {
  supersonic: 'All Supersonic features for your account.',
  wing: 'Supersonic for you and one friend you choose.',
};

// Format an integer amount in minor units (cents) for display. Prices arrive
// as integers and are formatted here, never on the server (spec R17).
export function formatPrice(minorUnits: number, currency: string): string {
  return new Intl.NumberFormat(undefined, {
    style: 'currency',
    currency: currency.toUpperCase(),
  }).format(minorUnits / 100);
}

// The founders price, matching the server's rounding: half the unit amount,
// rounded down to a whole cent, so the price shown never exceeds the charge.
export function foundersAmount(unitAmount: number): number {
  return Math.floor((unitAmount * 50) / 100);
}

export function intervalSuffix(interval: 'month' | 'year'): string {
  return interval === 'month' ? '/month' : '/year';
}

export const BILLING_COPY = {
  confirming: 'Confirming your subscription…',
  confirmingStalled:
    "We haven't received confirmation yet. If you were charged, your subscription will appear here shortly. You don't need to pay again.",
  activated: 'Your subscription is active.',
  trialActivated: 'Your 30-day free trial has started.',
  checkAgain: 'Check again',
  shellTooOld: 'Update Concord to subscribe or manage billing.',
  selfHosted: 'Your server provides premium features.',
  manageHint: 'Change plan, update your card, or see invoices.',
  pastDueTitle: "Payment didn't go through",
  pastDueBody: "We'll keep trying, and you keep Supersonic meanwhile. Update your card to keep it.",
  foundersEnds: 'Your Founders rate ends with it.',
  foundersChip: 'Founders rate',
  foundersDiscount: '50% off while you stay subscribed. Canceling ends it.',
  renewsAutomatically: 'Renews automatically until you cancel.',
  trialTerms:
    'Supersonic comes with a 30-day free trial. It starts billing when the trial ends unless you cancel before then. Any redeemed code credit reduces the first paid charge. Wing has no trial and starts billing today.',
  trialStripePage: "You'll enter your card on Stripe's secure page. Concord never sees your card.",
  stripePage: "You'll pay on Stripe's secure page. Concord never sees your card.",
  openingCheckout: 'Opening secure checkout…',
  openingPortal: 'Opening billing…',
  plansUnavailable: "Couldn't load plans.",
} as const;

export const BILLING_NOTICE_COPY: Record<BillingNotice, string> = {
  manage_existing: 'You already have a subscription.',
  seat_member: "You have Supersonic through a friend's Wing. Leave Wing to buy your own plan.",
  checkout_failed: "Couldn't open checkout. Nothing was charged. Try again.",
  portal_failed: "Couldn't open billing. Try again.",
  rate_limited: 'Too many tries. Wait a few minutes, then try again.',
  offline: "You're offline. Reconnect to continue.",
  checkout_closed: 'Checkout closed. If you finished paying, this page updates on its own.',
  checkout_load_failed: "Couldn't load checkout. Check your connection and try again.",
  portal_load_failed: "Couldn't load billing. Check your connection and try again.",
  plan_change_failed: "Couldn't change your plan. Please try again.",
  plan_change_conflict:
    'The charge or subscription changed. Check the updated amount and try again.',
  pending_plan_change:
    'Keep your current plan first to withdraw the booked change, then choose a new one.',
  plan_change_saved: 'Your plan change is saved.',
  plan_change_kept: 'Your current plan will continue at renewal.',
  plan_change_partial:
    'Your upgrade is active, but the later interval change could not be booked. Check your subscription before choosing the interval again.',
  plan_change_payment_pending:
    'Complete payment in the Stripe invoice page. Your plan changes after payment succeeds.',
  plan_change_payment_unavailable:
    'Your plan change is waiting for payment, but the Stripe invoice page could not open. Open billing to complete payment.',
  card_declined:
    'Your card was declined. Complete payment on the Stripe invoice page or update your card.',
  subscription_canceling: 'Your subscription will end after the paid period.',
  subscription_cancel_failed: "Couldn't cancel your subscription. Please try again.",
};

export function planChangeSummary(
  currentPlan: 'supersonic' | 'wing',
  currentInterval: 'month' | 'year',
  nextPlan: 'supersonic' | 'wing',
  nextInterval: 'month' | 'year'
): string {
  if (
    currentPlan === 'supersonic' &&
    currentInterval === 'year' &&
    nextPlan === 'wing' &&
    nextInterval === 'month'
  ) {
    return 'Wing yearly starts now with a prorated charge. Wing monthly starts at the end of your current yearly period.';
  }
  if (
    (currentPlan === 'wing' && nextPlan === 'supersonic') ||
    (currentInterval === 'year' && nextInterval === 'month')
  ) {
    return 'Changes at the end of your current paid period. Your current plan stays active until then.';
  }
  if (currentInterval === 'month' && nextInterval === 'year') {
    return 'Changes now. The unused part of this month is credited toward the yearly charge.';
  }
  if (currentPlan === 'supersonic' && nextPlan === 'wing') {
    return 'Changes now. You pay only the prorated difference for the days left; the new price starts at renewal.';
  }
  return 'Changes at your next renewal.';
}

export function codeOverlapNote(date: string | undefined, trialAvailable = false): string {
  const coverage = date
    ? `Your code already covers Supersonic until ${date}.`
    : 'Your code already covers Supersonic.';
  return trialAvailable
    ? `${coverage} A Supersonic trial starts today; paid billing begins when it ends unless you cancel. Wing starts billing today.`
    : `${coverage} Subscribing starts billing today.`;
}

export function foundersTerms(date: string): string {
  return `Founders rate: 50% off for as long as you stay subscribed. If you cancel, you lose it. Available for subscriptions started before ${date}.`;
}

export function alsoFromCode(date: string | undefined): string {
  return date ? `Also Supersonic from a code until ${date}.` : 'Also Supersonic from a code.';
}

// ── Wing seats (#2033 PR-5, spec §13) ────────────────────────────────────

// Names come from the friend list; a counterpart who is not (or no longer) a
// friend falls back to these.
export const SEAT_FALLBACK_HOLDER = 'your +1';
export const SEAT_FALLBACK_PAYER = 'a friend';

export const SEAT_COPY = {
  wellTitle: 'Your +1',
  inviteFriend: 'Invite a friend',
  inviteSomeoneElse: 'Invite someone else',
  noFriends: 'Add a friend first, then invite them to your Wing.',
  withdraw: 'Withdraw invite',
  change: 'Change +1',
  remove: 'Remove',
  leave: 'Leave Wing',
  pickerTitle: 'Invite a friend to your Wing',
  pickerSend: 'Send invite',
  pickerSending: 'Sending…',
  pickerFailed: "Couldn't send the invite. Check that you're still friends.",
  actionFailed: "Couldn't update your Wing. Try again.",
  inviteRegion: 'Wing invite',
  accept: 'Accept',
  accepting: 'Accepting…',
  decline: 'Decline',
  acceptFailed: "Couldn't accept the invite. Try again.",
  declineFailed: "Couldn't decline the invite. Try again.",
  inviteExpired: 'This invite expired.',
  inviteGone: 'This invite is no longer available.',
  alreadyMember: "You're already on a friend's Wing.",
} as const;

export function seatActiveLine(name: string): string {
  return `${name} has Supersonic through your Wing.`;
}

export function seatPendingLine(name: string, expires: string | undefined): string {
  return expires ? `Invite sent to ${name}. It expires on ${expires}.` : `Invite sent to ${name}.`;
}

export function seatExpiredLine(name: string): string {
  return `Your invite to ${name} expired.`;
}

export function seatCooldownLine(date: string): string {
  return `You can change your +1 again on ${date}.`;
}

export function seatIncludedWith(name: string): string {
  return `Included with ${name}'s Wing`;
}

/** A member seat whose Wing is paused or unpaid: it grants nothing for now. */
export function seatOnHold(name: string): string {
  return `On hold until ${name}'s Wing is active again`;
}

// The 30-day cooldown is authoritative (spec §9), so the confirm never offers
// an instant switch back.
export function seatChangeConfirm(oldName: string): string {
  return `${oldName} loses Supersonic right away. You can't change your +1 again for 30 days.`;
}

export function seatRemoveConfirm(name: string, nextPick: string | undefined): string {
  const base = `Remove ${name} as your +1? They lose Supersonic right away.`;
  return nextPick ? `${base} You can pick someone new on ${nextPick}.` : base;
}

export function seatLeaveConfirm(name: string): string {
  return `Leave ${name}'s Wing? You'll go back to Sonic right away.`;
}

export function seatInviteBody(name: string, expires: string | undefined): string {
  const base = `${name} invited you to their Wing. You get Supersonic while their Wing is active.`;
  return expires ? `${base} The invite expires on ${expires}.` : base;
}

export function seatBlockedLine(date: string | undefined): string {
  return date
    ? `You already pay for Supersonic. You can accept after your plan ends on ${date}.`
    : 'You already pay for Supersonic. You can accept after your plan ends.';
}

export function seatPayerCooldown(name: string, date: string | undefined): string {
  return date ? `${name} can't add a new +1 until ${date}.` : `${name} can't add a new +1 yet.`;
}

export function seatInviteCooldown(date: string | undefined): string {
  return date ? `You can invite someone new on ${date}.` : "You can't invite someone new yet.";
}
