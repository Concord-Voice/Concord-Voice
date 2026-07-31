import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, userEvent, waitFor } from '../../../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../../../mocks/server';
import { resetAllStores } from '../../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import RedeemCodeForm from '@/renderer/components/Settings/subscription/RedeemCodeForm';

const API_BASE = 'http://localhost:8080';
const REDEEM_PATH = `${API_BASE}/api/v1/redeem`;
const ENTITLEMENTS_PATH = `${API_BASE}/api/v1/entitlements`;

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('RedeemCodeForm (#1304)', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    server.use(
      http.get(ENTITLEMENTS_PATH, () => HttpResponse.json({ tier: 'free' }, { status: 200 }))
    );
  });

  it('renders neutral code input copy and a submit control', () => {
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    expect(screen.getByRole('textbox', { name: 'Code' })).toHaveAttribute(
      'placeholder',
      'Enter a code'
    );
    expect(screen.getByText('Enter a code you received from Concord Voice.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Redeem' })).toBeInTheDocument();
  });

  it('on 200 accepts arbitrary non-empty text, reports the grant, and calls onRedeemed', async () => {
    const user = userEvent.setup();
    const onRedeemed = vi.fn();
    server.use(
      http.post(REDEEM_PATH, () =>
        HttpResponse.json({ success: true, description: 'Premium subscription for 12 months' })
      )
    );
    render(<RedeemCodeForm onRedeemed={onRedeemed} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'PROMO thank-you code');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('status')).toHaveTextContent(
      'Premium subscription for 12 months'
    );
    await waitFor(() => expect(onRedeemed).toHaveBeenCalledTimes(1));
  });

  it('on 400 shows a generic no-oracle alert', async () => {
    const user = userEvent.setup();
    server.use(
      http.post(REDEEM_PATH, () =>
        HttpResponse.json({ error: 'That code is not valid.' }, { status: 400 })
      )
    );
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'BAD-CODE');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/not valid/i);
  });

  it('on 409 shows the already-redeemed alert', async () => {
    const user = userEvent.setup();
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 409 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'PROMO-DUP');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/already redeemed/i);
  });

  it('on 429 shows the rate-limited alert', async () => {
    const user = userEvent.setup();
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 429 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'PROMO-RATE');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/Too many attempts/i);
  });

  it('on 500 shows the generic try-again alert', async () => {
    const user = userEvent.setup();
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 500 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'PROMO-BOOM');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/Please try again/i);
  });

  it('does not submit an empty or whitespace-only code', async () => {
    const user = userEvent.setup();
    let called = false;
    server.use(
      http.post(REDEEM_PATH, () => {
        called = true;
        return HttpResponse.json({ success: true, description: 'x' });
      })
    );
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), '   ');

    expect(screen.getByRole('button', { name: 'Redeem' })).toBeDisabled();
    expect(called).toBe(false);
  });
});
