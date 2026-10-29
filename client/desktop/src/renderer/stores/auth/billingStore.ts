import { z } from 'zod';
import { createStore } from '../../utils/runtime/createStore';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import {
  openBillingSheet,
  type BillingKind,
  type BillingOutcome,
} from '../../services/system/billingLauncher';
import { useSubscriptionStore } from './subscriptionStore';

// Billing state for Settings ▸ Account ▸ Subscription (#2033 PR-4, spec §13).
//
// Holds the account's subscription read (`GET /subscriptions/me`), the plan
// catalog (`GET /billing/plans`), whether a purchase is being confirmed
// (`pending`, PlanCard state P6) and whether a hosted Stripe page is being
// opened (`opening`). Entitlement itself is never decided here: only the
// server grants, through a verified webhook, and the page reads the result.
//
// Every write that follows an `await` re-checks the auth lifecycle, so a
// response for the account that just signed out can never land in the next
// account's store. `resetService.gracefulReset` calls `reset()`.

const LIVE_STATUSES = ['active', 'trialing', 'past_due'] as const;

const StripeRowSchema = z.object({
  // Null on the wire when the Stripe price is not one Concord recognises.
  plan: z.enum(['supersonic', 'wing']).nullable().catch(null),
  interval: z.enum(['month', 'year']).nullable().catch(null),
  // The server only describes a live row here. A row in any other state fails
  // the whole read closed (the degraded-read page), never reads as live.
  status: z.enum(LIVE_STATUSES),
  current_period_end: z.string(),
  canceling: z.boolean(),
  ends_at: z.string().optional(),
  founders_rate: z.boolean(),
  unit_amount: z.number().int().nonnegative().optional(),
  schedule_attached: z.boolean().optional().catch(false),
  pending_plan: z.enum(['supersonic', 'wing']).optional(),
  pending_interval: z.enum(['month', 'year']).optional(),
  pending_change_at: z.string().optional(),
});

// Wing seats (#2033 PR-5). Ids only: names come from the friend list.
const SeatSchema = z.object({
  role: z.enum(['payer', 'member']),
  seat_id: z.string().optional(),
  state: z.enum(['pending', 'active', 'expired']).optional().catch(undefined),
  counterpart_user_id: z.string().optional(),
  invite_expires_at: z.string().optional(),
  next_swap_at: z.string().optional(),
  suspended: z.boolean().optional(),
});

const InviteSchema = z.object({
  invite_id: z.string(),
  payer_user_id: z.string(),
  expires_at: z.string(),
  blocked: z.literal('own_subscription_active').optional().catch(undefined),
});

const MeSchema = z.object({
  tier: z.string(),
  status: z.string(),
  source: z.enum(['code', 'stripe', 'seat']).optional().catch(undefined),
  current_period_end: z.string().optional(),
  // An older server sends no `billing`. Reading that as `not_configured` hides
  // every paid control, which is the page as it was before billing existed.
  billing: z.enum(['self_hosted', 'not_configured', 'available']).catch('not_configured'),
  trial_available: z.boolean().optional().catch(false),
  stripe: StripeRowSchema.optional(),
  code: z.object({ current_period_end: z.string().nullable() }).optional(),
  // A seat or invite this client cannot read is dropped, never guessed at.
  seat: SeatSchema.optional().catch(undefined),
  invites: z
    .array(z.unknown())
    .optional()
    .transform((rows) =>
      (rows ?? []).flatMap((row) => {
        const parsed = InviteSchema.safeParse(row);
        return parsed.success ? [parsed.data] : [];
      })
    ),
});

const PlanPriceSchema = z.object({
  plan: z.enum(['supersonic', 'wing']),
  interval: z.enum(['month', 'year']),
  currency: z.literal('usd'),
  unit_amount: z.number().int().nonnegative(),
  founders_unit_amount: z.number().int().nonnegative().optional(),
});

const PlansSchema = z.object({
  purchase_available: z.boolean(),
  founders: z.object({ active: z.boolean(), ends_at: z.string().optional() }),
  // Entries this client does not know (a new plan, interval or currency) are
  // dropped rather than failing the catalog; all four known prices must remain.
  plans: z
    .array(z.unknown())
    .transform((rows) =>
      rows.flatMap((row) => {
        const parsed = PlanPriceSchema.safeParse(row);
        return parsed.success ? [parsed.data] : [];
      })
    )
    .refine(
      (rows) =>
        (['supersonic', 'wing'] as const).every((plan) =>
          (['month', 'year'] as const).every((interval) =>
            rows.some((r) => r.plan === plan && r.interval === interval)
          )
        ),
      'catalog is missing a known price'
    ),
});

const SessionUrlSchema = z.object({ url: z.string().min(1) });
const ErrorBodySchema = z.object({ error: z.string() });
const SeatErrorSchema = z.object({
  error: z.string(),
  available_at: z.string().optional(),
  current_period_end: z.string().optional(),
});
const PlanChangeQuoteSchema = z.object({
  effective: z.enum(['immediate', 'scheduled', 'immediate_then_scheduled']),
  due_now: z.number().int().nonnegative(),
  currency: z.literal('usd'),
  proration_date: z.number().int().optional(),
  change_at: z.string().optional(),
});
const PlanChangeResultSchema = z.object({ booking_failed: z.boolean().optional() });
const PendingPlanPaymentSchema = z.object({
  effective: z.literal('payment_pending'),
  payment_status: z.enum(['requires_action', 'card_declined', 'processing']),
  payment_url: z.string().url(),
});

function isStripePaymentUrl(raw: string): boolean {
  try {
    const url = new URL(raw);
    const host = url.hostname.toLowerCase();
    return (
      url.protocol === 'https:' &&
      url.username === '' &&
      url.password === '' &&
      url.port === '' &&
      (host === 'pay.concordvoice.com' || host === 'stripe.com' || host.endsWith('.stripe.com'))
    );
  } catch {
    return false;
  }
}

export type BillingMe = z.infer<typeof MeSchema>;
export type StripeRow = z.infer<typeof StripeRowSchema>;
export type SeatInfo = z.infer<typeof SeatSchema>;
export type SeatInvite = z.infer<typeof InviteSchema>;

/**
 * The outcome of a seat request. `error` is the server's code, or `failed`,
 * `rate_limited` or `offline`; `date` carries `available_at` (seat_cooldown)
 * or `current_period_end` (own_subscription_active) when the server sent one.
 */
export type SeatResult = { ok: true } | { ok: false; error: string; date?: string };
export type BillingPlans = z.infer<typeof PlansSchema>;
export type PlanPrice = z.infer<typeof PlanPriceSchema>;
export type PlanId = PlanPrice['plan'];
export type PlanInterval = PlanPrice['interval'];
export type PortalFlow = 'payment_method_update' | 'subscription_cancel';
export type PlanChangeQuote = z.infer<typeof PlanChangeQuoteSchema> & {
  plan: PlanId;
  interval: PlanInterval;
};

/** Messages the page shows after a launcher outcome or a failed request. */
export type BillingNotice =
  | 'manage_existing'
  | 'seat_member'
  | 'checkout_failed'
  | 'portal_failed'
  | 'rate_limited'
  | 'offline'
  | 'checkout_closed'
  | 'checkout_load_failed'
  | 'portal_load_failed'
  | 'plan_change_failed'
  | 'plan_change_conflict'
  | 'pending_plan_change'
  | 'plan_change_saved'
  | 'plan_change_kept'
  | 'plan_change_partial'
  | 'plan_change_payment_pending'
  | 'plan_change_payment_unavailable'
  | 'card_declined'
  | 'subscription_canceling'
  | 'subscription_cancel_failed';

// Server error code → notice for a refused plan change; anything else is a plain failure.
const PLAN_CHANGE_ERROR_NOTICES = new Map<string, BillingNotice>([
  ['card_declined', 'card_declined'],
  ['pending_plan_change', 'pending_plan_change'],
  ['payment_pending', 'plan_change_payment_pending'],
  ['plan_change_conflict', 'plan_change_conflict'],
]);

/** How long Confirming waits before its one silent refetch (spec §13 P6). */
export const CONFIRMING_STALL_MS = 45_000;
/** How long a Checkout or Portal session request may take (spec §13 error copy). */
export const SESSION_REQUEST_TIMEOUT_MS = 8_000;

interface BillingState {
  me: BillingMe | null;
  meError: boolean;
  plans: BillingPlans | null;
  plansLoading: boolean;
  plansError: boolean;
  /** A completed Checkout is waiting for the server to confirm it (P6). */
  pending: boolean;
  /** P6 has waited CONFIRMING_STALL_MS without a confirmation. */
  pendingStalled: boolean;
  /** Set once when P6 ends on a live Stripe row; cleared by the next action or on leaving the page. */
  activated: boolean;
  opening: BillingKind | null;
  changing: boolean;
  previewing: boolean;
  preview: PlanChangeQuote | null;
  previewError: boolean;
  notice: BillingNotice | null;
  fetchMe: () => Promise<void>;
  fetchPlans: () => Promise<void>;
  startCheckout: (plan: PlanId, interval: PlanInterval) => Promise<BillingOutcome | null>;
  openPortal: (flow?: PortalFlow) => Promise<BillingOutcome | null>;
  previewPlanChange: (plan: PlanId, interval: PlanInterval) => Promise<void>;
  clearPlanPreview: () => void;
  changePlan: (plan: PlanId, interval: PlanInterval, quote: PlanChangeQuote) => Promise<void>;
  keepCurrentPlan: () => Promise<void>;
  cancelSubscription: () => Promise<void>;
  checkAgain: () => Promise<void>;
  /** Clears the one-shot activation and the last notice when the page closes. */
  dismissTransient: () => void;
  /** Wing seats (#2033 PR-5). Each re-reads `/me` on success. */
  inviteSeat: (userId: string, replace: boolean) => Promise<SeatResult>;
  removeSeat: () => Promise<SeatResult>;
  leaveSeat: () => Promise<SeatResult>;
  acceptInvite: (inviteId: string) => Promise<SeatResult>;
  declineInvite: (inviteId: string) => Promise<SeatResult>;
  reset: () => void;
}

// A typed Pick, not `satisfies`: the CI Semgrep TypeScript parser rejects `satisfies`.
type BillingData = Pick<
  BillingState,
  | 'me'
  | 'meError'
  | 'plans'
  | 'plansLoading'
  | 'plansError'
  | 'pending'
  | 'pendingStalled'
  | 'activated'
  | 'opening'
  | 'changing'
  | 'previewing'
  | 'preview'
  | 'previewError'
  | 'notice'
>;

const INITIAL: BillingData = {
  me: null,
  meError: false,
  plans: null,
  plansLoading: false,
  plansError: false,
  pending: false,
  pendingStalled: false,
  activated: false,
  opening: null,
  changing: false,
  previewing: false,
  preview: null,
  previewError: false,
  notice: null,
};

// Each read takes a ticket and commits only while it is still the newest one,
// so an older response arriving last never replaces a newer one.
let meTicket = 0;
let plansTicket = 0;
// One Checkout or Portal launch at a time; reset() retires the one in flight.
let launchTicket = 0;
let previewTicket = 0;
// Bumped whenever Confirming starts or ends, so a stale stall timer is inert.
let pendingTicket = 0;
let stallTimer: ReturnType<typeof setTimeout> | null = null;

function clearStallTimer(): void {
  if (stallTimer !== null) {
    clearTimeout(stallTimer);
    stallTimer = null;
  }
}

function isOffline(): boolean {
  return typeof navigator !== 'undefined' && navigator.onLine === false;
}

type SessionResult = { ok: true; url: string } | { ok: false; notice: BillingNotice };

async function readErrorCode(res: Response): Promise<string | null> {
  try {
    const parsed = ErrorBodySchema.safeParse(await safeJson<unknown>(res));
    return parsed.success ? parsed.data.error : null;
  } catch {
    return null;
  }
}

/**
 * POST a session request and return its hosted-page URL, or the notice to show.
 * Covers the spec's error table: 409 `manage_existing`, 429, and every other
 * failure (503, a network error, or the 8 s timeout) as the generic copy.
 */
async function requestSessionUrl(
  path: string,
  body: Record<string, string>,
  failed: BillingNotice
): Promise<SessionResult> {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), SESSION_REQUEST_TIMEOUT_MS);
  try {
    const res = await apiFetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: controller.signal,
    });
    if (res.ok) {
      const parsed = SessionUrlSchema.safeParse(await safeJson<unknown>(res));
      return parsed.success ? { ok: true, url: parsed.data.url } : { ok: false, notice: failed };
    }
    if (res.status === 429) return { ok: false, notice: 'rate_limited' };
    if (res.status === 409) {
      const code = await readErrorCode(res);
      if (code === 'manage_existing' || code === 'seat_member') return { ok: false, notice: code };
    }
    return { ok: false, notice: failed };
  } catch {
    return { ok: false, notice: failed };
  } finally {
    clearTimeout(timer);
  }
}

/** One seat request; never rejects. */
async function seatRequest(
  path: string,
  method: 'POST' | 'DELETE',
  body?: Record<string, unknown>
): Promise<SeatResult> {
  if (isOffline()) return { ok: false, error: 'offline' };
  try {
    const res = await apiFetch(path, {
      method,
      ...(body
        ? { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }
        : {}),
    });
    if (res.ok) return { ok: true };
    if (res.status === 429) return { ok: false, error: 'rate_limited' };
    let parsed: ReturnType<typeof SeatErrorSchema.safeParse> | undefined;
    try {
      parsed = SeatErrorSchema.safeParse(await safeJson<unknown>(res));
    } catch {
      parsed = undefined;
    }
    if (!parsed?.success) return { ok: false, error: 'failed' };
    const date = parsed.data.available_at ?? parsed.data.current_period_end;
    return date
      ? { ok: false, error: parsed.data.error, date }
      : { ok: false, error: parsed.data.error };
  } catch {
    return { ok: false, error: 'failed' };
  }
}

/** A 409 that says a plan change is already booked. */
async function isPendingPlanChangeConflict(res: Response): Promise<boolean> {
  return res.status === 409 && (await readErrorCode(res)) === 'pending_plan_change';
}

export const useBillingStore = createStore<BillingState>()((set, get) => {
  const enterConfirming = (): void => {
    clearStallTimer();
    const ticket = ++pendingTicket;
    set({ pending: true, pendingStalled: false, activated: false });
    stallTimer = setTimeout(() => {
      stallTimer = null;
      // Owned by the pending ticket alone: reset() bumps it on an account
      // change, and a same-account auth change must not drop the deadline.
      if (ticket !== pendingTicket) return;
      // The stalled copy shows at the deadline even if a read below hangs;
      // a read that finds the live row still ends Confirming.
      if (get().pending) set({ pendingStalled: true });
      void get().fetchMe();
      void useSubscriptionStore.getState().hydrate();
    }, CONFIRMING_STALL_MS);
    // One read now covers a confirmation that was pushed while the sheet was
    // still open, before Confirming started listening for it.
    void get().fetchMe();
  };

  const applyOutcome = (kind: BillingKind, outcome: BillingOutcome): void => {
    switch (outcome) {
      case 'success':
        if (kind === 'checkout') {
          enterConfirming();
        } else {
          void get().fetchMe();
        }
        return;
      case 'already_subscribed':
        set({ notice: 'manage_existing' });
        void get().fetchMe();
        return;
      case 'closed':
      case 'returned':
        if (kind === 'checkout') set({ notice: 'checkout_closed' });
        void get().fetchMe();
        return;
      case 'load_failed':
        set({ notice: kind === 'checkout' ? 'checkout_load_failed' : 'portal_load_failed' });
        return;
      case 'cancel':
      case 'busy':
        return;
    }
  };

  const launch = async (
    kind: BillingKind,
    path: string,
    body: Record<string, string>
  ): Promise<BillingOutcome | null> => {
    if (get().opening !== null || get().changing) return null;
    const ticket = ++launchTicket;
    const lifecycle = captureAuthLifecycle();
    const current = (): boolean => ticket === launchTicket && isSameAuthLifecycle(lifecycle);
    set({ opening: kind, notice: null, activated: false });
    try {
      if (isOffline()) {
        set({ notice: 'offline' });
        return null;
      }
      const session = await requestSessionUrl(
        path,
        body,
        kind === 'checkout' ? 'checkout_failed' : 'portal_failed'
      );
      if (!current()) return null;
      if (!session.ok) {
        set({ notice: session.notice });
        if (session.notice === 'manage_existing' || session.notice === 'seat_member') {
          void get().fetchMe();
        }
        return null;
      }
      const outcome = await openBillingSheet(kind, session.url);
      if (!current()) return null;
      applyOutcome(kind, outcome);
      return outcome;
    } finally {
      // Release on the ticket alone: reset() retires a launch by bumping it, and
      // a same-account generation advance (password change) must not strand it.
      if (ticket === launchTicket) set({ opening: null });
    }
  };

  const seatAction = async (
    path: string,
    method: 'POST' | 'DELETE',
    body?: Record<string, unknown>
  ): Promise<SeatResult> => {
    const result = await seatRequest(path, method, body);
    // The push also re-reads; this covers a client whose socket is down. A
    // refused request re-reads too, because the refusal usually means the
    // page is stale (an expired invite, a seat ended elsewhere). Capability
    // gates read the entitlement store, so it is re-read alongside.
    if (result.ok || result.error !== 'offline') {
      void get().fetchMe();
      void useSubscriptionStore.getState().hydrate();
    }
    return result;
  };

  // A booked change that needs a payment step: open Stripe's payment page.
  const applyPlanChangePayment = async (res: Response, current: () => boolean): Promise<void> => {
    const payment = PendingPlanPaymentSchema.parse(await safeJson<unknown>(res));
    if (!current()) return;
    previewTicket += 1;
    set({ preview: null, previewing: false });
    if (!isStripePaymentUrl(payment.payment_url)) {
      set({ notice: 'plan_change_payment_unavailable' });
      return;
    }
    const opened = await globalThis.electron?.openExternal?.(payment.payment_url);
    if (!current()) return;
    let notice: BillingNotice = 'plan_change_payment_unavailable';
    if (opened?.ok) {
      notice =
        payment.payment_status === 'card_declined'
          ? 'card_declined'
          : 'plan_change_payment_pending';
    }
    set({ notice });
  };

  const applyPlanChangeResult = async (res: Response, current: () => boolean): Promise<void> => {
    const result = PlanChangeResultSchema.parse(await safeJson<unknown>(res));
    if (!current()) return;
    previewTicket += 1;
    set({
      notice: result.booking_failed ? 'plan_change_partial' : 'plan_change_saved',
      preview: null,
      previewing: false,
    });
    await get().fetchMe();
    if (current()) await useSubscriptionStore.getState().hydrate();
  };

  const applyPlanChangeRefusal = async (
    res: Response,
    current: () => boolean,
    plan: PlanId,
    interval: PlanInterval
  ): Promise<void> => {
    const code = await readErrorCode(res);
    if (!current()) return;
    set({ notice: PLAN_CHANGE_ERROR_NOTICES.get(code ?? '') ?? 'plan_change_failed' });
    if (code === 'card_declined' || code === 'payment_pending') {
      previewTicket += 1;
      set({ preview: null, previewing: false, previewError: true });
    }
    if (code === 'plan_change_conflict') void get().previewPlanChange(plan, interval);
    if (code === 'pending_plan_change') void get().fetchMe();
  };

  return {
    ...INITIAL,

    fetchMe: async () => {
      const ticket = ++meTicket;
      const lifecycle = captureAuthLifecycle();
      const current = (): boolean => ticket === meTicket && isSameAuthLifecycle(lifecycle);
      set({ meError: false });
      try {
        const res = await apiFetch('/api/v1/subscriptions/me');
        if (!current()) return;
        if (!res.ok) throw new Error(`subscriptions/me ${res.status}`);
        const raw = await safeJson<unknown>(res);
        if (!current()) return;
        // A 200 is not proof of access when required fields are missing, so a
        // partial body fails closed rather than reading as the free plan.
        const me = MeSchema.parse(raw);
        // Confirming ends only on a live Stripe row; a push that carries an
        // unchanged tier (a buyer already covered by a code) does not end it.
        const confirmed = get().pending && me.stripe !== undefined;
        if (confirmed) {
          clearStallTimer();
          pendingTicket += 1;
          set({ me, meError: false, pending: false, pendingStalled: false, activated: true });
        } else {
          set({ me, meError: false });
        }
      } catch {
        if (current()) set({ meError: true });
      }
    },

    fetchPlans: async () => {
      if (get().plansLoading) return;
      const ticket = ++plansTicket;
      const lifecycle = captureAuthLifecycle();
      const current = (): boolean => ticket === plansTicket && isSameAuthLifecycle(lifecycle);
      set({ plansLoading: true, plansError: false });
      try {
        const res = await apiFetch('/api/v1/billing/plans');
        if (!current()) return;
        if (!res.ok) throw new Error(`billing/plans ${res.status}`);
        const raw = await safeJson<unknown>(res);
        if (!current()) return;
        set({ plans: PlansSchema.parse(raw), plansLoading: false, plansError: false });
      } catch {
        if (current()) set({ plansLoading: false, plansError: true });
      } finally {
        // The in-flight latch belongs to this ticket, not to the auth lifecycle.
        if (ticket === plansTicket && get().plansLoading) set({ plansLoading: false });
      }
    },

    startCheckout: (plan, interval) => {
      if (get().pending) return Promise.resolve(null);
      return launch('checkout', '/api/v1/billing/checkout-session', { plan, interval });
    },

    openPortal: (flow) => launch('portal', '/api/v1/billing/portal-session', flow ? { flow } : {}),

    previewPlanChange: async (plan, interval) => {
      const ticket = ++previewTicket;
      const lifecycle = captureAuthLifecycle();
      const current = (): boolean => ticket === previewTicket && isSameAuthLifecycle(lifecycle);
      set({ previewing: true, preview: null, previewError: false });
      try {
        if (isOffline()) {
          if (current()) set({ previewError: true });
          return;
        }
        const res = await apiFetch('/api/v1/billing/plan-change/preview', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ plan, interval }),
        });
        if (!current()) return;
        if (!res.ok) {
          if (await isPendingPlanChangeConflict(res)) {
            if (current()) {
              set({ notice: 'pending_plan_change' });
              void get().fetchMe();
            }
            return;
          }
          throw new Error(`plan preview ${res.status}`);
        }
        const parsed = PlanChangeQuoteSchema.parse(await safeJson<unknown>(res));
        if (current()) set({ preview: { ...parsed, plan, interval }, previewError: false });
      } catch {
        if (current()) set({ previewError: true });
      } finally {
        if (current()) set({ previewing: false });
      }
    },

    clearPlanPreview: () => {
      previewTicket += 1;
      set({ preview: null, previewing: false, previewError: false });
    },

    changePlan: async (plan, interval, quote) => {
      if (quote.plan !== plan || quote.interval !== interval || get().preview !== quote) return;
      if (get().changing || get().opening !== null || get().pending) return;
      const ticket = ++launchTicket;
      const lifecycle = captureAuthLifecycle();
      const current = (): boolean => ticket === launchTicket && isSameAuthLifecycle(lifecycle);
      set({ changing: true, notice: null });
      try {
        if (isOffline()) {
          if (current()) set({ notice: 'offline' });
          return;
        }
        const res = await apiFetch('/api/v1/billing/plan-change', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            plan,
            interval,
            expected_effective: quote.effective,
            expected_change_at: quote.change_at,
            quoted_amount_due: quote.due_now,
            proration_date: quote.proration_date,
          }),
        });
        if (!current()) return;
        if (res.status === 202) await applyPlanChangePayment(res, current);
        else if (res.ok) await applyPlanChangeResult(res, current);
        else await applyPlanChangeRefusal(res, current, plan, interval);
      } catch {
        if (current()) set({ notice: 'plan_change_failed' });
      } finally {
        if (ticket === launchTicket) set({ changing: false });
      }
    },

    keepCurrentPlan: async () => {
      if (get().changing || get().opening !== null) return;
      const ticket = ++launchTicket;
      const lifecycle = captureAuthLifecycle();
      const current = (): boolean => ticket === launchTicket && isSameAuthLifecycle(lifecycle);
      set({ changing: true, notice: null });
      try {
        if (isOffline()) {
          if (current()) set({ notice: 'offline' });
          return;
        }
        const res = await apiFetch('/api/v1/billing/plan-change/cancel', { method: 'POST' });
        if (!current()) return;
        if (!res.ok) throw new Error(`keep plan ${res.status}`);
        previewTicket += 1;
        set({ notice: 'plan_change_kept', preview: null, previewing: false });
        await get().fetchMe();
      } catch {
        if (current()) set({ notice: 'plan_change_failed' });
      } finally {
        if (ticket === launchTicket) set({ changing: false });
      }
    },

    cancelSubscription: async () => {
      if (get().changing || get().opening !== null) return;
      const ticket = ++launchTicket;
      const lifecycle = captureAuthLifecycle();
      const current = (): boolean => ticket === launchTicket && isSameAuthLifecycle(lifecycle);
      set({ changing: true, notice: null });
      try {
        if (isOffline()) {
          if (current()) set({ notice: 'offline' });
          return;
        }
        const res = await apiFetch('/api/v1/billing/subscription-cancel', { method: 'POST' });
        if (!current()) return;
        if (!res.ok) throw new Error(`cancel subscription ${res.status}`);
        previewTicket += 1;
        set({ notice: 'subscription_canceling', preview: null, previewing: false });
        await get().fetchMe();
        if (current()) await useSubscriptionStore.getState().hydrate();
      } catch {
        if (current()) set({ notice: 'subscription_cancel_failed' });
      } finally {
        if (ticket === launchTicket) set({ changing: false });
      }
    },

    checkAgain: () => get().fetchMe(),

    dismissTransient: () => set({ activated: false, notice: null }),

    inviteSeat: (userId, replace) =>
      seatAction('/api/v1/billing/seat/invite', 'POST', { user_id: userId, replace }),
    removeSeat: () => seatAction('/api/v1/billing/seat', 'DELETE'),
    leaveSeat: async () => {
      const result = await seatAction('/api/v1/billing/seat/leave', 'POST');
      if (result.ok && get().notice === 'seat_member') set({ notice: null });
      return result;
    },
    acceptInvite: (inviteId) =>
      seatAction(`/api/v1/billing/seat-invites/${encodeURIComponent(inviteId)}/accept`, 'POST'),
    declineInvite: (inviteId) =>
      seatAction(`/api/v1/billing/seat-invites/${encodeURIComponent(inviteId)}/decline`, 'POST'),

    reset: () => {
      meTicket += 1;
      plansTicket += 1;
      launchTicket += 1;
      previewTicket += 1;
      pendingTicket += 1;
      clearStallTimer();
      set({ ...INITIAL });
    },
  };
});

/** The catalog entry for a plan and interval, when the catalog has loaded. */
export function findPlanPrice(
  plans: BillingPlans | null,
  plan: PlanId,
  interval: PlanInterval
): PlanPrice | undefined {
  return plans?.plans.find((p) => p.plan === plan && p.interval === interval);
}
