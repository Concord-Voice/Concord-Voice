import { beforeEach, describe, expect, it, vi } from 'vitest';

import { apiFetch } from '@/renderer/services/system/apiClient';
import {
  STEP_UP_REQUIREMENTS_PATH,
  codeProvenUnspent,
  fetchStepUpRequirements,
  intersectInline,
  isTotpHintActive,
  isTotpShaped,
  pickDefaultMethod,
  totpHintExpiresAt,
  type InlineStepUpMethod,
} from '@/renderer/services/system/stepUpRequirements';
import type { StepUpRefusal } from '@/renderer/services/system/stepUpRefusal';

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));

const mockApiFetch = vi.mocked(apiFetch);

/** A Response stand-in. `body` may be a function that throws, to model an unreadable body. */
const response = (status: number, body: unknown): Response =>
  ({
    ok: status >= 200 && status < 300,
    status,
    json: typeof body === 'function' ? (body as () => Promise<unknown>) : async () => body,
  }) as Response;

const unreadable = () => Promise.reject(new SyntaxError('Unexpected end of JSON input'));

const GOOD_BODY = {
  methods: ['webauthn', 'totp'],
  default_method: 'totp',
  backup_code_available: true,
};

/** Answers the next read with `res`, then runs it. */
async function readWith(res: Response) {
  mockApiFetch.mockResolvedValueOnce(res);
  return fetchStepUpRequirements(new AbortController().signal);
}

beforeEach(() => {
  mockApiFetch.mockReset();
});

describe('fetchStepUpRequirements: ready', () => {
  it('issues one GET to the route with the caller signal', async () => {
    const { signal } = new AbortController();
    mockApiFetch.mockResolvedValueOnce(response(200, GOOD_BODY));

    await fetchStepUpRequirements(signal);

    expect(mockApiFetch).toHaveBeenCalledTimes(1);
    expect(mockApiFetch).toHaveBeenCalledWith(STEP_UP_REQUIREMENTS_PATH, {
      method: 'GET',
      signal,
    });
  });

  it('reads a well-formed body', async () => {
    await expect(readWith(response(200, GOOD_BODY))).resolves.toEqual({
      kind: 'ready',
      methods: ['webauthn', 'totp'],
      defaultMethod: 'totp',
      backupCodeAvailable: true,
    });
  });

  it('passes backup_code_available through as given', async () => {
    const result = await readWith(response(200, { ...GOOD_BODY, backup_code_available: false }));

    expect(result).toMatchObject({ kind: 'ready', backupCodeAvailable: false });
  });

  // Mutant: email/sms survive the intersection.
  it('drops email, sms and unknown methods from the offered set', async () => {
    const result = await readWith(
      response(200, { ...GOOD_BODY, methods: ['email', 'sms', 'totp', 'carrier-pigeon'] })
    );

    expect(result).toMatchObject({ kind: 'ready', methods: ['totp'] });
  });

  // Mutant: a list naming only email/sms is read as usable.
  it('is ready with an empty set when only email and sms are offered', async () => {
    const result = await readWith(
      response(200, {
        methods: ['email', 'sms'],
        default_method: 'email',
        backup_code_available: false,
      })
    );

    expect(result).toEqual({
      kind: 'ready',
      methods: [],
      defaultMethod: null,
      backupCodeAvailable: false,
    });
  });

  it('accepts an empty methods list with a null default', async () => {
    const result = await readWith(
      response(200, { methods: [], default_method: null, backup_code_available: false })
    );

    expect(result).toMatchObject({ kind: 'ready', methods: [], defaultMethod: null });
  });

  // Mutant: the server default is trusted without checking it is offered.
  it.each([
    ['a method that is not offered', { methods: ['totp'], default_method: 'webauthn' }],
    ['email', { methods: ['totp'], default_method: 'email' }],
    ['backup', { methods: ['totp'], default_method: 'backup' }],
    ['null', { methods: ['totp'], default_method: null }],
  ])('keeps a server default of %s out of the result', async (_label, partial) => {
    const result = await readWith(response(200, { ...GOOD_BODY, ...partial }));

    expect(result).toMatchObject({ kind: 'ready', methods: ['totp'], defaultMethod: null });
  });

  // Mutant: intersectInline lets duplicates or the server's order through.
  it('lists each method once, strongest first, whatever order the server used', async () => {
    const result = await readWith(
      response(200, { ...GOOD_BODY, methods: ['totp', 'webauthn', 'totp', 'webauthn'] })
    );

    expect(result).toMatchObject({ kind: 'ready', methods: ['webauthn', 'totp'] });
  });
});

describe('fetchStepUpRequirements: malformed 200 is unavailable, never ready (Q6)', () => {
  // Mutant: a malformed 200 is read as `ready`.
  it.each<[string, unknown]>([
    ['a null body', null],
    ['an array body', [GOOD_BODY]],
    ['a string body', 'ok'],
    ['an empty object', {}],
    ['methods missing', { default_method: 'totp', backup_code_available: true }],
    ['methods not an array', { ...GOOD_BODY, methods: 'totp' }],
    ['a non-string method', { ...GOOD_BODY, methods: ['totp', 7] }],
    ['a null method', { ...GOOD_BODY, methods: [null] }],
    ['default_method missing', { methods: ['totp'], backup_code_available: true }],
    ['a numeric default_method', { ...GOOD_BODY, default_method: 3 }],
    ['an object default_method', { ...GOOD_BODY, default_method: { name: 'totp' } }],
    ['backup_code_available missing', { methods: ['totp'], default_method: 'totp' }],
    ['a string backup_code_available', { ...GOOD_BODY, backup_code_available: 'true' }],
    ['a numeric backup_code_available', { ...GOOD_BODY, backup_code_available: 1 }],
    ['a null backup_code_available', { ...GOOD_BODY, backup_code_available: null }],
  ])('maps %s to unavailable', async (_label, body) => {
    await expect(readWith(response(200, body))).resolves.toEqual({ kind: 'unavailable' });
  });

  it('maps a 200 whose body cannot be parsed to unavailable', async () => {
    await expect(readWith(response(200, unreadable))).resolves.toEqual({ kind: 'unavailable' });
  });
});

describe('fetchStepUpRequirements: HTTP outcomes', () => {
  // Mutant: 404 is read as `unavailable` (or anything but `unsupported`).
  it('maps 404 to unsupported', async () => {
    await expect(readWith(response(404, { error: 'not found' }))).resolves.toEqual({
      kind: 'unsupported',
    });
  });

  // Mutant: 401 is read as `unavailable`.
  it('maps a 401 that survived the refresh to refused/session', async () => {
    await expect(readWith(response(401, { error: 'unauthorized' }))).resolves.toEqual({
      kind: 'refused',
      reason: 'session',
    });
  });

  // Mutant: account_disabled maps to anything but refused/account.
  it('maps 403 account_disabled to refused/account', async () => {
    await expect(
      readWith(response(403, { error_code: 'account_disabled', error: 'Account disabled' }))
    ).resolves.toEqual({ kind: 'refused', reason: 'account' });
  });

  // Mutant: the EMAIL_NOT_VERIFIED mapping is lost.
  it('maps 403 EMAIL_NOT_VERIFIED to refused/emailUnverified', async () => {
    await expect(
      readWith(response(403, { code: 'EMAIL_NOT_VERIFIED', error: 'Verify your email' }))
    ).resolves.toEqual({ kind: 'refused', reason: 'emailUnverified' });
  });

  it.each([
    ['an attestation refusal', { code: 'ATTESTATION_REQUIRED' }],
    ['a client-version refusal', { code: 'CLIENT_VERSION_TOO_OLD' }],
    ['an unrecognised body', { error: 'forbidden' }],
    ['a null body', null],
    ['an unreadable body', unreadable],
  ])('maps 403 with %s to refused/client', async (_label, body) => {
    await expect(readWith(response(403, body))).resolves.toEqual({
      kind: 'refused',
      reason: 'client',
    });
  });

  it.each([400, 405, 409, 422])('maps %i to refused/client', async (status) => {
    await expect(readWith(response(status, { error: 'nope' }))).resolves.toEqual({
      kind: 'refused',
      reason: 'client',
    });
  });

  // Mutant: 429 or a 5xx is read as a refusal (terminal) rather than a retryable read.
  it.each([429, 500, 502, 503, 504])('maps %i to unavailable', async (status) => {
    await expect(readWith(response(status, { error: 'later' }))).resolves.toEqual({
      kind: 'unavailable',
    });
  });

  it('maps an unexpected non-error status to unavailable', async () => {
    await expect(readWith(response(302, null))).resolves.toEqual({ kind: 'unavailable' });
  });
});

describe('fetchStepUpRequirements: transport failure and abort never throw', () => {
  // Mutants: a network rejection is not `unavailable`; the function throws.
  it.each([
    ['a network TypeError', () => new TypeError('Failed to fetch')],
    ['a plain Error', () => new Error('socket hang up')],
    ['a non-Error rejection', () => 'boom'],
  ])('maps %s to unavailable', async (_label, make) => {
    mockApiFetch.mockRejectedValueOnce(make());

    await expect(fetchStepUpRequirements(new AbortController().signal)).resolves.toEqual({
      kind: 'unavailable',
    });
  });

  // Mutant: apiFetch's pre-dispatch fence (an AbortError with the signal still
  // live) is read as `unavailable`.
  it.each([
    ['a DOMException', () => new DOMException('The operation was aborted.', 'AbortError')],
    ['an Error', () => Object.assign(new Error('aborted before dispatch'), { name: 'AbortError' })],
  ])('maps %s named AbortError to aborted even when the signal is live', async (_label, make) => {
    const controller = new AbortController();
    mockApiFetch.mockRejectedValueOnce(make());

    await expect(fetchStepUpRequirements(controller.signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });

  // Mutant: fetch rejects with the signal's own (non-AbortError) reason.
  it('maps any rejection to aborted once the signal has aborted', async () => {
    const controller = new AbortController();
    mockApiFetch.mockImplementationOnce(async () => {
      controller.abort(new Error('caller gave up'));
      throw new Error('caller gave up');
    });

    await expect(fetchStepUpRequirements(controller.signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });

  // Mutant: a response that arrives after the abort is rendered.
  it('discards a 200 that arrives after the signal aborted', async () => {
    const controller = new AbortController();
    mockApiFetch.mockImplementationOnce(async () => {
      controller.abort();
      return response(200, GOOD_BODY);
    });

    await expect(fetchStepUpRequirements(controller.signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });

  it('discards a refusal that arrives after the signal aborted', async () => {
    const controller = new AbortController();
    mockApiFetch.mockImplementationOnce(async () => {
      controller.abort();
      return response(401, { error: 'unauthorized' });
    });

    await expect(fetchStepUpRequirements(controller.signal)).resolves.toEqual({
      kind: 'aborted',
    });
  });
});

describe('intersectInline', () => {
  it('keeps webauthn and totp, strongest first, each once', () => {
    expect(intersectInline(['totp', 'webauthn', 'totp'])).toEqual(['webauthn', 'totp']);
  });

  it.each([[['email', 'sms']], [['backup']], [['']], [[]]])(
    'returns an empty set for %j',
    (methods) => {
      expect(intersectInline(methods)).toEqual([]);
    }
  );

  it('drops email and sms from a mixed list', () => {
    expect(intersectInline(['sms', 'totp', 'email'])).toEqual(['totp']);
  });
});

describe('pickDefaultMethod', () => {
  it('returns the server default when it is offered', () => {
    expect(pickDefaultMethod(['webauthn', 'totp'], 'totp')).toBe('totp');
    expect(pickDefaultMethod(['webauthn', 'totp'], 'webauthn')).toBe('webauthn');
  });

  // Mutant: a server default outside the offered set is returned.
  it('falls back to the strongest offered method when the default is not offered', () => {
    expect(pickDefaultMethod(['totp'], 'webauthn')).toBe('totp');
    expect(pickDefaultMethod(['webauthn'], 'totp')).toBe('webauthn');
  });

  // Mutant: `backup` (or email/sms) returned as the default.
  it.each(['backup', 'email', 'sms', 'unknown', ''])(
    'never returns %j, and falls back to the strongest offered method',
    (serverDefault) => {
      expect(pickDefaultMethod(['webauthn', 'totp'], serverDefault)).toBe('webauthn');
      expect(pickDefaultMethod(['totp'], serverDefault)).toBe('totp');
    }
  );

  it('does not return backup even when a caller put it in the offered set', () => {
    const offered = ['backup'] as unknown as InlineStepUpMethod[];

    expect(pickDefaultMethod(offered, 'backup')).toBeNull();
    expect(pickDefaultMethod([...offered, 'totp'] as InlineStepUpMethod[], 'backup')).toBe('totp');
  });

  it.each([null, undefined])(
    'uses the strongest offered method for a %s default',
    (serverDefault) => {
      expect(pickDefaultMethod(['webauthn', 'totp'], serverDefault)).toBe('webauthn');
    }
  );

  // Mutant: strength order follows the order the caller built the set in.
  it('picks by strength, not by the offered set order', () => {
    expect(pickDefaultMethod(['totp', 'webauthn'], null)).toBe('webauthn');
  });

  it('returns null when nothing is offered, whatever the server default says', () => {
    expect(pickDefaultMethod([], null)).toBeNull();
    expect(pickDefaultMethod([], 'totp')).toBeNull();
    expect(pickDefaultMethod([], 'backup')).toBeNull();
  });
});

describe('isTotpShaped', () => {
  it.each(['123456', '000000', '123 456', '123-456', ' 1 2-3 4 5-6 ', '12 34 56'])(
    'accepts %j',
    (code) => {
      expect(isTotpShaped(code)).toBe(true);
    }
  );

  // Mutants: accepting 7 or 8 characters, letters, or non-ASCII digits.
  it.each([
    ['five digits', '12345'],
    ['seven digits', '1234567'],
    ['eight digits', '12345678'],
    ['an eight-character backup code', 'ABCD1234'],
    ['letters in a six-character code', '12345a'],
    ['an empty string', ''],
    ['only separators', ' - - '],
    ['a trailing newline', '123456\n'],
    ['Arabic-Indic digits', '١٢٣٤٥٦'],
    ['fullwidth digits', '１２３４５６'],
    ['six digits plus a letter', '123456a'],
  ])('rejects %s', (_label, code) => {
    expect(isTotpShaped(code)).toBe(false);
  });
});

describe('codeProvenUnspent', () => {
  // One entry per StepUpRefusal kind; the Record type makes tsc fail when a kind is added.
  const REFUSALS: Record<StepUpRefusal['kind'], { refusal: StepUpRefusal; unspent: boolean }> = {
    invalidMfaCode: { refusal: { kind: 'invalidMfaCode' }, unspent: true },
    invalidPassword: { refusal: { kind: 'invalidPassword' }, unspent: true },
    mfaRequired: { refusal: { kind: 'mfaRequired', methods: ['totp'] }, unspent: true },
    passwordRequired: { refusal: { kind: 'passwordRequired' }, unspent: true },
    rateLimited: { refusal: { kind: 'rateLimited' }, unspent: true },
    deleteRateLimited: {
      refusal: { kind: 'deleteRateLimited', methods: ['totp'] },
      unspent: false,
    },
    unavailable: { refusal: { kind: 'unavailable' }, unspent: false },
    inlineFactorRequired: {
      refusal: { kind: 'inlineFactorRequired', message: 'Turn off email codes first.' },
      unspent: false,
    },
    sessionExpired: { refusal: { kind: 'sessionExpired' }, unspent: false },
    failed: { refusal: { kind: 'failed' }, unspent: false },
  };

  // Mutants: true for anything outside the five, or false for one of them.
  it.each(Object.entries(REFUSALS))('%s', (_kind, { refusal, unspent }) => {
    expect(codeProvenUnspent(refusal)).toBe(unspent);
  });

  it('judges by kind, so an expired-token password refusal is still unspent', () => {
    expect(codeProvenUnspent({ kind: 'passwordRequired', tokenExpired: true })).toBe(true);
  });

  it('treats a failed refusal carrying a server message as possibly spent', () => {
    expect(codeProvenUnspent({ kind: 'failed', message: 'Something went wrong' })).toBe(false);
  });
});

describe('S2a hint window (milliseconds, C64)', () => {
  const PERIOD = 30_000;
  // A period start: 56_666_667 whole periods since the epoch.
  const START = 56_666_667 * PERIOD;

  // Mutants: the window computed in seconds (a seconds-scale expiry is nowhere near
  // these ms values); off by one period.
  it('expires at the end of the period after the one the code fell in', () => {
    expect(totpHintExpiresAt(START + 1)).toBe(START + 2 * PERIOD);
    expect(totpHintExpiresAt(START)).toBe(START + 2 * PERIOD);
    expect(totpHintExpiresAt(START + PERIOD - 1)).toBe(START + 2 * PERIOD);
    expect(totpHintExpiresAt(START + PERIOD)).toBe(START + 3 * PERIOD);
  });

  it('is active 1 ms into the accepting period', () => {
    expect(isTotpHintActive(START + 1, START + 1)).toBe(true);
    expect(isTotpHintActive(START + 1, START + 2)).toBe(true);
  });

  it('is active through the first ms of the next period (not one period short)', () => {
    expect(isTotpHintActive(START + 1, START + PERIOD - 1)).toBe(true);
    expect(isTotpHintActive(START + 1, START + PERIOD)).toBe(true);
    expect(isTotpHintActive(START + 1, START + PERIOD + 1)).toBe(true);
  });

  it('is active in the last ms of the next period', () => {
    expect(isTotpHintActive(START + 1, START + 2 * PERIOD - 1)).toBe(true);
  });

  it('is inactive 1 ms past the next period (not one period long)', () => {
    expect(isTotpHintActive(START + 1, START + 2 * PERIOD)).toBe(false);
    expect(isTotpHintActive(START + 1, START + 2 * PERIOD + 1)).toBe(false);
  });

  it('measures the window from the period the code fell in, not from the moment it was accepted', () => {
    // Accepted in the last ms of its period: the window is still only the next period.
    expect(isTotpHintActive(START + PERIOD - 1, START + 2 * PERIOD - 1)).toBe(true);
    expect(isTotpHintActive(START + PERIOD - 1, START + 2 * PERIOD)).toBe(false);
  });

  it('is inactive when no code was accepted', () => {
    expect(isTotpHintActive(undefined, START)).toBe(false);
  });

  it('is active for an acceptance at the epoch only while now is inside its window', () => {
    expect(isTotpHintActive(0, 2 * PERIOD - 1)).toBe(true);
    expect(isTotpHintActive(0, 2 * PERIOD)).toBe(false);
  });
});
