import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { MINT_PATH } from '../../helpers/stepUpTokenWire';
import {
  isSoftLockChallengeResult,
  isStepUpPurgeResult,
  purgeMessages,
} from '@/renderer/services/messaging/purgeApi';

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

// The DM/group step-up proves its factor under ONE capture of the account and
// server (useStepUpFactor's `run`) and hands it to the purge as the second
// argument. The request is admitted against that capture, so a security-key
// token or a password is never sent as another account or to another server.
describe('purgeMessages request context', () => {
  const DM_ROUTE = '*/api/v1/dm/conversations/:id/messages';

  /** Counts the requests that reached a route. */
  function countHits(method: 'delete' | 'post', route: string): () => number {
    let hits = 0;
    server.use(
      http[method](route, () => {
        hits += 1;
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0, step_up_token: 'tok' });
      })
    );
    return () => hits;
  }

  const replaceAccount = () =>
    useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));

  // Mutant: the second argument dropped from apiFetchInContext, so the request
  // goes out as whoever is signed in by then.
  it.each(['dm', 'group'] as const)(
    'a %s purge refuses to dispatch once the captured account was replaced',
    async (context) => {
      const hits = countHits('delete', DM_ROUTE);
      const captured = captureApiRequestContext();
      replaceAccount();

      await expect(
        purgeMessages(
          { context, scopeId: CONVERSATION, range: '7d', mfaCode: FIXTURE_OTP },
          captured
        )
      ).rejects.toMatchObject({ name: 'AbortError' });
      expect(hits()).toBe(0);
    }
  );

  it('a DM purge under a still-current capture is sent once', async () => {
    const hits = countHits('delete', DM_ROUTE);
    const captured = captureApiRequestContext();

    const result = await purgeMessages(
      { context: 'dm', scopeId: CONVERSATION, range: '7d', mfaCode: FIXTURE_OTP },
      captured
    );
    expect(result).toEqual({ kind: 'success', deletedCount: 1, hiddenCount: 0 });
    expect(hits()).toBe(1);
  });

  it('without a capture the purge is its own operation, as before', async () => {
    const hits = countHits('delete', DM_ROUTE);
    replaceAccount();

    const result = await purgeMessages({ context: 'dm', scopeId: CONVERSATION, range: '7d' });
    expect(result.kind).toBe('success');
    expect(hits()).toBe(1);
  });

  // Mutant: the self-purge exchange ignoring the caller's capture and taking
  // its own, so a password is minted for an account that is no longer current.
  it('a self-purge password exchange is admitted against the supplied capture too', async () => {
    const mints = countHits('post', `*${MINT_PATH}`);
    const purges = countHits('delete', '*/api/v1/channels/:id/messages');
    const captured = captureApiRequestContext();
    replaceAccount();

    // The mint reports a fenced exchange as a refusal (`unsent`) rather than
    // throwing, and the soft-lock stage words it on the password field.
    const result = await purgeMessages(
      { context: 'channel', scopeId: CHANNEL, range: '7d', currentPassword: FIXTURE_PW },
      captured
    );
    expect(result).toEqual({
      kind: 'softLockChallenge',
      view: { view: 'password', error: expect.any(String) },
    });
    expect(mints()).toBe(0);
    expect(purges()).toBe(0);
  });
});

// #3455: the channel/server self-purge soft-lock. The gate runs before any purge
// batch, so every one of these outcomes means nothing was purged.
describe('purgeMessages soft-lock (#3455)', () => {
  const SOFT_LOCK_MFA = {
    error: 'Confirm it is you',
    delete_rate_limited: true,
    mfa_required: true,
    methods: ['totp', 'webauthn'],
  };
  const SOFT_LOCK_PASSWORD = {
    error: 'Password required',
    delete_rate_limited: true,
    password_required: true,
  };

  const ROUTES = [
    ['channel', CHANNEL, '*/api/v1/channels/:id/messages'],
    ['server', 'server-1', '*/api/v1/servers/:id/messages'],
  ] as const;

  describe.each(ROUTES)('on the %s route', (context, scopeId, path) => {
    it('a delete_rate_limited MFA 403 is a confirm challenge carrying the methods', async () => {
      server.use(http.delete(path, () => HttpResponse.json(SOFT_LOCK_MFA, { status: 403 })));
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'softLockChallenge',
        view: { view: 'confirm', methods: ['totp', 'webauthn'] },
      });
    });

    it('a delete_rate_limited password 403 is a password challenge', async () => {
      server.use(http.delete(path, () => HttpResponse.json(SOFT_LOCK_PASSWORD, { status: 403 })));
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'softLockChallenge',
        view: { view: 'password' },
      });
    });

    it('an invalid code after a confirm challenge stays on it, with the per-attempt error', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            { error: 'Invalid MFA code', delete_rate_limited: true },
            { status: 403 }
          )
        )
      );
      expect(
        await purgeMessages({
          context,
          scopeId,
          range: '7d',
          mfaCode: '000000',
          softLockPrior: { view: 'confirm', methods: ['totp'] },
        })
      ).toEqual({
        kind: 'softLockChallenge',
        view: {
          view: 'confirm',
          methods: ['totp'],
          error: "That didn't work. Try again with a new code.",
        },
      });
    });

    it('an invalid code with no challenge on screen is softLockFailed, not a challenge', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            { error: 'Invalid MFA code', delete_rate_limited: true },
            { status: 403 }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d', mfaCode: '000000' })).toEqual({
        kind: 'softLockFailed',
        message: "That didn't work. Try again with a new code.",
        retryAfterSeconds: undefined,
      });
    });

    // Rewritten for #3509: the password goes only to the mint endpoint, so an
    // invalid password is the MINT's refusal, and the purge route never runs.
    it('an invalid password is a password challenge with the per-field error', async () => {
      let purged = false;
      server.use(
        http.post('*/api/v1/auth/step-up/password', () =>
          HttpResponse.json({ error: 'Invalid password' }, { status: 403 })
        ),
        http.delete(path, () => {
          purged = true;
          return HttpResponse.json({ deleted_count: 0, hidden_count: 0 });
        })
      );
      expect(
        await purgeMessages({
          context,
          scopeId,
          range: '7d',
          currentPassword: FIXTURE_PW,
          softLockPrior: { view: 'password' },
        })
      ).toEqual({
        kind: 'softLockChallenge',
        view: { view: 'password', error: 'That password is not correct.' },
      });
      expect(purged, 'a refused exchange never reaches the purge route').toBe(false);
    });

    it('any other flagged 403 (enrolment required) is softLockFailed with the server text', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            {
              error: 'Set up an authenticator app or security key to do this.',
              delete_rate_limited: true,
              code: 'mfa_enrollment_required',
            },
            { status: 403, headers: { 'Retry-After': '60' } }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'softLockFailed',
        message: 'Set up an authenticator app or security key to do this.',
        retryAfterSeconds: 60,
      });
    });

    it('an UNflagged 403 is still plain forbidden', async () => {
      server.use(
        http.delete(path, () => HttpResponse.json({ error: 'Forbidden' }, { status: 403 }))
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'forbidden',
      });
    });

    it('re-sends the range with only the factor it is given', async () => {
      let body: unknown;
      server.use(
        http.delete(path, async ({ request }) => {
          body = await request.json();
          return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
        })
      );
      await purgeMessages({ context, scopeId, range: '7d', mfaCode: FIXTURE_OTP });
      expect(body).toEqual({ range: '7d', mfa_code: FIXTURE_OTP });
    });

    it('a 429 carrying the step-up budget flag is verificationLimited, with its countdown', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            { error: 'Too many verification attempts', step_up_budget_exhausted: true },
            { status: 429, headers: { 'Retry-After': '300' } }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'verificationLimited',
        retryAfterSeconds: 300,
      });
    });

    it('a 429 without the flag stays rateLimited', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            { error: 'Rate limit exceeded' },
            { status: 429, headers: { 'Retry-After': '900' } }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'rateLimited',
        retryAfterSeconds: 900,
      });
    });

    it('the budget string alone, without the flag, is not enough for verificationLimited', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json({ error: 'Too many verification attempts' }, { status: 429 })
        )
      );
      expect((await purgeMessages({ context, scopeId, range: '7d' })).kind).toBe('rateLimited');
    });

    it('a 429 whose body is not JSON stays rateLimited', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.text('<html>slow down</html>', {
            status: 429,
            headers: { 'Retry-After': '30' },
          })
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'rateLimited',
        retryAfterSeconds: 30,
      });
    });

    it('the flag is read strictly: a truthy non-boolean is not the flag', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json({ step_up_budget_exhausted: 'true' }, { status: 429 })
        )
      );
      expect((await purgeMessages({ context, scopeId, range: '7d' })).kind).toBe('rateLimited');
    });
  });

  describe.each([['dm'], ['group']] as const)('on the %s route', (context) => {
    it('a flagged password 403 keeps the DM step-up handling, not the soft-lock', async () => {
      server.use(
        http.delete('*/api/v1/dm/conversations/:id/messages', () =>
          HttpResponse.json(SOFT_LOCK_PASSWORD, { status: 403 })
        )
      );
      expect(await purgeMessages({ context, scopeId: CONVERSATION, range: '7d' })).toEqual({
        kind: 'passwordRequired',
      });
    });

    it('a flagged invalid-code 403 keeps the DM step-up handling, even with a prior challenge', async () => {
      server.use(
        http.delete('*/api/v1/dm/conversations/:id/messages', () =>
          HttpResponse.json(
            { error: 'Invalid MFA code', delete_rate_limited: true },
            { status: 403 }
          )
        )
      );
      expect(
        await purgeMessages({
          context,
          scopeId: CONVERSATION,
          range: '7d',
          mfaCode: '000000',
          softLockPrior: { view: 'confirm', methods: ['totp'] },
        })
      ).toEqual({ kind: 'invalidMfaCode' });
    });

    it('a flagged MFA 403 is never routed to the soft-lock stage', async () => {
      server.use(
        http.delete('*/api/v1/dm/conversations/:id/messages', () =>
          HttpResponse.json(SOFT_LOCK_MFA, { status: 403 })
        )
      );
      const r = await purgeMessages({ context, scopeId: CONVERSATION, range: '7d' });
      expect(r.kind).not.toBe('softLockChallenge');
      expect(r.kind).not.toBe('softLockFailed');
    });

    it('an unflagged mfa_required 403 is unchanged', async () => {
      server.use(
        http.delete('*/api/v1/dm/conversations/:id/messages', () =>
          HttpResponse.json(
            { error: 'mfa_required', mfa_required: true, methods: ['totp'] },
            { status: 403 }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId: CONVERSATION, range: '7d' })).toEqual({
        kind: 'mfaRequired',
        methods: ['totp'],
      });
    });
  });

  describe('result guards', () => {
    it('a soft-lock challenge is not a DM step-up result', () => {
      const challenge = {
        kind: 'softLockChallenge',
        view: { view: 'password' },
      } as const;
      expect(isSoftLockChallengeResult(challenge)).toBe(true);
      expect(isStepUpPurgeResult(challenge)).toBe(false);
    });

    it('softLockFailed and verificationLimited are terminal, not challenges', () => {
      expect(isSoftLockChallengeResult({ kind: 'softLockFailed' })).toBe(false);
      expect(isSoftLockChallengeResult({ kind: 'verificationLimited' })).toBe(false);
      expect(isStepUpPurgeResult({ kind: 'softLockFailed' })).toBe(false);
      expect(isStepUpPurgeResult({ kind: 'verificationLimited' })).toBe(false);
    });
  });
});
