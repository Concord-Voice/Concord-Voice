import { describe, it, expect, beforeAll, beforeEach, afterEach, afterAll } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../../mocks/server';
import { resetAllStores } from '../../../helpers/store-helpers';
import {
  isStepUpLocked,
  mapMfaStepUpResponse,
  stepUpBanner,
  stepUpPromptMethods,
  submitMfaStepUp,
  type MfaStepUpResult,
} from '@/renderer/components/Settings/mfaStepUp';

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

describe('shared presentation helpers', () => {
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

  it('stepUpPromptMethods: the server list wins, filtered to inline factors', () => {
    expect(
      stepUpPromptMethods({ kind: 'mfaRequired', methods: ['webauthn', 'sms'] }, ['totp'])
    ).toEqual(['webauthn']);
    // An empty or email-only server list falls back to the known methods.
    expect(
      stepUpPromptMethods({ kind: 'mfaRequired', methods: ['email'] }, ['totp', 'email'])
    ).toEqual(['totp']);
    expect(stepUpPromptMethods(null, ['totp', 'webauthn', 'email'])).toEqual(['totp', 'webauthn']);
  });
});
