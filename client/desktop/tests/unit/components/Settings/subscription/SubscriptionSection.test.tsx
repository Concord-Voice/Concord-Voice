import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, render, screen, userEvent, within } from '../../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../../mocks/server';
import { resetAllStores } from '../../../../helpers/store-helpers';
import SubscriptionSection from '@/renderer/components/Settings/subscription/SubscriptionSection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';
import { useBillingStore } from '@/renderer/stores/auth/billingStore';

const API_BASE = 'http://localhost:8080/api/v1';
const STATUS_PATH = `${API_BASE}/subscriptions/me`;

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

async function openSubscription() {
  const section = document.getElementById('section-subscription');
  expect(section).toBeInstanceOf(HTMLDetailsElement);
  await userEvent.setup().click(within(section as HTMLElement).getByText('Subscription'));
  return section as HTMLDetailsElement;
}

describe('SubscriptionSection compact Account summary', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ tier: 'free', status: 'none' })));
  });

  it('offers one collapsed Subscription section with inline redemption and an external comparison', async () => {
    render(<SubscriptionSection />);
    const sections = [...document.querySelectorAll('.subscription-section > details')];
    expect(sections.map((node) => node.id)).toEqual(['section-subscription']);
    expect(sections[0]).not.toHaveAttribute('open');
    await openSubscription();
    expect(await screen.findByText('Sonic')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Code' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Redeem' })).toBeDisabled();
    expect(screen.getByRole('link', { name: 'Compare planned options' })).toHaveAttribute(
      'href',
      'https://concordvoice.com/compare/'
    );
    expect(
      screen.getByText('Paid subscriptions are planned. Options and prices may change.')
    ).toBeInTheDocument();
    expect(screen.queryByText('Personal Plans')).not.toBeInTheDocument();
    expect(screen.queryByText('Group Plans')).not.toBeInTheDocument();
    expect(screen.queryByText('Server Plans')).not.toBeInTheDocument();
    expect(screen.queryByRole('table', { name: /planned.*prices/i })).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: /buy|purchase|checkout|waitlist|pledge|upgrade/i })
    ).not.toBeInTheDocument();
  });

  it.each([
    ['empty object', {}],
    ['tier without status', { tier: 'free' }],
  ])(
    'keeps a partial HTTP 200 %s unconfirmed instead of manufacturing a free plan',
    async (_label, payload) => {
      server.use(http.get(STATUS_PATH, () => HttpResponse.json(payload)));
      render(<SubscriptionSection />);
      await openSubscription();
      expect(await screen.findByRole('button', { name: 'Retry' })).toBeInTheDocument();
      expect(
        screen.getByText(
          /Subscription status not confirmed|Could not check your subscription status/i
        )
      ).toBeInTheDocument();
      expect(screen.queryByText('Free plan')).not.toBeInTheDocument();
      expect(screen.queryByText('You’re on Sonic, the free plan.')).not.toBeInTheDocument();
      expect(screen.queryByText('Active')).not.toBeInTheDocument();
      expect(screen.queryByText(/Last-known access:/i)).not.toBeInTheDocument();
    }
  );

  it('labels hydrated free entitlement as last-known access when a HTTP 200 status is missing', async () => {
    useSubscriptionStore.setState({ hydrated: true });
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ tier: 'free' })));
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(screen.getByText(/Last-known access:/i)).toHaveTextContent('Sonic');
    expect(screen.queryByText('Free plan')).not.toBeInTheDocument();
    expect(screen.queryByText('You’re on Sonic, the free plan.')).not.toBeInTheDocument();
  });

  it('keeps a hydrated premium entitlement separate from an incomplete current status response', async () => {
    useSubscriptionStore.setState({
      hydrated: true,
      entitlement: { ...useSubscriptionStore.getState().entitlement, tier: 'premium' },
    });
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({})));
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByRole('button', { name: 'Retry' })).toBeInTheDocument();
    expect(screen.getByText(/Last-known access:/i)).toHaveTextContent('Supersonic');
    expect(screen.queryByText('Free plan')).not.toBeInTheDocument();
    expect(screen.queryByText('Active')).not.toBeInTheDocument();
    expect(screen.queryByText(/No expiry|lifetime|Renews/i)).not.toBeInTheDocument();
  });

  it('retries a failed read without presenting confirmed free access', async () => {
    server.use(http.get(STATUS_PATH, () => new HttpResponse(null, { status: 503 })));
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByRole('button', { name: /retry/i })).toBeInTheDocument();
    expect(screen.queryByText('Free plan')).not.toBeInTheDocument();
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({
          tier: 'premium',
          status: 'active',
          source: 'code',
          current_period_end: '2027-01-01T00:00:00Z',
        })
      )
    );
    await userEvent.setup().click(screen.getByRole('button', { name: /retry/i }));
    expect(await screen.findByRole('heading', { name: 'Supersonic' })).toBeInTheDocument();
    expect(screen.getByText(/Access ends/i)).toBeInTheDocument();
  });

  it('refreshes the visible current access after successful inline redemption', async () => {
    let redeemed = false;
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json(
          redeemed
            ? {
                tier: 'premium',
                status: 'active',
                source: 'code',
                current_period_end: '2027-01-01T00:00:00Z',
              }
            : { tier: 'free', status: 'none' }
        )
      ),
      http.post(`${API_BASE}/redeem`, async ({ request }) => {
        expect(await request.json()).toEqual({ code: 'TEST-GRANT' });
        redeemed = true;
        return HttpResponse.json({ success: true, description: 'Supersonic access granted.' });
      }),
      http.get(`${API_BASE}/entitlements`, () => HttpResponse.json({ tier: 'premium' }))
    );
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByText('Sonic')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox', { name: 'Code' }), '  TEST-GRANT  ');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));
    // PlanCard keeps its own live <output> mounted, so look inside the form.
    const form = screen.getByRole('textbox', { name: 'Code' }).closest('form') as HTMLElement;
    expect(await within(form).findByRole('status')).toHaveTextContent('Supersonic access granted.');
    expect(await screen.findByRole('heading', { name: 'Supersonic' })).toBeInTheDocument();
    expect(screen.getByText('Redeemed via code')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Code' })).toHaveValue('');
  });

  it('keeps failed inline redemption generic and preserves confirmed current access', async () => {
    server.use(http.post(`${API_BASE}/redeem`, () => new HttpResponse(null, { status: 400 })));
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByText('Sonic')).toBeInTheDocument();
    const user = userEvent.setup();
    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'TEST-INVALID');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'That code is not valid. Double-check it and try again.'
    );
    expect(screen.getByText('Sonic')).toBeInTheDocument();
    expect(screen.getByRole('textbox', { name: 'Code' })).toHaveValue('TEST-INVALID');
  });
});

describe('SubscriptionSection paid plans (#2033 PR-4)', () => {
  const PLANS_PATH = `${API_BASE}/billing/plans`;
  const CHECKOUT_PATH = `${API_BASE}/billing/checkout-session`;
  const catalog = (purchase: boolean) => ({
    purchase_available: purchase,
    founders: { active: false },
    plans: [
      { plan: 'supersonic', interval: 'month', currency: 'usd', unit_amount: 899 },
      { plan: 'supersonic', interval: 'year', currency: 'usd', unit_amount: 9889 },
      { plan: 'wing', interval: 'month', currency: 'usd', unit_amount: 1499 },
      { plan: 'wing', interval: 'year', currency: 'usd', unit_amount: 16489 },
    ],
  });
  const ACTIVE = {
    tier: 'premium',
    status: 'active',
    source: 'stripe',
    current_period_end: '2027-01-15T12:00:00Z',
    billing: 'available',
    stripe: {
      plan: 'supersonic',
      interval: 'month',
      status: 'active',
      current_period_end: '2027-01-15T12:00:00Z',
      canceling: false,
      founders_rate: false,
    },
  };
  type Bridge = { openBilling?: unknown };
  const bridge = (): Bridge => globalThis.electron as unknown as Bridge;

  function serve(me: unknown, plans: unknown = catalog(true), plansStatus = 200) {
    server.use(
      http.get(STATUS_PATH, () => HttpResponse.json(me)),
      http.get(PLANS_PATH, () => HttpResponse.json(plans, { status: plansStatus }))
    );
  }

  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    bridge().openBilling = vi.fn().mockResolvedValue({ result: 'cancel' });
  });

  afterEach(() => {
    delete bridge().openBilling;
  });

  it('keeps the page as it was when purchase is off', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' }, catalog(false));
    render(<SubscriptionSection />);
    await openSubscription();
    expect(
      await screen.findByText('Paid subscriptions are planned. Options and prices may change.')
    ).toBeInTheDocument();
    expect(screen.queryByRole('group', { name: 'Billing period' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
  });

  it('shows the picker instead of the footer when purchase is on', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' });
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByRole('button', { name: 'Subscribe to Wing' })).toBeInTheDocument();
    expect(screen.queryByText(/Paid subscriptions are planned/)).not.toBeInTheDocument();
  });

  it('uses server trial eligibility to show the first Checkout terms', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available', trial_available: true });
    render(<SubscriptionSection />);
    await openSubscription();
    expect(
      await screen.findByRole('button', { name: 'Start 30-day free trial of Supersonic' })
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Subscribe to Wing' })).toBeInTheDocument();
    expect(screen.getByText(/Supersonic comes with a 30-day free trial/)).toBeInTheDocument();
  });

  it('shows a catalog failure as an inline alert with Retry, not the footer', async () => {
    serve(
      { tier: 'free', status: 'none', billing: 'available' },
      { error: 'catalog_unavailable' },
      503
    );
    render(<SubscriptionSection />);
    await openSubscription();
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't load plans.");
    expect(screen.queryByText(/Paid subscriptions are planned/)).not.toBeInTheDocument();
    server.use(http.get(PLANS_PATH, () => HttpResponse.json(catalog(true))));
    await userEvent.setup().click(within(alert).getByRole('button', { name: 'Retry' }));
    expect(
      await screen.findByRole('button', { name: 'Subscribe to Supersonic' })
    ).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('hides every paid control when billing is not configured', async () => {
    let plansRead = false;
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({ tier: 'free', status: 'none', billing: 'not_configured' })
      ),
      http.get(PLANS_PATH, () => {
        plansRead = true;
        return HttpResponse.json(catalog(true));
      })
    );
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByText(/Paid subscriptions are planned/)).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
    expect(plansRead).toBe(false);
  });

  it('a self-hosted server shows its own state, no picker and no planned-options footer', async () => {
    serve({ tier: 'free', status: 'none', billing: 'self_hosted' });
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByText('Your server provides premium features.')).toBeInTheDocument();
    expect(screen.queryByText(/Paid subscriptions are planned/)).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
  });

  it('a shell without the launcher gets no picker and asks to update', async () => {
    delete bridge().openBilling;
    serve({ tier: 'free', status: 'none', billing: 'available' });
    render(<SubscriptionSection />);
    await openSubscription();
    expect(
      await screen.findByText('Update Concord to subscribe or manage billing.')
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
  });

  it('a Wing +1 sees Leave Wing, not the picker (#2033 PR-5)', async () => {
    serve({
      tier: 'premium',
      status: 'active',
      source: 'seat',
      billing: 'available',
      seat: { role: 'member', seat_id: 's-1', state: 'active', counterpart_user_id: 'u-2' },
    });
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByRole('button', { name: 'Leave Wing' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
  });

  it('shows an open Wing invite above the plan card (#2033 PR-5)', async () => {
    serve({
      tier: 'free',
      status: 'none',
      billing: 'available',
      invites: [{ invite_id: 's-1', payer_user_id: 'u-2', expires_at: '2027-01-15T12:00:00Z' }],
    });
    render(<SubscriptionSection />);
    await openSubscription();
    const region = await screen.findByRole('region', { name: 'Wing invite' });
    expect(region).toHaveTextContent('a friend invited you to their Wing.');
    expect(within(region).getByRole('button', { name: 'Accept' })).toBeInTheDocument();
  });

  it('shows the seat_member notice when Checkout refuses a +1 (#2033 PR-5)', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' });
    server.use(
      http.post(`${API_BASE}/billing/checkout-session`, () =>
        HttpResponse.json({ error: 'seat_member' }, { status: 409 })
      )
    );
    render(<SubscriptionSection />);
    await openSubscription();
    await userEvent.setup().click(await screen.findByRole('button', { name: 'Subscribe to Wing' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/Leave Wing to buy your own plan/);
  });

  it('a subscriber sees management, not the picker', async () => {
    serve(ACTIVE);
    render(<SubscriptionSection />);
    await openSubscription();
    expect(await screen.findByRole('button', { name: 'Manage subscription' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
  });

  it('buys, confirms on the subscription read, and replaces the picker with the active plan', async () => {
    let live = false;
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json(live ? ACTIVE : { tier: 'free', status: 'none', billing: 'available' })
      ),
      http.get(PLANS_PATH, () => HttpResponse.json(catalog(true))),
      http.post(CHECKOUT_PATH, () =>
        HttpResponse.json({ url: 'https://pay.concordvoice.com/c/1' }, { status: 201 })
      )
    );
    bridge().openBilling = vi.fn().mockResolvedValue({ result: 'success' });
    render(<SubscriptionSection />);
    await openSubscription();
    await userEvent
      .setup()
      .click(await screen.findByRole('button', { name: 'Subscribe to Supersonic' }));
    expect(await screen.findByText('Confirming')).toBeInTheDocument();

    live = true;
    // What an entitlements_changed push or a reconnect hydrate does while Confirming.
    await act(async () => {
      await useBillingStore.getState().fetchMe();
    });
    expect(await screen.findByRole('button', { name: 'Manage subscription' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /subscribe/i })).not.toBeInTheDocument();
    expect(screen.getByText('Your subscription is active.')).toBeInTheDocument();
    useBillingStore.getState().reset();
  });

  it('shows the manage copy with a Manage action when Checkout says one exists', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' });
    render(<SubscriptionSection />);
    await openSubscription();
    await screen.findByRole('button', { name: 'Subscribe to Supersonic' });
    act(() => useBillingStore.setState({ notice: 'manage_existing' }));
    const alert = screen.getByRole('alert');
    expect(alert).toHaveTextContent('You already have a subscription.');
    expect(within(alert).getByRole('button', { name: 'Manage subscription' })).toBeInTheDocument();
  });

  it('reports a closed Checkout politely, not as an alert', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' });
    render(<SubscriptionSection />);
    await openSubscription();
    await screen.findByRole('button', { name: 'Subscribe to Supersonic' });
    act(() => useBillingStore.setState({ notice: 'checkout_closed' }));
    expect(
      screen.getByText('Checkout closed. If you finished paying, this page updates on its own.')
    ).toHaveRole('status');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('explains how to withdraw a booked change before choosing another', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' });
    render(<SubscriptionSection />);
    await openSubscription();
    act(() => useBillingStore.setState({ notice: 'pending_plan_change' }));
    expect(screen.getByRole('alert')).toHaveTextContent(
      'Keep your current plan first to withdraw the booked change, then choose a new one.'
    );
  });

  it('does not replay a notice or the activation cue when the page is reopened', async () => {
    serve({ tier: 'free', status: 'none', billing: 'available' });
    const { unmount } = render(<SubscriptionSection />);
    await openSubscription();
    await screen.findByRole('button', { name: 'Subscribe to Supersonic' });
    act(() => useBillingStore.setState({ notice: 'checkout_failed', activated: true }));
    unmount();
    expect(useBillingStore.getState().notice).toBeNull();
    expect(useBillingStore.getState().activated).toBe(false);
  });
});
