import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import {
  mergeExpirationPolicy,
  parseExpirationPolicyFromListRow,
  parseExpirationPolicyResponse,
  updateExpirationPolicy,
} from '@/renderer/services/messaging/expirationPolicyApi';

const API_BASE = 'http://localhost:8080';
const policyWire = {
  window_seconds: 86400,
  updated_at: '2026-09-08T05:00:00Z',
  revision: 4,
  backfill_pending: false,
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());
beforeEach(() => resetAllStores());

describe('expiration policy wire contract', () => {
  it.each([3600, 86400, 604800, 2592000, null])(
    'accepts supported window %s and null metadata',
    (windowSeconds) => {
      expect(
        parseExpirationPolicyResponse({
          ...policyWire,
          window_seconds: windowSeconds,
          updated_at: null,
        })
      ).toEqual({ windowSeconds, updatedAt: null, revision: 4, backfillPending: false });
    }
  );
  it.each([
    ['response', parseExpirationPolicyResponse, policyWire],
    [
      'list row',
      parseExpirationPolicyFromListRow,
      {
        expiration_window_seconds: 86400,
        expiration_updated_at: policyWire.updated_at,
        expiration_revision: 4,
        expiration_backfill_pending: false,
      },
    ],
  ])('maps a valid %s and strips unknown fields', (_name, parse, raw) => {
    expect(parse({ ...raw, future_metadata: true })).toEqual({
      windowSeconds: 86400,
      updatedAt: policyWire.updated_at,
      revision: 4,
      backfillPending: false,
    });
  });

  it.each([
    { ...policyWire, revision: -1 },
    { ...policyWire, revision: 1.5 },
    { ...policyWire, window_seconds: 300 },
    { ...policyWire, updated_at: 'yesterday' },
    { ...policyWire, backfill_pending: 'false' },
    { ...policyWire, window_seconds: undefined },
  ])('rejects malformed response policy %j', (raw) => {
    expect(parseExpirationPolicyResponse(raw)).toBeUndefined();
  });

  it('orders whole policies by revision, then completed over pending at equality', () => {
    const pending = {
      windowSeconds: 86400 as const,
      updatedAt: policyWire.updated_at,
      revision: 4,
      backfillPending: true,
    };
    const completed = { ...pending, backfillPending: false };
    expect(mergeExpirationPolicy(completed, pending)).toEqual(completed);
    expect(mergeExpirationPolicy(pending, completed)).toEqual(completed);
    expect(mergeExpirationPolicy(completed, { ...completed, revision: 3 })).toEqual(completed);
    expect(mergeExpirationPolicy(completed, { ...completed, revision: 5 })).toEqual({
      ...completed,
      revision: 5,
    });
  });
});

describe('updateExpirationPolicy', () => {
  it.each([
    [{ mode: 'set', window_seconds: 3600, retroactive: 'apply' }, 3600],
    [{ mode: 'set', window_seconds: 2592000, retroactive: 'new_only' }, 2592000],
    [{ mode: 'clear', retroactive: 'clear_pending' }, null],
    [{ mode: 'clear', retroactive: 'leave_pending' }, null],
    [{ mode: 'resume', revision: 8 }, 86400],
  ])('sends the exact closed request body %j', async (request, windowSeconds) => {
    let body: unknown;
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, async ({ request: req }) => {
        body = await req.json();
        return HttpResponse.json({ ...policyWire, window_seconds: windowSeconds, revision: 8 });
      })
    );

    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request)
    ).resolves.toMatchObject({ kind: 'ok' });
    expect(body).toEqual(request);
  });

  it.each([
    { mode: 'set', window_seconds: 604800, retroactive: 'apply' },
    { mode: 'set', window_seconds: 86400, retroactive: 'new_only' },
  ])('accepts the remaining supported set window %j', async (request) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json(policyWire)
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request)
    ).resolves.toMatchObject({ kind: 'ok' });
  });

  it.each([
    { mode: 'set', window_seconds: 0, retroactive: 'apply' },
    { mode: 'set', window_seconds: 3600, retroactive: 'other' },
    { mode: 'clear', retroactive: 'other' },
    { mode: 'resume', revision: -1 },
    { mode: 'resume', revision: Number.MAX_SAFE_INTEGER + 1 },
  ])('rejects missing or invalid request values %j', async (request) => {
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, request as never)
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
  });

  it('rejects an extra request property before dispatch', async () => {
    let called = false;
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () => {
        called = true;
        return HttpResponse.json(policyWire);
      })
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, {
        mode: 'clear',
        retroactive: 'clear_pending',
        future: true,
      } as never)
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
    expect(called).toBe(false);
  });

  it.each([
    [400, { kind: 'rejected', reason: 'invalidRequest' }],
    [401, { kind: 'rejected', reason: 'sessionExpired' }],
    [403, { kind: 'rejected', reason: 'forbidden' }],
    [404, { kind: 'rejected', reason: 'notFound' }],
    [429, { kind: 'rejected', reason: 'rateLimited', retryAfterSeconds: 12 }],
    [500, { kind: 'rejected', reason: 'unavailable' }],
  ])('maps HTTP %i to its semantic result', async (status, expected) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, () =>
        HttpResponse.json(
          {},
          {
            status,
            headers: status === 429 ? { 'Retry-After': '12' } : undefined,
          }
        )
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'dm', id: 'conv-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual(expected);
  });

  it('maps a valid 409 policy to conflict', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, () =>
        HttpResponse.json({ ...policyWire, revision: 5 }, { status: 409 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'dm', id: 'conv-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({
      kind: 'conflict',
      policy: {
        windowSeconds: 86400,
        updatedAt: policyWire.updated_at,
        revision: 5,
        backfillPending: false,
      },
    });
  });

  it('maps 413 to invalidRequest', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({}, { status: 413 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'rejected', reason: 'invalidRequest' });
  });

  it('maps an unknown status to ambiguous even with a valid-looking body', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json(policyWire, { status: 418 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it.each([
    [{ ...policyWire, revision: -1 }],
    {
      revision: 4,
      window_seconds: 300,
      updated_at: policyWire.updated_at,
      backfill_pending: false,
    },
  ])('maps malformed 409 policy to conflict without a candidate', async (body) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/dm/conversations/conv-1/expiration`, () =>
        HttpResponse.json(body, { status: 409 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'dm', id: 'conv-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({ kind: 'conflict' });
  });

  it('keeps a valid 503 candidate partial and never treats it as current success', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({ ...policyWire, backfill_pending: true }, { status: 503 })
      )
    );
    await expect(
      updateExpirationPolicy({ kind: 'channel', id: 'channel-1' }, { mode: 'resume', revision: 4 })
    ).resolves.toEqual({
      kind: 'partial',
      candidate: {
        windowSeconds: 86400,
        updatedAt: policyWire.updated_at,
        revision: 4,
        backfillPending: true,
      },
    });
  });

  it.each([undefined, { ...policyWire, revision: -1 }])(
    'rejects malformed 503 candidates',
    async (body) => {
      server.use(
        http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
          body === undefined
            ? HttpResponse.json({}, { status: 503 })
            : HttpResponse.json(body, { status: 503 })
        )
      );
      await expect(
        updateExpirationPolicy(
          { kind: 'channel', id: 'channel-1' },
          { mode: 'resume', revision: 4 }
        )
      ).resolves.toEqual({ kind: 'partial' });
    }
  );

  it('maps an unreadable success body to ambiguous', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.text('<html>proxy</html>', { status: 200 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it('maps a readable but incomplete success body to ambiguous', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({ revision: 4 })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it('maps a network failure to ambiguous', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () => HttpResponse.error())
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'ambiguous' });
  });

  it.each(['-1', '1.5', '12x'])('does not expose invalid Retry-After %s', async (retryAfter) => {
    server.use(
      http.patch(`${API_BASE}/api/v1/channels/channel-1/expiration`, () =>
        HttpResponse.json({}, { status: 429, headers: { 'Retry-After': retryAfter } })
      )
    );
    await expect(
      updateExpirationPolicy(
        { kind: 'channel', id: 'channel-1' },
        { mode: 'clear', retroactive: 'leave_pending' }
      )
    ).resolves.toEqual({ kind: 'rejected', reason: 'rateLimited' });
  });
});
