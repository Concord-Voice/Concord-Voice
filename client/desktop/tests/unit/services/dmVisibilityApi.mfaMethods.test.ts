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
});
