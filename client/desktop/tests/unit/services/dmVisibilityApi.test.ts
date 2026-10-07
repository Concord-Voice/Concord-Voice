import { beforeEach, describe, expect, it, vi } from 'vitest';

import { apiFetch } from '@/renderer/services/system/apiClient';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';
import { clearDMHistory, hideDMThread } from '@/renderer/services/messaging/dmVisibilityApi';

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));

const mockApiFetch = vi.mocked(apiFetch);
const CONVERSATION_ID = '11111111-1111-4111-8111-111111111111';

const response = (status: number, body: unknown, headers: Record<string, string> = {}): Response =>
  ({
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(headers),
    json: async () => body,
  }) as Response;

const contextOf = (call: number) =>
  (mockApiFetch.mock.calls[call]?.[2] as { context?: unknown } | undefined)?.context;

describe('dm visibility API', () => {
  beforeEach(() => mockApiFetch.mockReset());

  it('posts Hide without a body and reports the HTTP result', async () => {
    mockApiFetch.mockResolvedValueOnce(response(204, null));

    await expect(hideDMThread(CONVERSATION_ID)).resolves.toBe(true);
    expect(mockApiFetch).toHaveBeenCalledWith(`/api/v1/dm/conversations/${CONVERSATION_ID}/hide`, {
      method: 'POST',
    });

    mockApiFetch.mockResolvedValueOnce(response(500, { error: 'server error' }));
    await expect(hideDMThread(CONVERSATION_ID)).resolves.toBe(false);
  });

  // Rewritten for #3509: the password goes only to the mint endpoint, and
  // Clear gets the single-use token it returned.
  it('posts an empty object, then exchanges the password and sends only the token', async () => {
    mockApiFetch
      .mockResolvedValueOnce(response(403, { password_required: true }))
      .mockResolvedValueOnce(response(200, { step_up_token: 'minted-token', expires_in: 60 }))
      .mockResolvedValueOnce(
        response(200, {
          conversation_id: CONVERSATION_ID,
          cleared_at: '2026-09-23T20:00:00Z',
        })
      );

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({
      kind: 'passwordRequired',
    });
    await expect(
      clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'test-password-123' })
    ).resolves.toEqual({ kind: 'success' });

    expect(mockApiFetch.mock.calls[0]?.[1]).toMatchObject({
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    });
    expect(mockApiFetch.mock.calls[1]).toEqual([
      '/api/v1/auth/step-up/password',
      expect.objectContaining({
        method: 'POST',
        body: '{"current_password":"test-password-123","purpose":"dm.clear"}', // pragma: allowlist secret
      }),
      { context: expect.any(Object) },
    ]);
    expect(mockApiFetch.mock.calls[2]?.[0]).toBe(
      `/api/v1/dm/conversations/${CONVERSATION_ID}/clear`
    );
    expect(mockApiFetch.mock.calls[2]?.[1]).toMatchObject({
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{"step_up_token":"minted-token"}',
    });
    // The exchange and the Clear are admitted against ONE captured context,
    // so the token is never spent by an account that did not mint it (#3509).
    const mintOpts = mockApiFetch.mock.calls[1]?.[2] as { context?: unknown } | undefined;
    const clearOpts = mockApiFetch.mock.calls[2]?.[2] as { context?: unknown } | undefined;
    expect(clearOpts?.context).toBe(mintOpts?.context);
  });

  // Inverted for D2: a refusal that names no methods used to be answered with
  // `['totp']`, which showed a code box to an account that could not fill it.
  // It now stays empty, and the picker lands on its no-usable-method state.
  it('posts only the server-selected MFA factor after MFA is required, naming nothing it was not told', async () => {
    mockApiFetch.mockResolvedValueOnce(response(403, { mfa_required: true })).mockResolvedValueOnce(
      response(200, {
        conversation_id: CONVERSATION_ID,
        cleared_at: '2026-09-23T20:00:00Z',
      })
    );

    // Mutant: reinstating the `['totp']` fallback.
    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({
      kind: 'mfaRequired',
      methods: [],
    });
    await expect(
      clearDMHistory(CONVERSATION_ID, { kind: 'mfa', value: '123456' })
    ).resolves.toEqual({ kind: 'success' });

    expect(mockApiFetch.mock.calls[1]?.[1]).toMatchObject({
      body: '{"mfa_code":"123456"}',
    });
  });

  it.each([
    [
      'password',
      { kind: 'password' as const, value: 'wrong-password' },
      'Invalid password',
      'invalidPassword',
    ],
    ['MFA', { kind: 'mfa' as const, value: '000000' }, 'Invalid MFA code', 'invalidMfaCode'],
  ] as const)(
    'maps an invalid %s factor without retrying it',
    async (_label, factor, error, kind) => {
      mockApiFetch.mockResolvedValueOnce(response(403, { error }));

      await expect(clearDMHistory(CONVERSATION_ID, factor)).resolves.toEqual({ kind });
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    }
  );

  it.each([
    [
      'both factor flags',
      response(403, { password_required: true, mfa_required: true }),
      undefined,
      { kind: 'refused' },
    ],
    ['unknown 403', response(403, { error: 'unknown factor' }), undefined, { kind: 'refused' }],
    [
      'malformed success body',
      response(200, { conversation_id: CONVERSATION_ID }),
      undefined,
      { kind: 'uncertain' },
    ],
    [
      'wrong conversation success body',
      response(200, {
        conversation_id: '22222222-2222-4222-8222-222222222222',
        cleared_at: '2026-09-23T20:00:00Z',
      }),
      undefined,
      { kind: 'uncertain' },
    ],
  ] as const)('fails closed on %s', async (_label, result, factor, expected) => {
    mockApiFetch.mockResolvedValueOnce(result);

    await expect(clearDMHistory(CONVERSATION_ID, factor)).resolves.toEqual(expected);
  });

  it('distinguishes the backend no-factor error from an ordinary refusal', async () => {
    mockApiFetch.mockResolvedValueOnce(
      response(400, { error: 'Clear history requires verification when MFA is not configured' })
    );

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({ kind: 'stepUpImpossible' });
  });

  it.each([
    [401, { error: 'expired' }, { kind: 'sessionExpired' }],
    [404, { error: 'not found' }, { kind: 'notFound' }],
    [500, { error: 'server error' }, { kind: 'uncertain' }],
  ] as const)('maps HTTP %s without claiming success', async (status, body, expected) => {
    mockApiFetch.mockResolvedValueOnce(response(status, body));

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual(expected);
  });

  it('preserves Retry-After for rate limiting', async () => {
    mockApiFetch.mockResolvedValueOnce(
      response(429, { error: 'slow down' }, { 'Retry-After': '37' })
    );

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({
      kind: 'rateLimited',
      retryAfterSeconds: 37,
    });
  });

  it('returns an uncertain result when the response body or fetch is unavailable', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      headers: new Headers(),
      json: vi.fn().mockRejectedValue(new Error('malformed JSON')),
    } as unknown as Response);
    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({ kind: 'uncertain' });

    mockApiFetch.mockRejectedValueOnce(new Error('network failed'));
    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({ kind: 'uncertain' });
  });

  describe('a request that never left (#3509 review)', () => {
    const abort = () => new DOMException('Request lifecycle changed before dispatch', 'AbortError');

    // Mutant: ignoring `minted.unsent`, which blames the password ("We couldn't
    // check your password") for a request that was never made.
    it('reports an unsent password exchange as aborted, and never sends Clear', async () => {
      mockApiFetch.mockRejectedValueOnce(abort());

      await expect(
        clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'test-password-123' })
      ).resolves.toEqual({ kind: 'aborted' });
      expect(mockApiFetch).toHaveBeenCalledOnce();
      expect(mockApiFetch.mock.calls[0]?.[0]).toBe('/api/v1/auth/step-up/password');
    });

    it('still blames the exchange for a transport failure that may have left', async () => {
      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));

      await expect(
        clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'test-password-123' })
      ).resolves.toEqual({
        kind: 'passwordRefused',
        message: "We couldn't check your password. Try again.",
      });
      expect(mockApiFetch).toHaveBeenCalledOnce();
    });

    it('reports a Clear fenced before dispatch as aborted, and any other rejection as uncertain', async () => {
      mockApiFetch.mockRejectedValueOnce(abort());
      await expect(
        clearDMHistory(CONVERSATION_ID, { kind: 'mfa', value: '123456' })
      ).resolves.toEqual({ kind: 'aborted' });

      mockApiFetch.mockRejectedValueOnce(new TypeError('Failed to fetch'));
      await expect(
        clearDMHistory(CONVERSATION_ID, { kind: 'mfa', value: '123456' })
      ).resolves.toEqual({ kind: 'uncertain' });
    });
  });

  describe('the caller capture', () => {
    it('admits an MFA Clear against the caller context, and a bare Clear against none', async () => {
      const context = captureApiRequestContext();
      mockApiFetch
        .mockResolvedValueOnce(response(403, { error: 'x' }))
        .mockResolvedValueOnce(response(403, { error: 'x' }));

      await clearDMHistory(CONVERSATION_ID, { kind: 'mfa', value: '123456' }, context);
      await clearDMHistory(CONVERSATION_ID, { kind: 'mfa', value: '123456' });

      expect(contextOf(0)).toBe(context);
      expect(mockApiFetch.mock.calls[1]).toHaveLength(2);
    });

    it('exchanges the password and sends Clear under the caller context', async () => {
      const context = captureApiRequestContext();
      mockApiFetch
        .mockResolvedValueOnce(response(200, { step_up_token: 'minted-token', expires_in: 60 }))
        .mockResolvedValueOnce(
          response(200, { conversation_id: CONVERSATION_ID, cleared_at: '2026-09-23T20:00:00Z' })
        );

      await expect(
        clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'test-password-123' }, context)
      ).resolves.toEqual({ kind: 'success' });

      // Mutant: the exchange taking its own capture instead of the caller's.
      // Identity, not structure: a fresh capture of the same state is `toEqual`.
      expect(contextOf(0)).toBe(context);
      expect(contextOf(1)).toBe(context);
    });
  });

  describe('mint refusals', () => {
    it.each([
      [423, { error_code: 'account_locked' }, 'Too many attempts. Try again later.'],
      [404, { error: 'Not Found' }, "This server doesn't support this confirmation yet."],
      [500, {}, "We couldn't check your password. Try again."],
    ] as const)(
      'puts the %i mint refusal on the password field and never sends Clear',
      async (status, body, message) => {
        mockApiFetch.mockResolvedValueOnce(response(status, body));

        await expect(
          clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'test-password-123' })
        ).resolves.toEqual({ kind: 'passwordRefused', message });
        expect(mockApiFetch).toHaveBeenCalledOnce();
      }
    );

    it("maps Clear's refused token to the expiry copy", async () => {
      mockApiFetch.mockResolvedValueOnce(
        response(403, { password_required: true, step_up_token_invalid: true })
      );

      await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({
        kind: 'passwordRefused',
        message: 'Your confirmation expired. Enter your password again.',
      });
    });

    it('moves an account that gained MFA mid-prompt to the code stage, naming what the mint named', async () => {
      mockApiFetch.mockResolvedValueOnce(
        response(403, { mfa_required: true, mfa_methods: ['totp', 'webauthn'] })
      );

      await expect(
        clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'test-password-123' })
      ).resolves.toEqual({ kind: 'mfaRequired', methods: ['totp', 'webauthn'] });
    });
  });

  it('reads the no-factor flag, and the older copy, as an impossible step-up', async () => {
    mockApiFetch.mockResolvedValueOnce(response(400, { step_up_unavailable: true }));

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({ kind: 'stepUpImpossible' });
  });

  it('treats a non-object body as an empty one', async () => {
    mockApiFetch.mockResolvedValueOnce(response(403, ['mfa_required']));

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({ kind: 'refused' });
  });
});
