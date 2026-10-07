import { beforeEach, describe, expect, it, vi } from 'vitest';
import { apiFetch } from '@/renderer/services/system/apiClient';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import {
  mintPasswordStepUpToken,
  passwordStepUpRefusalMessage,
  STEP_UP_PASSWORD_PATH,
} from '@/renderer/services/system/stepUpToken';

// The password step-up exchange (#3509): the only request that carries an
// own-rule password, and the one reader of the mint's refusals.

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));
const mockApiFetch = vi.mocked(apiFetch);

// Bound to a constant: detect-secrets flags keyword/literal adjacency.
const FIXTURE_PW = 'mint-fixture';

function response(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

describe('mintPasswordStepUpToken (#3509)', () => {
  beforeEach(() => mockApiFetch.mockReset());

  it('posts the password and purpose to the mint endpoint, once, and returns the token', async () => {
    mockApiFetch.mockResolvedValueOnce(response(200, { step_up_token: 'tok', expires_in: 60 }));

    await expect(mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear')).resolves.toEqual({
      kind: 'minted',
      token: 'tok',
    });
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
    expect(mockApiFetch).toHaveBeenCalledWith(STEP_UP_PASSWORD_PATH, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ current_password: FIXTURE_PW, purpose: 'dm.clear' }),
    });
    expect(STEP_UP_PASSWORD_PATH).toBe('/api/v1/auth/step-up/password');
  });

  it.each([
    ['a wrong password', response(403, { error: 'Invalid password' }), 'invalidPassword'],
    ['the shared lockout', response(423, { error_code: 'account_locked' }), 'tooManyAttempts'],
    ['a rate limit', response(429, { error: 'Rate limit exceeded' }), 'tooManyAttempts'],
    [
      'an MFA account',
      response(403, {
        error: 'MFA verification required',
        mfa_required: true,
        mfa_methods: ['totp'],
      }),
      'mfaRequired',
    ],
    [
      'an unknown purpose',
      response(400, { error: 'A valid verification purpose is required' }),
      'failed',
    ],
    ['a 200 without a token', response(200, {}), 'failed'],
  ] as const)('maps %s to a %s refusal', async (_label, res, reason) => {
    mockApiFetch.mockResolvedValueOnce(res);

    await expect(mintPasswordStepUpToken(FIXTURE_PW, 'messages.delete')).resolves.toMatchObject({
      kind: 'refused',
      reason,
    });
  });

  it('a transport failure is a refusal, never a throw', async () => {
    mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));

    await expect(mintPasswordStepUpToken(FIXTURE_PW, 'messages.delete')).resolves.toEqual({
      kind: 'refused',
      reason: 'failed',
    });
  });

  it('carries Retry-After into the lockout copy', async () => {
    mockApiFetch.mockResolvedValueOnce(response(429, {}, { 'Retry-After': '30' }));

    const result = await mintPasswordStepUpToken(FIXTURE_PW, 'messages.delete');

    expect(result).toEqual({ kind: 'refused', reason: 'tooManyAttempts', retryAfterSeconds: 30 });
    expect(
      passwordStepUpRefusalMessage(result as Extract<typeof result, { kind: 'refused' }>)
    ).toBe('Too many attempts. Try again in 30 seconds.');
  });

  it('every refusal has its own password-field copy', () => {
    const messages = (['invalidPassword', 'tooManyAttempts', 'mfaRequired', 'failed'] as const).map(
      (reason) => passwordStepUpRefusalMessage({ kind: 'refused', reason })
    );
    expect(new Set(messages).size).toBe(4);
    expect(messages[0]).toBe('That password is not correct.');
  });

  describe('a request that never left', () => {
    // Mutant: dropping the `unsent` mark (the caller then blames the password).
    it('marks a refusal apiFetch raised before dispatch as unsent', async () => {
      mockApiFetch.mockRejectedValueOnce(
        new DOMException('Request lifecycle changed before dispatch', 'AbortError')
      );

      await expect(mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear')).resolves.toEqual({
        kind: 'refused',
        reason: 'failed',
        unsent: true,
      });
      expect(mockApiFetch).toHaveBeenCalledOnce();
    });

    it('does not mark a transport failure unsent: it may have reached the server', async () => {
      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));

      const result = await mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear');

      expect(result).toEqual({ kind: 'refused', reason: 'failed' });
      expect(result).not.toHaveProperty('unsent');
    });

    it('does not mark an answered refusal unsent', async () => {
      mockApiFetch.mockResolvedValueOnce(response(500, {}));

      expect(await mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear')).not.toHaveProperty('unsent');
    });
  });

  it('admits the exchange against the caller context, and against none by default', async () => {
    const context = captureApiRequestContext();
    mockApiFetch
      .mockResolvedValueOnce(response(200, { step_up_token: 'tok', expires_in: 60 }))
      .mockResolvedValueOnce(response(200, { step_up_token: 'tok', expires_in: 60 }));

    await mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear', context);
    await mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear');

    expect((mockApiFetch.mock.calls[0]?.[2] as { context?: unknown }).context).toBe(context);
    expect(mockApiFetch.mock.calls[1]).toHaveLength(2);
  });

  it.each([404, 405])('names a %i as a server older than the endpoint', async (status) => {
    mockApiFetch.mockResolvedValueOnce(response(status, {}));

    const result = await mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear');

    expect(result).toMatchObject({ kind: 'refused', reason: 'unsupported' });
    expect(
      passwordStepUpRefusalMessage(result as Extract<typeof result, { kind: 'refused' }>)
    ).toBe("This server doesn't support this confirmation yet.");
  });

  it('names the methods an mfa_required refusal carries, dropping non-strings', async () => {
    mockApiFetch.mockResolvedValueOnce(
      response(403, { mfa_required: true, mfa_methods: ['webauthn', 3, 'totp'] })
    );

    await expect(mintPasswordStepUpToken(FIXTURE_PW, 'dm.clear')).resolves.toMatchObject({
      reason: 'mfaRequired',
      methods: ['webauthn', 'totp'],
    });
  });
});
