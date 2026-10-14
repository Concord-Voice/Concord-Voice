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
