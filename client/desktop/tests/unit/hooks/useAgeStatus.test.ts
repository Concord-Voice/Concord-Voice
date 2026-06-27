import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useAgeStatus } from '@/renderer/hooks/useAgeStatus';

const API_BASE = 'http://localhost:8080';
const STATUS_PATH = `${API_BASE}/api/v1/age/status`;

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('useAgeStatus (#1763)', () => {
  beforeEach(() => {
    resetAllStores();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('starts in the loading state before the status resolves', () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ verified: false })));
    const { result } = renderHook(() => useAgeStatus());
    expect(result.current).toEqual({ state: 'loading' });
  });

  it('reports verified + nsfw-enabled for an adult record', async () => {
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({ verified: true, valid_age: true, nsfw_auth: true })
      )
    );
    const { result } = renderHook(() => useAgeStatus());
    await waitFor(() =>
      expect(result.current).toEqual({ state: 'verified', validAge: true, nsfwAuth: true })
    );
  });

  it('reports verified-but-locked for a 16–17 record (valid_age true, nsfw_auth false)', async () => {
    server.use(
      http.get(STATUS_PATH, () =>
        HttpResponse.json({ verified: true, valid_age: true, nsfw_auth: false })
      )
    );
    const { result } = renderHook(() => useAgeStatus());
    await waitFor(() =>
      expect(result.current).toEqual({ state: 'verified', validAge: true, nsfwAuth: false })
    );
  });

  it('does not grant valid-age/nsfw when verified:true arrives without the booleans (strict coercion)', async () => {
    // A malformed/partial verified response must coerce absent booleans to false — never
    // fabricate eligibility. Strict `=== true` is the fail-safe.
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ verified: true })));
    const { result } = renderHook(() => useAgeStatus());
    await waitFor(() =>
      expect(result.current).toEqual({ state: 'verified', validAge: false, nsfwAuth: false })
    );
  });

  it('reports unverified when no record exists', async () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ verified: false })));
    const { result } = renderHook(() => useAgeStatus());
    await waitFor(() => expect(result.current).toEqual({ state: 'unverified' }));
  });

  it('fails CLOSED to unverified on a server error (never grants verified on a degraded read)', async () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.json({ error: 'boom' }, { status: 503 })));
    const { result } = renderHook(() => useAgeStatus());
    await waitFor(() => expect(result.current).toEqual({ state: 'unverified' }));
  });

  it('fails CLOSED to unverified on a network/transport error', async () => {
    server.use(http.get(STATUS_PATH, () => HttpResponse.error()));
    const { result } = renderHook(() => useAgeStatus());
    await waitFor(() => expect(result.current).toEqual({ state: 'unverified' }));
  });
});
