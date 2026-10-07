import { beforeEach, describe, expect, it, vi } from 'vitest';

import { apiFetch } from '@/renderer/services/system/apiClient';
import { clearDMHistory } from '@/renderer/services/messaging/dmVisibilityApi';

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));

// Codex on #3509: Clear's mfaRequired result carried no methods, so the
// dialog could not know a security key was the account's factor.

const mockApiFetch = vi.mocked(apiFetch);
const CONVERSATION_ID = '11111111-1111-4111-8111-111111111111';

const response = (status: number, body: unknown): Response =>
  ({
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: async () => body,
  }) as Response;

describe('clearDMHistory MFA methods', () => {
  beforeEach(() => mockApiFetch.mockReset());

  it("carries the methods Clear's own refusal names", async () => {
    mockApiFetch.mockResolvedValueOnce(
      response(403, { mfa_required: true, methods: ['webauthn', 'totp'] })
    );

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({
      kind: 'mfaRequired',
      methods: ['webauthn', 'totp'],
    });
  });

  it('carries the methods the password mint names', async () => {
    mockApiFetch.mockResolvedValueOnce(
      response(403, { mfa_required: true, mfa_methods: ['webauthn'] })
    );

    await expect(
      clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'pw' })
    ).resolves.toEqual({ kind: 'mfaRequired', methods: ['webauthn'] });
  });

  // D2: the methods are exactly what the server named. A refusal that names
  // none, or none this app can verify, is the picker's no-usable-method state;
  // a substituted ['totp'] showed a code box the account could not fill.
  it.each([
    ['omits methods', { mfa_required: true }, []],
    ['sends an empty list', { mfa_required: true, methods: [] }, []],
    ['sends a non-array', { mfa_required: true, methods: 'totp' }, []],
    ['mixes in non-strings', { mfa_required: true, methods: ['totp', 7, null] }, ['totp']],
    [
      'names only a method the app cannot verify',
      { mfa_required: true, methods: ['email'] },
      ['email'],
    ],
  ])("keeps Clear's refusal exactly as named when it %s", async (_name, body, methods) => {
    mockApiFetch.mockResolvedValueOnce(response(403, body));

    await expect(clearDMHistory(CONVERSATION_ID)).resolves.toEqual({
      kind: 'mfaRequired',
      methods,
    });
  });

  it.each([
    ['omits mfa_methods', { mfa_required: true }],
    ['sends an empty list', { mfa_required: true, mfa_methods: [] }],
  ])('keeps the password mint refusal empty when it %s', async (_name, body) => {
    mockApiFetch.mockResolvedValueOnce(response(403, body));

    await expect(
      clearDMHistory(CONVERSATION_ID, { kind: 'password', value: 'pw' })
    ).resolves.toEqual({ kind: 'mfaRequired', methods: [] });
  });
});
