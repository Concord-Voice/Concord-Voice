import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { purgeMessages } from '@/renderer/services/purgeApi';

const CHANNEL = '11111111-1111-4111-8111-111111111111';
const CONVERSATION = '44444444-4444-4444-8444-444444444444';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
});

// Step-up fixture values. Bound to constants so the credential-named keys below
// are followed by identifiers rather than quoted literals — detect-secrets flags
// the keyword/literal adjacency, not the value, and an allowlist pragma here
// would suppress a detector we want live on this path.
const FIXTURE_PW = 'pw';
const FIXTURE_OTP = '123456';

describe('purgeMessages error matrix', () => {
  it('maps 200 to success with counts', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 12, hidden_count: 0 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'success', deletedCount: 12, hiddenCount: 0 });
  });

  it('maps an empty scope to success, not an error', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ deleted_count: 0, hidden_count: 0 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '1h' });
    expect(r).toEqual({ kind: 'success', deletedCount: 0, hiddenCount: 0 });
  });

  it('maps 429 to rateLimited with the Retry-After seconds', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json(
          { error: 'Rate limit exceeded' },
          { status: 429, headers: { 'Retry-After': '900' } }
        )
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'rateLimited', retryAfterSeconds: 900 });
  });

  it('maps 429 without Retry-After to rateLimited with no countdown', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'Rate limit exceeded' }, { status: 429 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'rateLimited', retryAfterSeconds: undefined });
  });

  it('maps 503 to unavailable — a distinct state from 429', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'service unavailable' }, { status: 503 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'unavailable' });
  });

  it('maps 404 to notFound (channel context only)', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'not found' }, { status: 404 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'notFound' });
  });

  it('maps a generic 403 to forbidden', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'forbidden' }, { status: 403 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'forbidden' });
  });

  it('maps 401 to sessionExpired — a refused request deleted nothing', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'unauthorized' }, { status: 401 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    // `partial` would tell a user whose session merely expired that some of
    // their history may already be gone.
    expect(r).toEqual({ kind: 'sessionExpired' });
  });

  it('tolerates a 200 whose body is not the expected JSON', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () => HttpResponse.text('<html>proxy</html>'))
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    // Rejecting here would escape a caller that has no catch; "Purged undefined
    // messages." is the other failure mode this coercion closes.
    expect(r).toEqual({ kind: 'success', deletedCount: 0, hiddenCount: 0 });
  });

  it('maps 500 to partial — messages may already be gone', async () => {
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'internal' }, { status: 500 })
      )
    );
    const r = await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });
    expect(r).toEqual({ kind: 'partial' });
  });

  it('maps a 403 password_required to passwordRequired', async () => {
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json({ error: 'password_required', password_required: true }, { status: 403 })
      )
    );
    const r = await purgeMessages({ context: 'dm', scopeId: CONVERSATION, range: '7d' });
    expect(r).toEqual({ kind: 'passwordRequired' });
  });

  it('maps a 403 mfa_required to mfaRequired with the offered methods', async () => {
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json(
          { error: 'mfa_required', mfa_required: true, methods: ['totp'] },
          { status: 403 }
        )
      )
    );
    const r = await purgeMessages({ context: 'dm', scopeId: CONVERSATION, range: '7d' });
    expect(r).toEqual({ kind: 'mfaRequired', methods: ['totp'] });
  });

  it('maps a 403 invalid password and invalid MFA code to their own states', async () => {
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json({ error: 'Invalid password' }, { status: 403 })
      )
    );
    expect(
      await purgeMessages({
        context: 'dm',
        scopeId: CONVERSATION,
        range: '7d',
        currentPassword: FIXTURE_PW,
      })
    ).toEqual({ kind: 'invalidPassword' });

    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json({ error: 'Invalid MFA code' }, { status: 403 })
      )
    );
    expect(
      await purgeMessages({
        context: 'dm',
        scopeId: CONVERSATION,
        range: '7d',
        mfaCode: '000000',
      })
    ).toEqual({ kind: 'invalidMfaCode' });
  });

  it('maps a DM 400 to stepUpImpossible but a channel 400 to unexpectedError', async () => {
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () =>
        HttpResponse.json({ error: 'no credentials' }, { status: 400 })
      ),
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json({ error: 'bad request' }, { status: 400 })
      )
    );
    expect(await purgeMessages({ context: 'dm', scopeId: CONVERSATION, range: '7d' })).toEqual({
      kind: 'stepUpImpossible',
    });
    // A 400 is refused before the handler deletes anything, so `partial` — which
    // claims some messages may already be gone — is reserved for 5xx.
    expect(await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' })).toEqual({
      kind: 'unexpectedError',
    });
  });
});

describe('purgeMessages request shape', () => {
  it('sends only the range when no credentials are supplied', async () => {
    let body: unknown = null;
    server.use(
      http.delete('*/api/v1/channels/:id/messages', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );
    await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '30d' });
    expect(body).toEqual({ range: '30d' });
  });

  it('sends both step-up factors together in one request', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ deleted_count: 3, hidden_count: 1 });
      })
    );
    await purgeMessages({
      context: 'dm',
      scopeId: CONVERSATION,
      range: '7d',
      currentPassword: FIXTURE_PW,
      mfaCode: FIXTURE_OTP,
    });
    expect(bodies).toHaveLength(1);
    expect(bodies[0]).toEqual({
      range: '7d',
      current_password: FIXTURE_PW,
      mfa_code: FIXTURE_OTP,
    });
  });

  it('routes group conversations to the DM endpoint', async () => {
    let hit = false;
    server.use(
      http.delete('*/api/v1/dm/conversations/:id/messages', () => {
        hit = true;
        return HttpResponse.json({ deleted_count: 0, hidden_count: 0 });
      })
    );
    await purgeMessages({ context: 'group', scopeId: CONVERSATION, range: '1d' });
    expect(hit).toBe(true);
  });

  it('routes the server context to the server endpoint', async () => {
    let hit = false;
    server.use(
      http.delete('*/api/v1/servers/:id/messages', () => {
        hit = true;
        return HttpResponse.json({ deleted_count: 0, hidden_count: 0 });
      })
    );
    await purgeMessages({ context: 'server', scopeId: 'server-1', range: 'all' });
    expect(hit).toBe(true);
  });
});
