import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  isStepUpLocked,
  mapMfaStepUpResponse,
  passwordOnlyBanner,
  stepUpBanner,
  submitMfaStepUp,
  toStepUpSubmitOutcome,
  type MfaStepUpResult,
} from '@/renderer/components/Settings/mfaStepUp';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';

const PATH = '/api/v1/mfa/email-sms/disable';

// Step-up fixture values. Bound to constants so the credential-named keys below
// are followed by identifiers rather than quoted literals — detect-secrets flags
// the keyword/literal adjacency, not the value (mirrors tests/unit/services/purgeApi.test.ts).
const FIXTURE_PW = 'pw';
const FIXTURE_OTP = '123456';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

beforeEach(() => {
  resetAllStores();
});

describe('mapMfaStepUpResponse — one case per kind', () => {
  // The wire body a settings route sends an account with no inline factor (E8).
  it('maps a 403 mfa_enrollment_required to enrollmentRequired', async () => {
    const res = new Response(JSON.stringify({ mfa_enrollment_required: true }), { status: 403 });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'enrollmentRequired' });
  });

  it('maps 2xx to accepted, carrying the parsed body', async () => {
    const res = new Response(JSON.stringify({ backup_email: 'a@b.com' }), { status: 200 });
    const result = await mapMfaStepUpResponse(res);
    expect(result).toEqual({ kind: 'accepted', data: { backup_email: 'a@b.com' } });
  });

  it('maps a 403 password_required to passwordRequired', async () => {
    const res = new Response(
      JSON.stringify({ error: 'Enter your password to continue.', password_required: true }),
      { status: 403 }
    );
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'passwordRequired' });
  });

  it('maps a 403 Invalid password (exact string) to invalidPassword', async () => {
    const res = new Response(JSON.stringify({ error: 'Invalid password' }), { status: 403 });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'invalidPassword' });
  });

  it('maps a 403 mfa_required to mfaRequired with the offered methods', async () => {
    const res = new Response(
      JSON.stringify({
        error: 'MFA verification required',
        mfa_required: true,
        methods: ['totp', 'webauthn'],
      }),
      { status: 403 }
    );
    expect(await mapMfaStepUpResponse(res)).toEqual({
      kind: 'mfaRequired',
      methods: ['totp', 'webauthn'],
    });
  });

  it('maps a 403 Invalid MFA code (exact string) to invalidMfaCode', async () => {
    const res = new Response(JSON.stringify({ error: 'Invalid MFA code' }), { status: 403 });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'invalidMfaCode' });
  });

  it('maps 429 to rateLimited', async () => {
    const res = new Response(JSON.stringify({ error: 'Too many verification attempts' }), {
      status: 429,
    });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'rateLimited' });
  });

  it('maps 401 to sessionExpired', async () => {
    const res = new Response(JSON.stringify({ error: 'Authentication required' }), {
      status: 401,
    });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'sessionExpired' });
  });

  // F9: the server's own text is carried on `failed`, so a 400 or a legacy
  // body ("Incorrect password" from a route not yet on the seam) reaches the
  // banner instead of "Something went wrong".
  it('maps any other status to failed, carrying the server text', async () => {
    const res = new Response(JSON.stringify({ error: 'internal' }), { status: 500 });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'failed', message: 'internal' });
  });

  it('maps a 403 with neither flag nor a recognized exact string to failed, carrying the text', async () => {
    const res = new Response(JSON.stringify({ error: 'Some other refusal' }), { status: 403 });
    expect(await mapMfaStepUpResponse(res)).toEqual({
      kind: 'failed',
      message: 'Some other refusal',
    });
  });

  it('maps a non-JSON failure to failed with no message', async () => {
    const res = new Response('<html>502</html>', { status: 502 });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'failed', message: undefined });
  });

  it('maps 503 to unavailable (the attempt budget could not be evaluated)', async () => {
    const res = new Response(
      JSON.stringify({
        error: 'Verification is temporarily unavailable. Try again in a few minutes.',
      }),
      { status: 503 }
    );
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'unavailable' });
  });

  it('maps a 409 inline_factor_required to inlineFactorRequired with the server text', async () => {
    const error =
      'Turn off email and text-message codes before removing your last authenticator app or security key.';
    const res = new Response(JSON.stringify({ error, inline_factor_required: true }), {
      status: 409,
    });
    expect(await mapMfaStepUpResponse(res)).toEqual({
      kind: 'inlineFactorRequired',
      message: error,
    });
  });
});

describe('mapMfaStepUpResponse — the two frozen strings are matched by EXACT equality only', () => {
  it('a near-miss on "Invalid password" does not match — falls to failed', async () => {
    const res = new Response(JSON.stringify({ error: 'Invalid password.' }), { status: 403 });
    expect((await mapMfaStepUpResponse(res)).kind).toBe('failed');
  });

  it('a near-miss on "Invalid MFA code" does not match — falls to failed', async () => {
    const res = new Response(JSON.stringify({ error: 'invalid mfa code' }), { status: 403 });
    expect((await mapMfaStepUpResponse(res)).kind).toBe('failed');
  });
});

describe('mapMfaStepUpResponse — methods filtering', () => {
  it('drops non-string entries from mfa_required.methods', async () => {
    const res = new Response(
      JSON.stringify({ mfa_required: true, methods: ['totp', 42, null, 'webauthn', {}] }),
      { status: 403 }
    );
    expect(await mapMfaStepUpResponse(res)).toEqual({
      kind: 'mfaRequired',
      methods: ['totp', 'webauthn'],
    });
  });

  it('defaults to an empty array when methods is missing or not an array', async () => {
    const res = new Response(JSON.stringify({ mfa_required: true }), { status: 403 });
    expect(await mapMfaStepUpResponse(res)).toEqual({ kind: 'mfaRequired', methods: [] });
  });
});

describe('submitMfaStepUp — request shape', () => {
  it('omits mfa_code from the body when it is empty', async () => {
    let body: unknown = null;
    server.use(
      http.post('*' + PATH, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ message: 'ok' });
      })
    );
    const result = await submitMfaStepUp(PATH, 'POST', {}, { password: FIXTURE_PW, mfaCode: '' });
    expect(result.kind).toBe('accepted');
    expect(body).toEqual({ password: FIXTURE_PW });
  });

  it('includes mfa_code in the body when it is non-empty', async () => {
    let body: unknown = null;
    server.use(
      http.post('*' + PATH, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ message: 'ok' });
      })
    );
    const result: MfaStepUpResult = await submitMfaStepUp(
      PATH,
      'POST',
      {},
      { password: FIXTURE_PW, mfaCode: FIXTURE_OTP }
    );
    expect(result.kind).toBe('accepted');
    expect(body).toEqual({ password: FIXTURE_PW, mfa_code: FIXTURE_OTP });
  });

  it('merges the extra body fields with the credentials', async () => {
    let body: unknown = null;
    server.use(
      http.put('*/api/v1/mfa/backup-email', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ backup_email: 'a@b.com' });
      })
    );
    await submitMfaStepUp(
      '/api/v1/mfa/backup-email',
      'PUT',
      { email: 'a@b.com' },
      { password: FIXTURE_PW, mfaCode: '' }
    );
    expect(body).toEqual({ email: 'a@b.com', password: FIXTURE_PW });
  });

  it('a rejected fetch maps to networkError', async () => {
    server.use(http.post('*' + PATH, () => HttpResponse.error()));
    const result = await submitMfaStepUp(PATH, 'POST', {}, { password: FIXTURE_PW, mfaCode: '' });
    expect(result).toEqual({ kind: 'networkError' });
  });

  it('codeField sends the code under `code` for the pre-seam TOTP disable route', async () => {
    let body: unknown = null;
    server.use(
      http.post('*/api/v1/mfa/totp/disable', async ({ request }) => {
        body = await request.json();
        return HttpResponse.json({ message: 'ok' });
      })
    );
    await submitMfaStepUp(
      '/api/v1/mfa/totp/disable',
      'POST',
      {},
      { password: FIXTURE_PW, mfaCode: FIXTURE_OTP },
      { codeField: 'code' }
    );
    expect(body).toEqual({ password: FIXTURE_PW, code: FIXTURE_OTP });
  });
});

describe("submitMfaStepUp — the run's capture (C82)", () => {
  it('a stale capture resolves aborted and sends nothing', async () => {
    let hits = 0;
    server.use(
      http.post('*' + PATH, () => {
        hits += 1;
        return HttpResponse.json({ message: 'ok' });
      })
    );
    const context = captureApiRequestContext();
    // The account changed after the capture was taken.
    useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
    const result = await submitMfaStepUp(
      PATH,
      'POST',
      {},
      { password: FIXTURE_PW, mfaCode: FIXTURE_OTP },
      { context }
    );
    expect(result).toEqual({ kind: 'aborted' });
    expect(hits).toBe(0);
  });

  it('a current capture sends the request and accepts', async () => {
    let hits = 0;
    server.use(
      http.post('*' + PATH, () => {
        hits += 1;
        return HttpResponse.json({ message: 'ok' });
      })
    );
    const context = captureApiRequestContext();
    const result = await submitMfaStepUp(
      PATH,
      'POST',
      {},
      { password: FIXTURE_PW, mfaCode: FIXTURE_OTP },
      { context }
    );
    expect(result.kind).toBe('accepted');
    expect(hits).toBe(1);
  });

  it('codeField is honoured together with a capture, and mfa_code is then absent', async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.post('*/api/v1/mfa/totp/disable', async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ message: 'ok' });
      })
    );
    await submitMfaStepUp(
      '/api/v1/mfa/totp/disable',
      'POST',
      {},
      { password: FIXTURE_PW, mfaCode: FIXTURE_OTP },
      { codeField: 'code', context: captureApiRequestContext() }
    );
    expect(body.code).toBe(FIXTURE_OTP);
    expect('mfa_code' in body).toBe(false);
  });

  it('a code of undefined sends neither field', async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.post('*' + PATH, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ message: 'ok' });
      })
    );
    await submitMfaStepUp(PATH, 'POST', {}, { password: FIXTURE_PW, mfaCode: undefined });
    expect(body).toEqual({ password: FIXTURE_PW });
  });
});

describe('toStepUpSubmitOutcome', () => {
  it('accepted is a success', () => {
    expect(toStepUpSubmitOutcome({ kind: 'accepted', data: null })).toEqual({ kind: 'success' });
  });

  it.each([
    { kind: 'passwordRequired' },
    { kind: 'invalidPassword' },
    { kind: 'invalidMfaCode' },
    { kind: 'mfaRequired', methods: ['totp'] },
    { kind: 'enrollmentRequired' },
    // Mutant: a 401 as `answered`, which left the stage live with the password
    // and an active primary under a dead session (picker PR 3 review).
    { kind: 'sessionExpired' },
  ] as const)('$kind reaches the hook as itself', (result) => {
    expect(toStepUpSubmitOutcome(result)).toEqual({ kind: 'refusal', refusal: result });
  });

  // C30: the budget is charged before any factor is read, so a 429 proves the
  // code unspent. As `answered` it would be recorded as a TOTP acceptance.
  it('rateLimited is a refusal, not an answer', () => {
    expect(toStepUpSubmitOutcome({ kind: 'rateLimited' })).toEqual({
      kind: 'refusal',
      refusal: { kind: 'rateLimited' },
    });
  });

  it.each([
    { kind: 'deleteRateLimited', retryAfterSeconds: 3 },
    { kind: 'inlineFactorRequired', message: 'x' },
    { kind: 'unavailable' },
    { kind: 'failed' },
  ] as unknown as MfaStepUpResult[])(
    '$kind may have spent the code, so it is answered',
    (result) => {
      expect(toStepUpSubmitOutcome(result)).toEqual({ kind: 'answered' });
    }
  );

  it('a lost response is a transport failure and a fenced request is aborted', () => {
    expect(toStepUpSubmitOutcome({ kind: 'networkError' })).toEqual({ kind: 'transport' });
    expect(toStepUpSubmitOutcome({ kind: 'aborted' })).toEqual({ kind: 'aborted' });
  });
});

describe('shared presentation helpers', () => {
  it("stepUpBanner: enrolment and a dead session are the stage's to say, not a banner", () => {
    expect(stepUpBanner({ kind: 'enrollmentRequired' })).toBeNull();
    expect(stepUpBanner({ kind: 'sessionExpired' })).toBeNull();
  });

  // Mutant: the stage-less key revoke falling back to `stepUpBanner`, which
  // says nothing about a dead session now that the stage words it.
  it('passwordOnlyBanner: the dead session in words, everything else as stepUpBanner', () => {
    expect(passwordOnlyBanner({ kind: 'sessionExpired' })).toBe(
      'Your session has expired. Sign in again to continue.'
    );
    expect(passwordOnlyBanner({ kind: 'rateLimited' })).toBe(
      'Too many attempts. Try again in a few minutes.'
    );
    expect(passwordOnlyBanner({ kind: 'invalidPassword' })).toBeNull();
    expect(passwordOnlyBanner(null)).toBeNull();
  });

  it('stepUpBanner: one line per banner kind, null for field kinds', () => {
    expect(stepUpBanner({ kind: 'unavailable' })).toBe(
      'Verification is temporarily unavailable. Try again in a few minutes.'
    );
    expect(stepUpBanner({ kind: 'inlineFactorRequired', message: 'server copy' })).toBe(
      'server copy'
    );
    expect(stepUpBanner({ kind: 'failed', message: 'Incorrect password' })).toBe(
      'Incorrect password'
    );
    expect(stepUpBanner({ kind: 'failed' })).toBe('Something went wrong. Try again.');
    expect(stepUpBanner({ kind: 'invalidPassword' })).toBeNull();
    expect(stepUpBanner({ kind: 'mfaRequired', methods: [] })).toBeNull();
    expect(stepUpBanner({ kind: 'aborted' })).toBeNull();
    expect(stepUpBanner(null)).toBeNull();
  });

  it('isStepUpLocked: only a spent budget or a dead session locks', () => {
    expect(isStepUpLocked({ kind: 'rateLimited' })).toBe(true);
    expect(isStepUpLocked({ kind: 'sessionExpired' })).toBe(true);
    expect(isStepUpLocked({ kind: 'unavailable' })).toBe(false);
    expect(isStepUpLocked({ kind: 'networkError' })).toBe(false);
    expect(isStepUpLocked(null)).toBe(false);
  });
});
