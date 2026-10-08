import { beforeEach, describe, expect, it, vi } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';

// The server's MFA-enforcement setting (#3456 §3.5). Only `apiFetch` is
// replaced; the request context, the abort classification and the body shapes
// are real. "Mutant:" comments name the production change each case turns red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import {
  fetchMfaEnforcement,
  mfaEnforcementPath,
  putMfaEnforcement,
} from '@/renderer/services/system/mfaEnforcementApi';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';

// Named fixture: the pre-commit detect-secrets hook flags credential-shaped keys.
const FIXTURE_OTP = '314159';

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });

const abortError = () => new DOMException('aborted', 'AbortError');

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset();
});

describe('mfaEnforcementPath', () => {
  // Mutant: the id is interpolated raw (a crafted id reaches another route).
  it('encodes the server id as one path segment', () => {
    expect(mfaEnforcementPath('a/b?c')).toBe('/api/v1/servers/a%2Fb%3Fc/mfa-enforcement');
  });
});

describe('fetchMfaEnforcement', () => {
  // Mutant: GET not sent with the caller's signal, or another path.
  it('reads the server route with a GET and the signal', async () => {
    mockApiFetch.mockResolvedValue(json(200, { enforce_mfa_dangerous_actions: true }));
    const controller = new AbortController();

    await fetchMfaEnforcement('srv', controller.signal);

    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/srv/mfa-enforcement', {
      method: 'GET',
      signal: controller.signal,
    });
  });

  // Mutant: the boolean is coerced (`!!`) or taken from another key.
  it.each([true, false])('a 200 carrying %s is ok with that value', async (enforcing) => {
    mockApiFetch.mockResolvedValue(json(200, { enforce_mfa_dangerous_actions: enforcing }));

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'ok',
      enforcing,
    });
  });

  // Mutant: 403/404 fall to `unavailable`, or 404 is dropped from the absent set.
  it.each([403, 404])('a %i is absent: not this member’s to see', async (code) => {
    mockApiFetch.mockResolvedValue(json(code, {}));

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'absent',
    });
  });

  // Mutant: another failing status is mapped to `absent` (hides the setting) or to `ok`.
  it.each([400, 401, 429, 500, 503])('a %i is unavailable', async (code) => {
    mockApiFetch.mockResolvedValue(json(code, {}));

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'unavailable',
    });
  });

  // Mutant: a 200 that is not the setting is read as a value (undefined coerced to false).
  it.each([
    ['an empty object', {}],
    ['a string', { enforce_mfa_dangerous_actions: 'true' }],
    ['a number', { enforce_mfa_dangerous_actions: 1 }],
    ['null', { enforce_mfa_dangerous_actions: null }],
    ['a bare boolean', true],
    ['null body', null],
  ])('a 200 whose body is %s is unavailable', async (_name, body) => {
    mockApiFetch.mockResolvedValue(json(200, body));

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'unavailable',
    });
  });

  // Mutant: an unparsable body throws out of the service instead of being unavailable.
  it('a 200 that is not JSON is unavailable', async () => {
    mockApiFetch.mockResolvedValue(new Response('<html>', { status: 200 }));

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'unavailable',
    });
  });

  // Mutant: a transport failure is rethrown, or classified as aborted.
  it('a transport failure is unavailable', async () => {
    mockApiFetch.mockRejectedValue(new TypeError('network'));

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'unavailable',
    });
  });

  // Mutant: an AbortError is reported as `unavailable` (a cancelled read paints the Retry row).
  it('an AbortError, or apiFetch’s pre-dispatch fence, is aborted', async () => {
    mockApiFetch.mockRejectedValue(abortError());

    await expect(fetchMfaEnforcement('srv', new AbortController().signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });

  // Mutant: the signal is not consulted when the rejection is not an AbortError.
  it('a rejection with the signal already aborted is aborted, whatever its type', async () => {
    const controller = new AbortController();
    mockApiFetch.mockImplementation(async () => {
      controller.abort();
      throw new TypeError('the signal reason');
    });

    await expect(fetchMfaEnforcement('srv', controller.signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });

  // Mutant: the post-read `signal.aborted` check dropped (a superseded read's answer is rendered).
  it('an answer that arrives after the signal aborted is discarded', async () => {
    const controller = new AbortController();
    mockApiFetch.mockImplementation(async () => {
      controller.abort();
      return json(200, { enforce_mfa_dangerous_actions: true });
    });

    await expect(fetchMfaEnforcement('srv', controller.signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });
});

describe('putMfaEnforcement', () => {
  const context = () => captureApiRequestContext();
  const sent = () => JSON.parse((mockApiFetch.mock.calls[0][1] as { body: string }).body);

  // Mutant: the verb, header or path changes; the context is not passed to apiFetch.
  it('PUTs JSON to the server route, admitted against the context', async () => {
    mockApiFetch.mockResolvedValue(json(200, {}));
    const ctx = context();

    await putMfaEnforcement('srv', { enabled: true }, ctx);

    const [path, init, options] = mockApiFetch.mock.calls[0];
    expect(path).toBe('/api/v1/servers/srv/mfa-enforcement');
    expect(init).toMatchObject({ method: 'PUT', headers: { 'Content-Type': 'application/json' } });
    expect(options).toEqual({ context: ctx });
  });

  // Mutant: a code key is sent with ON (`mfa_code: undefined` serialises away, but `null`/'' would not).
  it('ON carries no code', async () => {
    mockApiFetch.mockResolvedValue(json(200, {}));

    await putMfaEnforcement('srv', { enabled: true }, context());

    expect(sent()).toEqual({ enabled: true });
  });

  // Mutant: an empty string is sent as `mfa_code` (the server charges it to the budget as a wrong code).
  it('an empty code is omitted, not sent', async () => {
    mockApiFetch.mockResolvedValue(json(200, {}));

    await putMfaEnforcement('srv', { enabled: false, mfaCode: '' }, context());

    expect(sent()).toEqual({ enabled: false });
  });

  // Mutant: the code is sent under another key, or dropped.
  it('OFF with a code sends it as mfa_code', async () => {
    mockApiFetch.mockResolvedValue(json(200, {}));

    await putMfaEnforcement('srv', { enabled: false, mfaCode: FIXTURE_OTP }, context());

    expect(sent()).toEqual({ enabled: false, mfa_code: FIXTURE_OTP });
  });

  // Mutant: any 2xx other than the one checked, or `res.ok` replaced by `status === 200`.
  it.each([200, 204])('a %i is ok', async (code) => {
    mockApiFetch.mockResolvedValue(new Response(code === 204 ? null : '{}', { status: code }));

    await expect(putMfaEnforcement('srv', { enabled: true }, context())).resolves.toEqual({
      kind: 'ok',
    });
  });

  // Mutant: the refusal loses its status or its parsed body (the adapter then cannot classify it).
  it('a refusal carries its status and parsed body', async () => {
    mockApiFetch.mockResolvedValue(json(403, { mfa_required: true, methods: ['totp'] }));

    await expect(putMfaEnforcement('srv', { enabled: false }, context())).resolves.toEqual({
      kind: 'refused',
      status: 403,
      body: { mfa_required: true, methods: ['totp'] },
    });
  });

  // Mutant: an unparsable refusal body throws instead of being null.
  it('a refusal whose body is not JSON carries a null body', async () => {
    mockApiFetch.mockResolvedValue(new Response('<html>', { status: 502 }));

    await expect(putMfaEnforcement('srv', { enabled: true }, context())).resolves.toEqual({
      kind: 'refused',
      status: 502,
      body: null,
    });
  });

  // Mutant: a rejected request is rethrown, or reported as a refusal.
  it('no response is transport', async () => {
    mockApiFetch.mockRejectedValue(new TypeError('network'));

    await expect(putMfaEnforcement('srv', { enabled: true }, context())).resolves.toEqual({
      kind: 'transport',
    });
  });

  // Mutant: the fence's AbortError is reported as `transport` (the host then re-reads and shows a network error).
  it('an AbortError, from apiFetch’s fence, is aborted', async () => {
    mockApiFetch.mockRejectedValue(abortError());

    await expect(putMfaEnforcement('srv', { enabled: true }, context())).resolves.toEqual({
      kind: 'aborted',
    });
  });
});
