import {
  afterAll,
  afterEach,
  beforeAll,
  beforeEach,
  describe,
  expect,
  expectTypeOf,
  it,
} from 'vitest';
import { act, renderHook, waitFor } from '../../test-utils';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useSubscription } from '@/renderer/hooks/ui/useSubscription';
import type { SubscriptionSource } from '@/renderer/hooks/ui/useSubscription';

const API_BASE = 'http://localhost:8080';
const STATUS_PATH = `${API_BASE}/api/v1/subscriptions/me`;

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('useSubscription (#1304)', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('publishes only code and Stripe subscription sources', () => {
    expectTypeOf<SubscriptionSource>().toEqualTypeOf<'code' | 'stripe'>();
  });

  it('starts in the loading state before the status resolves', () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ tier: 'free', status: 'none' })));
    const { result } = renderHook(() => useSubscription());
    expect(result.current.loading).toBe(true);
    expect(result.current.error).toBe(false);
  });

  it('reports the free-default shape when there is no live subscription', async () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ tier: 'free', status: 'none' })));
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.tier).toBe('free');
    expect(result.current.status).toBe('none');
    expect(result.current.source).toBeUndefined();
    expect(result.current.expiry).toBeUndefined();
    expect(result.current.error).toBe(false);
  });

  it('reports an active-from-code premium grant with source + expiry', async () => {
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
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.tier).toBe('premium');
    expect(result.current.status).toBe('active');
    expect(result.current.source).toBe('code');
    expect(result.current.expiry).toBe('2027-01-01T00:00:00Z');
  });

  it('reports Stripe as the other supported subscription source', async () => {
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({ tier: 'premium', status: 'active', source: 'stripe' })
      )
    );
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.source).toBe('stripe');
  });

  it('drops an unknown source value rather than surfacing it', async () => {
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({ tier: 'premium', status: 'active', source: 'legacy' })
      )
    );
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.source).toBeUndefined();
  });

  it('falls back to the store tier (never blocks) on a server error', async () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ error: 'boom' }, { status: 503 })));
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe(true);
    expect(result.current.status).toBe('unknown');
    // Store default is the free floor, so the fallback tier is 'free' — never a
    // fabricated premium on a degraded read.
    expect(result.current.tier).toBe('free');
  });

  it('falls back to the store tier on a network/transport error', async () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.error()));
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toBe(true);
    expect(result.current.tier).toBe('free');
  });

  it('refetch re-reads the status (post-redeem refresh path)', async () => {
    let call = 0;
    server.use(
      http.get(STATUS_PATH, () => {
        call += 1;
        return call === 1
          ? HttpResponse.json({ tier: 'free', status: 'none' })
          : HttpResponse.json({ tier: 'premium', status: 'active', source: 'code' });
      })
    );
    const { result } = renderHook(() => useSubscription());
    await waitFor(() => expect(result.current.tier).toBe('free'));

    act(() => result.current.refetch());
    await waitFor(() => expect(result.current.tier).toBe('premium'));
    expect(result.current.source).toBe('code');
  });
});
