// Renderer side of the billing launcher (#2033 PR-4, IPC contract 32).
//
// The Electron shell opens Stripe's hosted Checkout or Customer Portal in a
// modal sheet through `window.electron.openBilling` and resolves when the sheet
// closes. A shell below contract 32 has no bridge, and then the Subscription
// page shows no paid controls (spec R41: there is no OS-browser fallback).
// Main validates the URL and the sender; nothing here is a security boundary.

export type BillingKind = 'checkout' | 'portal';

export type BillingOutcome =
  'success' | 'cancel' | 'already_subscribed' | 'returned' | 'closed' | 'load_failed' | 'busy';

const OUTCOMES: ReadonlySet<string> = new Set<BillingOutcome>([
  'success',
  'cancel',
  'already_subscribed',
  'returned',
  'closed',
  'load_failed',
  'busy',
]);

function isBillingOutcome(value: unknown): value is BillingOutcome {
  return typeof value === 'string' && OUTCOMES.has(value);
}

/** True when this shell exposes the billing launcher bridge (contract 32+). */
export function isBillingLauncherAvailable(): boolean {
  return typeof globalThis.electron?.openBilling === 'function';
}

/**
 * Open the hosted page and resolve with how the sheet ended.
 *
 * Never rejects. A missing bridge, a failed invoke, or a reply this renderer
 * does not recognise all resolve `load_failed`, the one outcome that tells the
 * user nothing opened and nothing was charged.
 */
export async function openBillingSheet(kind: BillingKind, url: string): Promise<BillingOutcome> {
  const open = globalThis.electron?.openBilling;
  if (typeof open !== 'function') return 'load_failed';
  try {
    const reply: unknown = await open({ kind, url });
    if (reply && typeof reply === 'object' && 'result' in reply && isBillingOutcome(reply.result)) {
      return reply.result;
    }
    return 'load_failed';
  } catch {
    return 'load_failed';
  }
}
