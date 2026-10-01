import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  apiFetchInContext,
  captureApiRequestContext,
} from '@/renderer/services/system/requestContext';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';

// #3509 review: a multi-request operation is admitted against ONE capture of
// the account and server, so a later request of it never goes out as another
// account or to another server.

const PATH = '/api/v1/request-context-probe';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => {
  server.resetHandlers();
  resetRuntimeServerBase();
});
afterAll(() => server.close());
beforeEach(() => resetAllStores());

/** Counts the probe requests that reached the network. */
function probe(): { hits: () => number } {
  const handler = vi.fn(() => HttpResponse.json({ ok: true }));
  server.use(http.get(`*${PATH}`, handler));
  return { hits: () => handler.mock.calls.length };
}

describe('captureApiRequestContext', () => {
  it('captures the signed-in account and the selected server', () => {
    useAuthStore.setState({ accessToken: 'tok-a', sessionId: 'sid-a', authGeneration: 7 });

    const context = captureApiRequestContext();

    expect(context.authLifecycle).toEqual({
      accessToken: 'tok-a',
      sessionId: 'sid-a',
      authGeneration: 7,
    });
    expect(context.serverSelection.apiBase).toBe(new URL(context.serverSelection.apiBase).origin);
  });
});

describe('apiFetchInContext', () => {
  it('dispatches when nothing changed since the capture', async () => {
    const { hits } = probe();
    const context = captureApiRequestContext();

    const res = await apiFetchInContext(PATH, { method: 'GET' }, context);

    expect(res.ok).toBe(true);
    expect(hits()).toBe(1);
  });

  it('refuses to dispatch once another account signed in after the capture', async () => {
    const { hits } = probe();
    const context = captureApiRequestContext();
    useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));

    await expect(apiFetchInContext(PATH, { method: 'GET' }, context)).rejects.toMatchObject({
      name: 'AbortError',
    });
    expect(hits()).toBe(0);
  });

  it('refuses to dispatch once another server was selected after the capture', async () => {
    const { hits } = probe();
    const context = captureApiRequestContext();
    setRuntimeServerBase('https://other.concordvoice.test');

    await expect(apiFetchInContext(PATH, { method: 'GET' }, context)).rejects.toMatchObject({
      name: 'AbortError',
    });
    expect(hits()).toBe(0);
  });

  it('without a context, admits the request against the account current at the call', async () => {
    const { hits } = probe();
    useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));

    const res = await apiFetchInContext(PATH, { method: 'GET' }, undefined);

    expect(res.ok).toBe(true);
    expect(hits()).toBe(1);
  });
});
