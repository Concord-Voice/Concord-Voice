import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll, vi } from 'vitest';
import { delay, http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { FIXTURE_PW as MINT_PW, MINT_PATH, MINTED_TOKEN } from '../../helpers/stepUpTokenWire';
import { passwordStepUpRefusalMessage } from '@/renderer/services/system/stepUpToken';
import {
  includePinnedFor,
  isSoftLockChallengeResult,
  isStepUpPurgeResult,
  PinClaimReplayRefused,
  pinClaimDispatchGuard,
  pinClaimStillHonoured,
  pinModeFor,
  purgeMessages,
} from '@/renderer/services/messaging/purgeApi';
import { clientConfigService } from '@/renderer/services/system/clientConfigService';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';
import { signInRefreshableSession } from '../../helpers/refreshReplay';

// The copy of a refused password exchange is wrapped so one case can reword it:
// the challenge's `refusal` must be read from the wire and the mint's reason,
// never from the text a view happens to carry.
vi.mock('@/renderer/services/system/stepUpToken', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/system/stepUpToken')>();
  return {
    ...actual,
    passwordStepUpRefusalMessage: vi.fn(actual.passwordStepUpRefusalMessage),
  };
});

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
    expect(body).toEqual({ range: '30d', include_pinned: false });
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
      include_pinned: false,
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

    // The mint reports a fenced exchange as a refusal marked `unsent` rather
    // than throwing, and nothing was checked, so the purge is NOT SENT (D7):
    // never a password error, which would blame a password nobody tested.
    const result = await purgeMessages(
      { context: 'channel', scopeId: CHANNEL, range: '7d', currentPassword: FIXTURE_PW },
      captured
    );
    expect(result).toEqual({ kind: 'notSent' });
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
        // The factor hook's reading: the soft-lock's own kind is its mfaRequired.
        refusal: { kind: 'mfaRequired', methods: ['totp', 'webauthn'] },
      });
    });

    it('a delete_rate_limited password 403 is a password challenge', async () => {
      server.use(http.delete(path, () => HttpResponse.json(SOFT_LOCK_PASSWORD, { status: 403 })));
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'softLockChallenge',
        view: { view: 'password' },
        refusal: { kind: 'passwordRequired' },
      });
    });

    it('an invalid code after a confirm challenge stays on it; the hook words the attempt', async () => {
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
        view: { view: 'confirm', methods: ['totp'] },
        refusal: { kind: 'invalidMfaCode' },
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
        refusal: { kind: 'invalidPassword' },
      });
      expect(purged, 'a refused exchange never reaches the purge route').toBe(false);
    });

    // E8 / D5: the enrolment body is `mfa_enrollment_required: true`, and the
    // soft-lock adds `delete_rate_limited` and `Retry-After` to it. It must be
    // the terminal challenge, never `softLockFailed` (a Retry and a countdown).
    it.each([
      ['with a Retry-After', { 'Retry-After': '60' }],
      ['without one', undefined],
    ])('a flagged enrolment 403 %s is the terminal enroll challenge', async (_label, headers) => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            {
              error: 'Set up an authenticator app or security key to do this.',
              delete_rate_limited: true,
              mfa_enrollment_required: true,
            },
            { status: 403, headers }
          )
        )
      );
      const result = await purgeMessages({ context, scopeId, range: '7d' });
      expect(result).toEqual({
        kind: 'softLockChallenge',
        view: { view: 'enroll' },
        refusal: { kind: 'enrollmentRequired' },
      });
      expect(result.kind).not.toBe('softLockFailed');
    });

    // #3456 C-6: the D1 gate's enrolment refusal is the plain seam body. It was
    // `forbidden`, a dead end with no way forward; it is a challenge now.
    it('an enrolment 403 with no delete_rate_limited is the D1 gate’s dangerousChallenge', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            { error: 'Set up an app', mfa_enrollment_required: true },
            { status: 403 }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'dangerousChallenge',
        refusal: { kind: 'enrollmentRequired' },
      });
    });

    it('a flagged 403 the credential fields cannot answer is softLockFailed with the server text', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            { error: 'Something else about the lock', delete_rate_limited: true },
            { status: 403, headers: { 'Retry-After': '60' } }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toEqual({
        kind: 'softLockFailed',
        message: 'Something else about the lock',
        retryAfterSeconds: 60,
      });
    });

    // The challenge's `refusal` is read from the wire, so rewording the server's
    // `error` text changes nothing about what the hook is told.
    it('the confirm challenge’s refusal does not depend on the server’s error text', async () => {
      server.use(
        http.delete(path, () =>
          HttpResponse.json(
            {
              error: 'Totally reworded',
              delete_rate_limited: true,
              mfa_required: true,
              methods: ['totp'],
            },
            { status: 403 }
          )
        )
      );
      expect(await purgeMessages({ context, scopeId, range: '7d' })).toMatchObject({
        kind: 'softLockChallenge',
        view: { view: 'confirm', methods: ['totp'] },
        refusal: { kind: 'mfaRequired', methods: ['totp'] },
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
      expect(body).toEqual({ range: '7d', mfa_code: FIXTURE_OTP, include_pinned: false });
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

    describe('the password exchange (#3509, D7)', () => {
      const replaceAccount = () =>
        useAuthStore.setState((st) => ({ authGeneration: st.authGeneration + 1 }));

      function standMintAndPurge(mint: () => Response) {
        const wire = {
          mint: [] as Record<string, unknown>[],
          purge: [] as Record<string, unknown>[],
        };
        server.use(
          http.post(`*${MINT_PATH}`, async ({ request }) => {
            wire.mint.push((await request.json()) as Record<string, unknown>);
            return mint();
          }),
          http.delete(path, async ({ request }) => {
            wire.purge.push((await request.json()) as Record<string, unknown>);
            return HttpResponse.json({ deleted_count: 3, hidden_count: 0 });
          })
        );
        return wire;
      }

      const purgeWithPassword = (extra: Partial<Parameters<typeof purgeMessages>[0]> = {}) =>
        purgeMessages({ context, scopeId, range: '7d', currentPassword: MINT_PW, ...extra });

      it('the password goes to the mint with this route’s purpose; the purge carries only the token', async () => {
        const wire = standMintAndPurge(() => HttpResponse.json({ step_up_token: MINTED_TOKEN }));

        expect(await purgeWithPassword()).toEqual({
          kind: 'success',
          deletedCount: 3,
          hiddenCount: 0,
        });

        expect(wire.mint).toEqual([
          {
            current_password: MINT_PW,
            purpose: context === 'channel' ? 'messages.channel_purge' : 'messages.server_purge',
          },
        ]);
        expect(wire.purge).toEqual([
          { range: '7d', include_pinned: false, step_up_token: MINTED_TOKEN },
        ]);
        expect(wire.purge[0]).not.toHaveProperty('current_password');
      });

      // A security-key assertion token travels as the code field of the purge.
      it('a WebAuthn assertion token reaches the purge body as mfa_code, with no exchange', async () => {
        const wire = standMintAndPurge(() => HttpResponse.json({ step_up_token: MINTED_TOKEN }));
        const assertion = 'webauthn-assertion-token-0123456789';

        await purgeMessages({ context, scopeId, range: '7d', mfaCode: assertion });

        expect(wire.purge).toEqual([{ range: '7d', include_pinned: false, mfa_code: assertion }]);
        expect(wire.mint).toEqual([]);
      });

      it('a backup code is just another mfa_code on this route', async () => {
        const wire = standMintAndPurge(() => HttpResponse.json({ step_up_token: MINTED_TOKEN }));

        await purgeMessages({ context, scopeId, range: '7d', mfaCode: 'abcd-efgh-ijkl' });

        expect(wire.purge).toEqual([
          { range: '7d', include_pinned: false, mfa_code: 'abcd-efgh-ijkl' },
        ]);
      });

      // D7: the mint never left (the account changed after the capture), so
      // nothing was checked. A password error would blame a password nobody tested.
      it('an exchange that never left is notSent, not a password error, and the purge is not sent', async () => {
        const wire = standMintAndPurge(() => HttpResponse.json({ step_up_token: MINTED_TOKEN }));
        const captured = captureApiRequestContext();
        replaceAccount();

        const result = await purgeMessages(
          { context, scopeId, range: '7d', currentPassword: MINT_PW },
          captured
        );

        expect(result).toEqual({ kind: 'notSent' });
        expect(wire.mint).toEqual([]);
        expect(wire.purge).toEqual([]);
      });

      it('an exchange that left and failed in transport is the password challenge, not notSent', async () => {
        const wire = standMintAndPurge(() => HttpResponse.error());

        const result = await purgeWithPassword();

        expect(result).toEqual({
          kind: 'softLockChallenge',
          view: { view: 'password', error: "We couldn't check your password. Try again." },
          refusal: null,
        });
        expect(wire.purge).toEqual([]);
      });

      it('a mint naming its methods moves the stage to the code prompt, seeded with them', async () => {
        const wire = standMintAndPurge(() =>
          HttpResponse.json(
            { error: 'MFA required', mfa_required: true, mfa_methods: ['totp'] },
            { status: 403 }
          )
        );

        expect(await purgeWithPassword()).toEqual({
          kind: 'softLockChallenge',
          view: { view: 'confirm', methods: ['totp'] },
          refusal: { kind: 'mfaRequired', methods: ['totp'] },
        });
        expect(wire.purge).toEqual([]);
      });

      it.each([
        ['names no methods', { error: 'MFA required', mfa_required: true }],
        ['names an empty list', { error: 'MFA required', mfa_required: true, mfa_methods: [] }],
      ])(
        'an mfaRequired mint that %s is no verdict on the password: refusal null',
        async (_l, body) => {
          standMintAndPurge(() => HttpResponse.json(body, { status: 403 }));

          const result = await purgeWithPassword();

          expect(result).toMatchObject({
            kind: 'softLockChallenge',
            view: { view: 'password' },
            refusal: null,
          });
        }
      );

      it.each([
        [
          'a lockout',
          () => HttpResponse.json({}, { status: 423, headers: { 'Retry-After': '30' } }),
        ],
        ['a rate limit', () => HttpResponse.json({}, { status: 429 })],
        ['a server without the endpoint', () => HttpResponse.json({}, { status: 404 })],
        ['a server error', () => HttpResponse.json({}, { status: 500 })],
      ])('%s at the mint cannot be answered by typing again: refusal null', async (_l, mint) => {
        const wire = standMintAndPurge(mint);

        const result = await purgeWithPassword();

        expect(result).toMatchObject({
          kind: 'softLockChallenge',
          view: { view: 'password', error: expect.any(String) },
          refusal: null,
        });
        expect(wire.purge).toEqual([]);
      });

      it('a wrong password at the mint is the hook’s invalidPassword', async () => {
        standMintAndPurge(() => HttpResponse.json({ error: 'Invalid password' }, { status: 403 }));

        expect(await purgeWithPassword()).toMatchObject({
          view: { view: 'password', error: 'That password is not correct.' },
          refusal: { kind: 'invalidPassword' },
        });
      });

      // Mutant: `refusal` derived by matching the view's copy.
      it('rewording the mint’s copy changes nothing about the refusal', async () => {
        const spy = vi.mocked(passwordStepUpRefusalMessage);
        const real = spy.getMockImplementation();
        spy.mockImplementation(() => 'Completely different wording');
        try {
          standMintAndPurge(() =>
            HttpResponse.json({ error: 'Invalid password' }, { status: 403 })
          );
          expect(await purgeWithPassword()).toEqual({
            kind: 'softLockChallenge',
            view: { view: 'password', error: 'Completely different wording' },
            refusal: { kind: 'invalidPassword' },
          });

          server.resetHandlers();
          standMintAndPurge(() => HttpResponse.json({}, { status: 429 }));
          expect(await purgeWithPassword()).toEqual({
            kind: 'softLockChallenge',
            view: { view: 'password', error: 'Completely different wording' },
            refusal: null,
          });
        } finally {
          if (real) spy.mockImplementation(real);
        }
      });
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

// #3458: the wire value is derived from one PinMode, and always sent.
describe('include_pinned', () => {
  it('only an explicit include sends true', () => {
    expect(includePinnedFor('include')).toBe(true);
    expect(includePinnedFor('keep')).toBe(false);
    expect(includePinnedFor('unsupported')).toBe(false);
  });

  it('an unsupported server is unsupported whatever the checkbox says', () => {
    expect(pinModeFor(false, true)).toBe('unsupported');
    expect(pinModeFor(false, false)).toBe('unsupported');
    expect(pinModeFor(true, true)).toBe('include');
    expect(pinModeFor(true, false)).toBe('keep');
  });

  // include_pinned is derived from the mode alone, so it cannot disagree with
  // the claim the dialog showed. Only `unsupported` and the default are tested
  // here: `keep` and `include` make a pin claim and need a fresh capability
  // answer, covered below.
  it.each([
    { pinMode: 'unsupported' as const, sent: false },
    { pinMode: undefined, sent: false },
  ])('the body carries include_pinned=$sent for mode $pinMode', async ({ pinMode, sent }) => {
    const bodies: unknown[] = [];
    server.use(
      http.delete('*/api/v1/channels/:id/messages', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );
    await purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d', pinMode });
    expect(bodies).toEqual([{ range: '7d', include_pinned: sent }]);
  });
});

// #3552 review: a send that makes a pin claim goes only on a fresh capability
// answer that still advertises purgeKeepsPinned.
describe('pinClaimStillHonoured', () => {
  const caps = (keepsPinned?: boolean) => ({
    auth: { oauthProviders: [] },
    features: keepsPinned === undefined ? {} : { purgeKeepsPinned: keepsPinned },
  });
  const refreshAnswers = (next: () => ReturnType<typeof caps> | null) =>
    vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockImplementation(async () => {
      useClientConfigStore.setState({ serverCapabilities: next() });
    });

  beforeEach(() => useClientConfigStore.setState({ serverCapabilities: caps(true) }));

  it('makes no request for a mode that claims nothing', async () => {
    const refresh = refreshAnswers(() => caps(false));
    expect(await pinClaimStillHonoured('unsupported')).toBe(true);
    expect(refresh).not.toHaveBeenCalled();
  });

  it('goes on a fresh answer that still advertises the capability', async () => {
    refreshAnswers(() => caps(true));
    expect(await pinClaimStillHonoured('keep')).toBe(true);
    expect(await pinClaimStillHonoured('include')).toBe(true);
  });

  it('stops on an answer without it: a rolled-back server', async () => {
    refreshAnswers(() => caps());
    expect(await pinClaimStillHonoured('keep')).toBe(false);
  });

  it('stops on a failed refresh', async () => {
    refreshAnswers(() => null);
    expect(await pinClaimStillHonoured('keep')).toBe(false);
  });

  it('stops when no new answer arrived, so the cached value never decides', async () => {
    // Counted here: the suite keeps mock calls between tests (clearMocks: false).
    let refreshes = 0;
    vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockImplementation(async () => {
      refreshes += 1;
    });
    expect(await pinClaimStillHonoured('keep')).toBe(false);
    // One retry for a superseded refresh, then it gives up.
    expect(refreshes).toBe(2);
  });

  // Gitar on #3552, through the real refresh: a refresh another caller starts
  // aborts the recheck's, which returns without an answer. Mutant: no second
  // attempt, so a server that still keeps pins is refused.
  it('asks once more when another refresh superseded its own', async () => {
    vi.restoreAllMocks();
    server.use(
      http.get('*/api/v1/server/capabilities', async () => {
        await delay(30);
        return HttpResponse.json(caps(true));
      })
    );

    const recheck = pinClaimStillHonoured('keep');
    // The poll, say, starting while the recheck's request is in flight.
    const poll = clientConfigService.refreshServerCapabilities();

    expect(await recheck).toBe(true);
    await poll;
  });

  // Codex P1 on #3552: the step-up mint is a request of its own, so the recheck
  // runs after it, immediately before the delete. A recheck taken before the
  // mint would pass on the pre-rollback answer and send the delete.
  it.each([
    ['the server rolled back during the mint', false, 0],
    ['control: the capability is still advertised', true, 1],
  ])('a self-purge rechecks after the step-up mint: %s', async (_name, keepsAfterMint, deletes) => {
    let minted = false;
    let purged = 0;
    vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockImplementation(async () => {
      useClientConfigStore.setState({ serverCapabilities: caps(minted ? keepsAfterMint : true) });
    });
    server.use(
      http.post(`*${MINT_PATH}`, () => {
        minted = true;
        return HttpResponse.json({ step_up_token: MINTED_TOKEN, expires_in: 60 });
      }),
      http.delete('*/api/v1/channels/:id/messages', () => {
        purged += 1;
        return HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );

    const result = await purgeMessages({
      context: 'channel',
      scopeId: CHANNEL,
      range: '7d',
      currentPassword: FIXTURE_PW,
      pinMode: 'keep',
    });

    expect(minted).toBe(true);
    expect(purged).toBe(deletes);
    expect(result.kind).toBe(deletes === 1 ? 'success' : 'unavailable');
  });
});

// Codex P1s on #3552. The pin recheck is a request of its own, so the purge it
// gates must stay bound to the account that confirmed it, and apiFetch's own
// recovery must not resend it without a new recheck.
describe('pin-claim purge dispatch (#3552 review)', () => {
  const DM_ROUTE = '*/api/v1/dm/conversations/:id/messages';
  const keepsPinned = () => ({
    auth: { oauthProviders: [] },
    features: { purgeKeepsPinned: true },
  });
  const recheckAnswers = (during?: () => void) =>
    vi.spyOn(clientConfigService, 'refreshServerCapabilities').mockImplementation(async () => {
      during?.();
      useClientConfigStore.setState({ serverCapabilities: keepsPinned() });
    });
  /** Counts the deletes; the first `refusals` are answered 401, the rest succeed. */
  const deletes = (refusals = 0) => {
    let hits = 0;
    server.use(
      http.delete(DM_ROUTE, () => {
        hits += 1;
        return hits <= refusals
          ? new HttpResponse(null, { status: 401 })
          : HttpResponse.json({ deleted_count: 1, hidden_count: 0 });
      })
    );
    return () => hits;
  };

  beforeEach(() => useClientConfigStore.setState({ serverCapabilities: keepsPinned() }));

  // Mutant: the capture taken after the recheck, or none, so the delete goes
  // out as the account that signed in meanwhile.
  it.each([
    ['the account is replaced during the recheck', true, 0],
    ['control: the account stays', false, 1],
  ])('a purge without a capture: %s', async (_name, replace, sent) => {
    recheckAnswers(
      replace
        ? () => useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }))
        : undefined
    );
    const hits = deletes();

    const purge = purgeMessages({
      context: 'dm',
      scopeId: CONVERSATION,
      range: '7d',
      pinMode: 'keep',
    });
    if (replace) await expect(purge).rejects.toMatchObject({ name: 'AbortError' });
    else await expect(purge).resolves.toMatchObject({ kind: 'success' });
    expect(hits()).toBe(sent);
  });

  // Mutant: no dispatch guard, so the refresh resends the delete unchecked. The
  // `unsupported` row is the control: no claim, so the ordinary resend happens.
  it.each([
    ['keep', 1, { kind: 'unavailable' }],
    ['include', 1, { kind: 'unavailable' }],
    ['unsupported', 2, { kind: 'success', deletedCount: 1, hiddenCount: 0 }],
  ] as const)('a %s purge answered 401 is sent %i time(s)', async (pinMode, sent, result) => {
    signInRefreshableSession();
    recheckAnswers();
    const hits = deletes(1);

    await expect(
      purgeMessages({ context: 'dm', scopeId: CONVERSATION, range: '7d', pinMode })
    ).resolves.toEqual(result);
    expect(hits()).toBe(sent);
  });

  // Codex P1 on #3552: a capability poll that lands while apiFetch awaits the
  // attestation IPC, after the recheck passed. Mutant: the guard checks only
  // whether a dispatch already happened, so the delete goes out.
  it.each([
    ['the option is withdrawn during the attestation await', true, 0, { kind: 'unavailable' }],
    ['control: the option stays', false, 1, { kind: 'success', deletedCount: 1, hiddenCount: 0 }],
  ] as const)('a keep purge: %s', async (_name, withdraw, sent, result) => {
    recheckAnswers();
    const hits = deletes();
    const g = globalThis as { electron?: Record<string, unknown> };
    const before = g.electron;
    g.electron = {
      ...before,
      attestation: {
        getToken: async () => {
          if (withdraw) {
            useClientConfigStore.setState({
              serverCapabilities: { auth: { oauthProviders: [] }, features: {} },
            });
          }
          return null;
        },
        clearToken: async () => undefined,
      },
    };
    try {
      await expect(
        purgeMessages({ context: 'dm', scopeId: CONVERSATION, range: '7d', pinMode: 'keep' })
      ).resolves.toEqual(result);
    } finally {
      g.electron = before;
    }
    expect(hits()).toBe(sent);
  });
});

describe('pinClaimDispatchGuard', () => {
  const advertise = (keepsPinned: boolean | undefined) =>
    useClientConfigStore.setState({
      serverCapabilities: {
        auth: { oauthProviders: [] },
        features: keepsPinned === undefined ? {} : { purgeKeepsPinned: keepsPinned },
      },
    });

  beforeEach(() => advertise(true));

  it('guards nothing for a mode that claims nothing', () => {
    expect(pinClaimDispatchGuard('unsupported')).toBeUndefined();
  });

  it.each(['keep', 'include'] as const)(
    'lets one %s dispatch through and refuses the next',
    (mode) => {
      const guard = pinClaimDispatchGuard(mode);
      expect(() => guard?.()).not.toThrow();
      expect(() => guard?.()).toThrow(PinClaimReplayRefused);
    }
  );

  // Mutant: the advertised check removed, or `!== true` loosened to `=== false`
  // (an absent flag must refuse too).
  it.each([
    ['keep', false],
    ['keep', undefined],
    ['include', false],
    ['include', undefined],
  ] as const)('refuses a first %s dispatch when the option reads %s', (mode, keepsPinned) => {
    advertise(keepsPinned);
    expect(() => pinClaimDispatchGuard(mode)?.()).toThrow(PinClaimReplayRefused);
  });

  it('refuses a first dispatch with no capabilities cached', () => {
    useClientConfigStore.setState({ serverCapabilities: null });
    expect(() => pinClaimDispatchGuard('keep')?.()).toThrow(PinClaimReplayRefused);
  });
});

// mapTooManyRequests parses Retry-After itself (no-rot, T10): the guard the other
// two parsers carry applies here too.
describe('purgeMessages 429 Retry-After', () => {
  const stand = (headers: Record<string, string>, body: Record<string, unknown> = {}) =>
    server.use(
      http.delete('*/api/v1/channels/:id/messages', () =>
        HttpResponse.json(body, { status: 429, headers })
      )
    );
  const purge = () => purgeMessages({ context: 'channel', scopeId: CHANNEL, range: '7d' });

  it.each([
    ['a negative value', '-5'],
    ['an HTTP-date', 'Wed, 21 Oct 2026 07:28:00 GMT'],
    ['an empty header', ''],
    ['garbage', 'soon'],
  ])('%s is no countdown on rateLimited', async (_label, header) => {
    stand({ 'Retry-After': header });
    expect(await purge()).toEqual({ kind: 'rateLimited', retryAfterSeconds: undefined });
  });

  it('a negative value is no countdown on verificationLimited either', async () => {
    stand({ 'Retry-After': '-1' }, { step_up_budget_exhausted: true });
    expect(await purge()).toEqual({ kind: 'verificationLimited', retryAfterSeconds: undefined });
  });

  it('zero is a legal countdown', async () => {
    stand({ 'Retry-After': '0' });
    expect(await purge()).toEqual({ kind: 'rateLimited', retryAfterSeconds: 0 });
  });
});
