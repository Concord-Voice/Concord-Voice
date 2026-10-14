import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { render, screen, userEvent, within } from '../../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../../mocks/server';
import { resetAllStores } from '../../../../helpers/store-helpers';
import SubscriptionSection from '@/renderer/components/Settings/subscription/SubscriptionSection';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useSubscriptionStore } from '@/renderer/stores/auth/subscriptionStore';

const API_BASE = 'http://localhost:8080/api/v1';
const STATUS_PATH = `${API_BASE}/subscriptions/me`;

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
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
    expect(await screen.findByRole('status')).toHaveTextContent('Supersonic access granted.');
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
