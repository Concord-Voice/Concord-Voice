import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock apiClient before importing the store. The store imports `apiFetch` and
// `safeJson` as ESM named bindings, which vi.spyOn cannot reassign at the
// namespace level — the repo convention (see userStore.changePassword.test.ts)
// is a hoisted vi.mock factory driven via vi.mocked(...).
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/subscriptionStore';
import { apiFetch, safeJson } from '@/renderer/services/apiClient';

const mockApiFetch = vi.mocked(apiFetch);
const mockSafeJson = vi.mocked(safeJson);

const premiumDTO = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  allowMusicMode: true,
  maxMessageChars: 10240,
};

describe('subscriptionStore', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useSubscriptionStore.setState({ entitlement: FREE_ENTITLEMENT, degraded: false });
  });

  it('defaults to the FREE_ENTITLEMENT floor', () => {
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
    expect(useSubscriptionStore.getState().degraded).toBe(false);
  });

  it('hydrate() success swaps to the server set', async () => {
    mockApiFetch.mockResolvedValue(new Response(JSON.stringify(premiumDTO), { status: 200 }));
    mockSafeJson.mockResolvedValue(premiumDTO);
    await useSubscriptionStore.getState().hydrate();
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('premium');
    expect(useSubscriptionStore.getState().degraded).toBe(false);
  });

  it('hydrate() error stays FREE + sets degraded (fail-closed)', async () => {
    useSubscriptionStore.setState({ entitlement: premiumDTO }); // prove it resets to free
    mockApiFetch.mockRejectedValue(new Error('network'));
    await useSubscriptionStore.getState().hydrate();
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
    expect(useSubscriptionStore.getState().degraded).toBe(true);
  });

  it('hydrate() with a malformed 200 body fails closed to free (fetch-path zod validation)', async () => {
    useSubscriptionStore.setState({ entitlement: premiumDTO }); // prove it resets to free
    // 200 OK but the body is missing required capability fields. safeJson would
    // return it (Content-Type check + cast only); the zod validation must reject
    // it so the store does NOT end up with an entitlement full of undefined caps.
    mockApiFetch.mockResolvedValue(new Response('{}', { status: 200 }));
    mockSafeJson.mockResolvedValue({ tier: 'premium' });
    await useSubscriptionStore.getState().hydrate();
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
    expect(useSubscriptionStore.getState().degraded).toBe(true);
  });

  it('setEntitlement updates the set', () => {
    useSubscriptionStore.getState().setEntitlement(premiumDTO);
    expect(useSubscriptionStore.getState().entitlement.allowMusicMode).toBe(true);
  });

  it('reset() restores the free floor (logout/account-switch ghost-wipe)', () => {
    useSubscriptionStore.setState({ entitlement: premiumDTO, degraded: true });
    useSubscriptionStore.getState().reset();
    expect(useSubscriptionStore.getState().entitlement).toEqual(FREE_ENTITLEMENT);
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
    expect(useSubscriptionStore.getState().degraded).toBe(false);
  });
});
