import { describe, it, expect, beforeAll, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { eraseAccount, PrivacyApiError } from '@/renderer/api/privacy';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

describe('eraseAccount', () => {
  it('POSTs empty body when no clientId supplied and resolves on 204', async () => {
    let received: unknown = 'not-set';
    server.use(
      http.post(`${API_BASE}/api/v1/privacy/erase-account`, async ({ request }) => {
        received = await request.json();
        return new HttpResponse(null, { status: 204 });
      })
    );

    await expect(eraseAccount()).resolves.toBeUndefined();
    expect(received).toEqual({});
  });

  it('POSTs with clientId when supplied', async () => {
    let received: unknown = null;
    server.use(
      http.post(`${API_BASE}/api/v1/privacy/erase-account`, async ({ request }) => {
        received = await request.json();
        return new HttpResponse(null, { status: 204 });
      })
    );

    await eraseAccount('xyz789');
    expect(received).toEqual({ clientId: 'xyz789' });
  });

  it('throws PrivacyApiError on 502', async () => {
    server.use(
      http.post(
        `${API_BASE}/api/v1/privacy/erase-account`,
        () => new HttpResponse(null, { status: 502 })
      )
    );

    await expect(eraseAccount()).rejects.toBeInstanceOf(PrivacyApiError);
    await expect(eraseAccount()).rejects.toMatchObject({ status: 502 });
  });

  it('does not include clientId value in error message', async () => {
    server.use(
      http.post(
        `${API_BASE}/api/v1/privacy/erase-account`,
        () => new HttpResponse(null, { status: 500 })
      )
    );

    const sentinel = 'sentinel-value-frobozz-222';
    try {
      await eraseAccount(sentinel);
      throw new Error('should have thrown');
    } catch (err) {
      expect(err).toBeInstanceOf(PrivacyApiError);
      expect((err as Error).message).not.toContain(sentinel);
    }
  });
});
