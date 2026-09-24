import { beforeEach, describe, expect, it, vi } from 'vitest';

import { apiFetch } from '@/renderer/services/system/apiClient';
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

  it('posts an empty object, then only the server-selected password factor', async () => {
    mockApiFetch
      .mockResolvedValueOnce(response(403, { password_required: true }))
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
    expect(mockApiFetch.mock.calls[1]?.[1]).toMatchObject({
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{"current_password":"test-password-123"}', // pragma: allowlist secret
    });
  });

  it('posts only the server-selected MFA factor after MFA is required', async () => {
    mockApiFetch.mockResolvedValueOnce(response(403, { mfa_required: true })).mockResolvedValueOnce(
      response(200, {
        conversation_id: CONVERSATION_ID,
        cleared_at: '2026-09-23T20:00:00Z',
      })
    );

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({ kind: 'mfaRequired' });
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
});
