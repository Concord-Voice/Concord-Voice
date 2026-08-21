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

// Reserved: the intro-video ciphertexts (#2859). Never use as arbitrary input.
const BOARD = 'AVYJANBUCLDSHKQVBYJWWHYDTLF';
const PICKUP = 'ZVUCDDSJCSFOAREL';

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

  it.each([
    ['the board literal', BOARD, /wiped off the board/i],
    ['the pickup literal', PICKUP, /1596 Paris Ave/i],
  ] as const)('answers %s locally without issuing a request', async (_label, literal, expected) => {
    const user = userEvent.setup();
    const onRedeemed = vi.fn();
    const redeemSpy = vi.fn(() => HttpResponse.json({ success: true, description: 'x' }));
    server.use(http.post(REDEEM_PATH, redeemSpy));
    render(<RedeemCodeForm onRedeemed={onRedeemed} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), literal);
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    // Positive DOM gate FIRST. The negatives below are only sound because this
    // has already settled the state under test (tests.md:241-250) -- a bare
    // negative after no gate, or a negative wrapped in waitFor, proves nothing.
    expect(await screen.findByRole('status')).toHaveTextContent(expected);
    expect(redeemSpy).not.toHaveBeenCalled();
    expect(onRedeemed).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    // Nothing was consumed, so the input keeps what the viewer transcribed.
    expect(screen.getByRole('textbox', { name: 'Code' })).toHaveValue(literal);
    // The submit machinery never ran, so the form is still usable. This fails if
    // the short-circuit is placed after setSubmitting(true), where the early
    // return skips the finally that clears it.
    expect(screen.getByRole('button', { name: 'Redeem' })).toBeEnabled();
  });

  it('keeps the notice visible while the user keeps typing', async () => {
    const user = userEvent.setup();
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ success: true })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), BOARD);
    await user.click(screen.getByRole('button', { name: 'Redeem' }));
    expect(await screen.findByRole('status')).toHaveTextContent(/wiped off the board/i);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'X');

    // Only kind === 'error' clears on keystroke. A bare `if (result)` would wipe
    // this -- and would wipe the success message too.
    expect(screen.getByRole('status')).toHaveTextContent(/wiped off the board/i);
  });

  it('sends the raw trimmed input, never a locally normalized form', async () => {
    const user = userEvent.setup();
    let sentCode: unknown = null;
    server.use(
      http.post(REDEEM_PATH, async ({ request }) => {
        const body = (await request.json()) as { code?: string };
        sentCode = body.code;
        return HttpResponse.json({ success: true, description: 'ok' });
      })
    );
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), '  cv-abcde-fghij  ');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('status')).toHaveTextContent('ok');
    // Byte-identical: hyphens kept, case kept. Canonicalization belongs to the
    // server (code.go normalizeSymbols), which computes the hash. A second
    // client-side implementation that drifted would accept codes it rejects.
    expect(sentCode).toBe('cv-abcde-fghij');
  });

  it('clears the error alert on the next keystroke', async () => {
    const user = userEvent.setup();
    server.use(http.post(REDEEM_PATH, () => HttpResponse.json({ error: 'x' }, { status: 400 })));
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'BAD-CODE');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/not valid/i);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'X');

    // `user.type` flushes the synchronous onChange inside its own act(), so this
    // negative is gated and stays synchronous (tests.md:241-250). Without this test
    // the TRUE branch of `if (result?.kind === 'error')` never executes at all, and
    // deleting its body leaves the entire suite green.
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('on a network failure shows the unreachable-server alert', async () => {
    const user = userEvent.setup();
    server.use(http.post(REDEEM_PATH, () => HttpResponse.error()));
    render(<RedeemCodeForm onRedeemed={() => {}} />);

    await user.type(screen.getByRole('textbox', { name: 'Code' }), 'PROMO-NETFAIL');
    await user.click(screen.getByRole('button', { name: 'Redeem' }));

    expect(await screen.findByRole('alert')).toHaveTextContent(/Could not reach the server/i);
  });
});
