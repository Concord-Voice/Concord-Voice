import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
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
    // The store's post-redeem hydrate hits /entitlements — stub a benign 200.
    server.use(
      http.get(ENTITLEMENTS_PATH, () => HttpResponse.json({ tier: 'free' }, { status: 200 }))
    );
  });

  it('renders a labelled input and a submit control', () => {
    render(<RedeemCodeForm onRedeemed={() => {}} />);
    expect(screen.getByLabelText(/Redeem a code/i)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /Redeem/i })).toBeInTheDocument();
  });

  it('on 200 shows the granted description in a role=status and calls onRedeemed', async () => {
    const onRedeemed = vi.fn();
    server.use(
      http.post(REDEEM_PATH, () =>
        HttpResponse.json({ success: true, description: 'Premium subscription for 12 months' })
      )
    );
    render(<RedeemCodeForm onRedeemed={onRedeemed} />);

    await userEvent.type(screen.getByLabelText(/Redeem a code/i), 'KS-AAAA-BBBB-CCCC');
    await userEvent.click(screen.getByRole('button', { name: /Redeem/i }));

    const status = await screen.findByRole('status');
    expect(status).toHaveTextContent('Premium subscription for 12 months');
    await waitFor(() => expect(onRedeemed).toHaveBeenCalledTimes(1));
  });

  it('on 400 shows a generic (no-oracle) alert', async () => {
    server.use(
      http.post(REDEEM_PATH, () =>
        HttpResponse.json({ error: 'That code is not valid.' }, { status: 400 })
      )
    );
    render(<RedeemCodeForm onRedeemed={() => {}} />);
    await userEvent.type(screen.getByLabelText(/Redeem a code/i), 'BAD-CODE');
    await userEvent.click(screen.getByRole('button', { name: /Redeem/i }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/not valid/i);
  });

  it('on 409 shows the already-redeemed alert', async () => {
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 409 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);
    await userEvent.type(screen.getByLabelText(/Redeem a code/i), 'KS-DUP');
    await userEvent.click(screen.getByRole('button', { name: /Redeem/i }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/already redeemed/i);
  });

  it('on 429 shows the rate-limited alert', async () => {
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 429 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);
    await userEvent.type(screen.getByLabelText(/Redeem a code/i), 'KS-RATE');
    await userEvent.click(screen.getByRole('button', { name: /Redeem/i }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/Too many attempts/i);
  });

  it('on 500 shows the generic try-again alert', async () => {
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 500 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);
    await userEvent.type(screen.getByLabelText(/Redeem a code/i), 'KS-BOOM');
    await userEvent.click(screen.getByRole('button', { name: /Redeem/i }));

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/Please try again/i);
  });

  it('does not submit an empty/whitespace code (client-side non-empty guard)', async () => {
    let called = false;
    server.use(
      http.post(REDEEM_PATH, () => {
        called = true;
        return HttpResponse.json({ success: true, description: 'x' });
      })
    );
    render(<RedeemCodeForm onRedeemed={() => {}} />);
    // Submit is disabled while empty; type whitespace only → still disabled.
    await userEvent.type(screen.getByLabelText(/Redeem a code/i), '   ');
    expect(screen.getByRole('button', { name: /Redeem/i })).toBeDisabled();
    expect(called).toBe(false);
  });
});
