import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach } from 'vitest';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useInvitePreview, clearInvitePreviewCache } from '@/renderer/hooks/useInvitePreview';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('useInvitePreview', () => {
  beforeEach(() => {
    resetAllStores();
    clearInvitePreviewCache();
    useAuthStore.getState().setAccessToken('mock-token');
  });

  it('resolves a valid invite to ready', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () =>
        HttpResponse.json({
          server_name: 'Acme',
          server_icon: '/api/v1/media/server-icons/abc',
          server_banner: null,
          member_count: 42,
          valid: true,
        })
      )
    );
    const { result } = renderHook(() => useInvitePreview('GHJKMNPQ'));
    expect(result.current.status).toBe('loading');
    await waitFor(() => expect(result.current.status).toBe('ready'));
    if (result.current.status === 'ready') {
      expect(result.current.info.server_name).toBe('Acme');
    }
  });

  it('marks valid:false invites as invalid', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () =>
        HttpResponse.json({
          server_name: '',
          server_icon: null,
          server_banner: null,
          member_count: 0,
          valid: false,
        })
      )
    );
    const { result } = renderHook(() => useInvitePreview('KKKKMNPQ'));
    await waitFor(() => expect(result.current.status).toBe('invalid'));
  });

  it('marks a request error as invalid', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 })
      )
    );
    const { result } = renderHook(() => useInvitePreview('GHKMNPQR'));
    await waitFor(() => expect(result.current.status).toBe('invalid'));
  });

  it('does not permanently cache a transient failure — retries on next mount', async () => {
    let calls = 0;
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, () => {
        calls++;
        if (calls === 1) return HttpResponse.json({ error: 'blip' }, { status: 503 });
        return HttpResponse.json({
          server_name: 'Acme',
          server_icon: null,
          server_banner: null,
          member_count: 3,
          valid: true,
        });
      })
    );
    const first = renderHook(() => useInvitePreview('GHJKMNPQ'));
    await waitFor(() => expect(first.result.current.status).toBe('invalid'));
    first.unmount();
    const second = renderHook(() => useInvitePreview('GHJKMNPQ'));
    await waitFor(() => expect(second.result.current.status).toBe('ready'));
    expect(calls).toBe(2);
  });

  it('deduplicates concurrent requests for the same code', async () => {
    let requestCount = 0;
    server.use(
      http.get(`${API_BASE}/api/v1/invites/:code`, async () => {
        requestCount++;
        // small delay so both hooks mount and share the in-flight promise
        await new Promise((r) => setTimeout(r, 30));
        return HttpResponse.json({
          server_name: 'Acme',
          server_icon: null,
          server_banner: null,
          member_count: 1,
          valid: true,
        });
      })
    );
    const { result: r1 } = renderHook(() => useInvitePreview('GHJKMNPQ'));
    const { result: r2 } = renderHook(() => useInvitePreview('GHJKMNPQ'));
    await waitFor(() => expect(r1.current.status).toBe('ready'));
    await waitFor(() => expect(r2.current.status).toBe('ready'));
    expect(requestCount).toBe(1);
  });
});
