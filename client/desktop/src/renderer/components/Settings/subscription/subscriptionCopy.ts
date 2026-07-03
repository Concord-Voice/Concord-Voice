// Marketing tier names + display copy for the subscription page (#1304).
// The WIRE tier is 'free' | 'premium' (the entitlement contract); the USER sees
// the marketing names Sonic / Supersonic. This map is the single translation
// point — never render the raw wire tier in the UI.

export const KICKSTARTER_URL = 'https://www.kickstarter.com/projects/concord-mark/concord-voice';

// Wire tier → marketing display name. Unknown tiers fall back to the free name
// (least surprise; a drifted tier never shows a scary blank).
const TIER_DISPLAY_NAME: Record<string, string> = {
  free: 'Sonic',
  premium: 'Supersonic',
};

export function tierDisplayName(tier: string): string {
  return TIER_DISPLAY_NAME[tier] ?? TIER_DISPLAY_NAME.free;
}

// Wire subscription source → human sentence for the PlanCard "how you got this".
const SOURCE_DISPLAY: Record<string, string> = {
  kickstarter: 'Redeemed via Kickstarter',
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
