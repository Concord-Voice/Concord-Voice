// @vitest-environment jsdom
import { render, screen, fireEvent } from '../../../test-utils';
import { describe, it, expect, beforeEach, afterEach, afterAll, beforeAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';

// Real apiClient + MSW + real userStore. Stub ONLY crypto/version (no device key in jsdom);
// the real userStore is used (mocking it would break resetAllStores's clearUser()).
const { mockE2EE } = vi.hoisted(() => ({
  mockE2EE: { isInitialized: true, signAgeClaim: vi.fn().mockResolvedValue('mockSigB64') },
}));
vi.mock('@/renderer/services/e2eeService', () => ({ e2eeService: mockE2EE }));

import NsfwContentGate from '@/renderer/components/Settings/NsfwContentGate';
import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';

// 'error' (stricter than 'bypass') fails the test on any request the handlers don't cover —
// correct for this single-flow integration test (only GET public-key + PUT claim expected).
beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

describe('NsfwContentGate (integration via MSW)', () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ['Date'] });
    vi.setSystemTime(new Date('2026-06-20T00:00:00Z'));
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
    useUserStore.setState({
      user: { id: '11111111-1111-4111-8111-111111111111', username: 'tester' },
    });
    (window as unknown as { electron: { getVersion: () => Promise<string> } }).electron = {
      getVersion: vi.fn().mockResolvedValue('0.1.65'),
    };
  });
  afterEach(() => vi.useRealTimers());

  it('submits a real signed claim through the service and unlocks for an adult', async () => {
    let putBody: Record<string, unknown> | null = null;
    server.use(
      http.get('*/api/v1/users/:id/public-key', () => HttpResponse.json({ key_version: 1 })),
      http.put('*/api/v1/age/claim', async ({ request }) => {
        putBody = (await request.json()) as Record<string, unknown>;
        return new HttpResponse(null, { status: 200 });
      })
    );

    render(<NsfwContentGate />);
    fireEvent.change(screen.getByRole('spinbutton', { name: /year/i }), {
      target: { value: '2000' },
    });
    fireEvent.change(screen.getByRole('spinbutton', { name: /month/i }), {
      target: { value: '1' },
    });
    fireEvent.change(screen.getByRole('spinbutton', { name: /day/i }), { target: { value: '1' } });
    fireEvent.click(screen.getByRole('button', { name: /verify age/i }));
    fireEvent.click(screen.getByRole('button', { name: /submit/i }));

    expect(await screen.findByText(/now enabled/i)).toBeInTheDocument();
    // The wire body carries the derived booleans + signed metadata — NO date-of-birth
    // field. Asserting field-shape (not a substring over the random hex nonce) keeps this
    // privacy assertion deterministic / non-flaky.
    expect(putBody).toMatchObject({ valid_age: true, nsfw_auth: true, signature: 'mockSigB64' });
    expect(putBody).not.toHaveProperty('birthdate');
    expect(putBody).not.toHaveProperty('year');
    expect(putBody).not.toHaveProperty('month');
    expect(putBody).not.toHaveProperty('day');
    expect(putBody).not.toHaveProperty('dob');
  });
});
