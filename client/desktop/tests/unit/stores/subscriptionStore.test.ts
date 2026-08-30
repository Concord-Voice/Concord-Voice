import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock apiClient before importing the store. The store imports `apiFetch` and
// `safeJson` as ESM named bindings, which vi.spyOn cannot reassign at the
// namespace level — the repo convention (see userStore.changePassword.test.ts)
// is a hoisted vi.mock factory driven via vi.mocked(...).
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: vi.fn(),
}));

import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { apiFetch, safeJson } from '@/renderer/services/system/apiClient';

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
    // Reset hydrated too — the #2172 reconnect-vs-first-load branch keys on it, so it
    // must not leak across tests (a prior success sets it true).
    useSubscriptionStore.setState({
      entitlement: FREE_ENTITLEMENT,
      degraded: false,
      hydrated: false,
    });
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

  it('does not restore a stale premium response after reset', async () => {
    let releaseFetch: ((response: Response) => void) | undefined;
    mockApiFetch.mockImplementationOnce(
      () =>
        new Promise<Response>((resolve) => {
          releaseFetch = resolve;
        })
    );
    mockSafeJson.mockResolvedValue(premiumDTO);
    let current = true;
    const controller = new AbortController();

    const hydration = useSubscriptionStore.getState().hydrate({
      signal: controller.signal,
      isCurrent: () => current,
    });
    useSubscriptionStore.getState().reset();
    current = false;
    controller.abort();
    releaseFetch?.(new Response(JSON.stringify(premiumDTO), { status: 200 }));
    await hydration;

    expect(useSubscriptionStore.getState()).toMatchObject({
      entitlement: FREE_ENTITLEMENT,
      degraded: false,
      hydrated: false,
    });
  });

  it('hydrate() FIRST-LOAD error fails closed to FREE + degraded (never grant premium)', async () => {
    // hydrated=false (beforeEach): a first-load failure has no authoritative prior value,
    // so it fails closed to the free floor — a user who never authenticated as premium
    // can never obtain it via a degraded state. Pre-set premium proves it resets to free.
    useSubscriptionStore.setState({ entitlement: premiumDTO, hydrated: false });
    mockApiFetch.mockRejectedValue(new Error('network'));
    await useSubscriptionStore.getState().hydrate();
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
    expect(useSubscriptionStore.getState().degraded).toBe(true);
    expect(useSubscriptionStore.getState().hydrated).toBe(false);
  });

  it('hydrate() RECONNECT error preserves the last-known PREMIUM entitlement (#2172)', async () => {
    // A prior authoritative hydrate succeeded (hydrated=true) as premium. A transient
    // reconnect failure must NOT clamp the user to free — preserve the last-known-good
    // entitlement and only flip degraded, so their screen share / features hold.
    useSubscriptionStore.setState({ entitlement: premiumDTO, hydrated: true, degraded: false });
    mockApiFetch.mockRejectedValue(new Error('network'));
    await useSubscriptionStore.getState().hydrate();
    expect(useSubscriptionStore.getState().entitlement.tier).toBe('premium');
    expect(useSubscriptionStore.getState().degraded).toBe(true);
    expect(useSubscriptionStore.getState().hydrated).toBe(true);
  });

  it('hydrate() RECONNECT error on an authoritative FREE user keeps FREE (no escape) (#2172)', async () => {
    // The preserve-last-known behaviour never escalates: a user authoritatively hydrated
    // as free keeps free through a reconnect failure.
    useSubscriptionStore.setState({
      entitlement: FREE_ENTITLEMENT,
      hydrated: true,
      degraded: false,
    });
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
