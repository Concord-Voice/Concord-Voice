// @vitest-environment jsdom
//
// Orchestration tests for submitSignedAgeClaim (#1624). The crypto correctness is
// covered by signing.test.ts; here the signature is mocked so the focus is the
// orchestration: claim assembly, just-in-time key_version fetch, error_code
// mapping, and — the privacy crux — that the raw birthdate never reaches a
// persistence sink or the request body.
import { describe, it, expect, beforeEach, vi } from 'vitest';

// vi.hoisted: these are referenced by the hoisted vi.mock factories below, so they
// must be hoisted alongside (a plain const initializes too late — ReferenceError).
const { mockApiFetch, mockUser, mockE2EE } = vi.hoisted(() => ({
  mockApiFetch: vi.fn(),
  mockUser: { user: null as { id: string } | null },
  mockE2EE: { isInitialized: true, signAgeClaim: vi.fn() },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (path: string, init?: RequestInit) => mockApiFetch(path, init),
}));
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: mockE2EE,
}));
vi.mock('@/renderer/stores/userStore', () => ({
  useUserStore: { getState: () => mockUser },
}));

import { submitSignedAgeClaim } from '@/renderer/services/ageClaim/ageClaimService';

const USER_ID = '11111111-1111-4111-8111-111111111111';
const BIRTH_YEAR = 2008;
const adultSignal = { kind: 'birthdate' as const, year: BIRTH_YEAR, month: 1, day: 1 };

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** Default routing: GET public-key -> key_version 1; PUT claim -> 200 ok. */
function defaultApi(putResponse: Response = new Response(null, { status: 200 })) {
  mockApiFetch.mockImplementation((path: string) => {
    if (path.includes('/public-key')) return Promise.resolve(jsonResponse({ key_version: 1 }));
    return Promise.resolve(putResponse);
  });
}

function putCallBody(): Record<string, unknown> {
  const put = mockApiFetch.mock.calls.find(([path]) => path === '/api/v1/age/claim');
  if (!put) throw new Error('PUT /api/v1/age/claim was not called');
  return JSON.parse((put[1] as RequestInit).body as string);
}

describe('submitSignedAgeClaim', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockUser.user = { id: USER_ID };
    mockE2EE.isInitialized = true;
    mockE2EE.signAgeClaim.mockResolvedValue('c2lnbmF0dXJlLWJhc2U2NA==');
    (window as unknown as { electron: { getVersion: () => Promise<string> } }).electron = {
      getVersion: vi.fn().mockResolvedValue('0.1.65'),
    };
    defaultApi();
  });

  it('submits a well-formed claim and returns ok on 200', async () => {
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: true, validAge: true, nsfwAuth: true });

    const body = putCallBody();
    // 8 signed-core fields (minus user_id) + signature.
    expect(body).toMatchObject({
      canonical_version: 1,
      valid_age: true,
      nsfw_auth: true,
      jurisdiction_obligation: 0,
      key_version: 1,
      client_version: '0.1.65',
    });
    expect(typeof body.nonce).toBe('string');
    expect(body.nonce as string).toMatch(/^[0-9a-f]{64}$/);
    expect(typeof body.timestamp).toBe('number');
    expect(typeof body.signature).toBe('string');
    // user_id is NEVER in the body (server reconstructs from JWT).
    expect(body).not.toHaveProperty('user_id');
  });

  it('fetches the current key_version just-in-time and signs under it', async () => {
    mockApiFetch.mockImplementation((path: string) => {
      if (path.includes('/public-key')) return Promise.resolve(jsonResponse({ key_version: 7 }));
      return Promise.resolve(new Response(null, { status: 200 }));
    });
    await submitSignedAgeClaim({ signal: adultSignal });
    expect(mockApiFetch).toHaveBeenCalledWith(`/api/v1/users/${USER_ID}/public-key`, undefined);
    expect(putCallBody().key_version).toBe(7);
  });

  it('passes a caller-supplied jurisdiction_obligation through', async () => {
    await submitSignedAgeClaim({ signal: adultSignal, jurisdictionObligation: 2 });
    expect(putCallBody().jurisdiction_obligation).toBe(2);
  });

  it.each([
    ['account_disabled', 403],
    ['stale_key_version', 422],
    ['invalid_signature', 422],
    ['replayed_nonce', 409],
  ])('maps server error_code %s to a failed result', async (code, status) => {
    defaultApi(jsonResponse({ error_code: code }, status));
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code });
  });

  it('returns unavailable when not logged in', async () => {
    mockUser.user = null;
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('returns unavailable when E2EE is not initialized', async () => {
    mockE2EE.isInitialized = false;
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
  });

  it('returns unavailable when the key-version fetch fails', async () => {
    mockApiFetch.mockImplementation((path: string) => {
      if (path.includes('/public-key')) return Promise.resolve(new Response(null, { status: 500 }));
      return Promise.resolve(new Response(null, { status: 200 }));
    });
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
  });

  it('returns unavailable on a network error during submit', async () => {
    mockApiFetch.mockImplementation((path: string) => {
      if (path.includes('/public-key')) return Promise.resolve(jsonResponse({ key_version: 1 }));
      return Promise.reject(new Error('network down'));
    });
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
  });

  it('returns malformed when the assembled claim fails validation', async () => {
    // A non-uuid user id survives toLowerCase() but fails validateAgeClaim.
    mockUser.user = { id: 'not-a-valid-uuid' };
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'malformed' });
  });

  it('returns unavailable when signing fails', async () => {
    mockE2EE.signAgeClaim.mockRejectedValue(new Error('sign failed'));
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
  });

  it('returns unavailable when the key-version response is malformed', async () => {
    mockApiFetch.mockImplementation((path: string) => {
      if (path.includes('/public-key')) return Promise.resolve(jsonResponse({}));
      return Promise.resolve(new Response(null, { status: 200 }));
    });
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
  });

  it('falls back to a default client_version when getVersion fails', async () => {
    (window as unknown as { electron: { getVersion: () => Promise<string> } }).electron = {
      getVersion: vi.fn().mockRejectedValue(new Error('ipc down')),
    };
    await submitSignedAgeClaim({ signal: adultSignal });
    expect(putCallBody().client_version).toBe('0.0.0');
  });

  it('maps an unparseable server error body to unavailable', async () => {
    defaultApi(new Response('not json', { status: 500 }));
    const result = await submitSignedAgeClaim({ signal: adultSignal });
    expect(result).toEqual({ ok: false, code: 'unavailable' });
  });

  it('NEVER persists the raw birthdate, and never sends it on the wire', async () => {
    const setItemSpy = vi.spyOn(Storage.prototype, 'setItem');
    await submitSignedAgeClaim({ signal: adultSignal });

    // No persistence sink received the raw birth year.
    const persisted = setItemSpy.mock.calls.map((c) => String(c[1]));
    expect(persisted.some((v) => v.includes(String(BIRTH_YEAR)))).toBe(false);

    // The wire body carries only booleans + derived fields — no raw DOB.
    const body = putCallBody();
    expect(JSON.stringify(body)).not.toContain(String(BIRTH_YEAR));
    expect(body).not.toHaveProperty('year');
    expect(body).not.toHaveProperty('birthdate');
    expect(body).not.toHaveProperty('signal');
  });
});
