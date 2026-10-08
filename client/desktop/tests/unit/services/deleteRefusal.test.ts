import { describe, it, expect } from 'vitest';
import {
  mintRefusalView,
  softLockSeed,
  toDeleteRefusalView,
  toDeleteSubmitOutcome,
  type DeleteRefusalView,
} from '@/renderer/services/messaging/deleteRefusal';
import type { PasswordStepUpMint } from '@/renderer/services/system/stepUpToken';

// The one reader of a refused single-message DELETE (#3455 §2.8, §2.10, D-1).
// Every branch routes by a machine-readable flag or an exact frozen string;
// each case asserts the whole view so a field cannot silently appear or vanish.

const BUDGET_STRING = 'Too many verification attempts';
const RETRY_COPY = "That didn't work. Try again with a new code.";

const CONFIRM_BODY = {
  error: 'Confirm it is you',
  delete_rate_limited: true,
  mfa_required: true,
  methods: ['totp', 'webauthn'],
};

describe('toDeleteRefusalView — 429', () => {
  it('reads the budget flag as a verification wait', () => {
    expect(toDeleteRefusalView(429, { step_up_budget_exhausted: true }, '30')).toEqual({
      view: 'wait',
      reason: 'verification',
      retryAfterSeconds: 30,
    });
  });

  it('reads the exact frozen budget string as a verification wait when the flag is absent', () => {
    expect(toDeleteRefusalView(429, { error: BUDGET_STRING }, '30')).toEqual({
      view: 'wait',
      reason: 'verification',
      retryAfterSeconds: 30,
    });
  });

  it('trims the budget string before comparing', () => {
    expect(toDeleteRefusalView(429, { error: `  ${BUDGET_STRING}  ` }, null)).toEqual({
      view: 'wait',
      reason: 'verification',
      retryAfterSeconds: undefined,
    });
  });

  it('a plain route-limiter 429 is a requests wait', () => {
    expect(toDeleteRefusalView(429, { error: 'Rate limit exceeded' }, '12')).toEqual({
      view: 'wait',
      reason: 'requests',
      retryAfterSeconds: 12,
    });
  });

  it('never widens the budget string to a substring match', () => {
    expect(
      toDeleteRefusalView(429, { error: `${BUDGET_STRING} for this route, slow down` }, null)
    ).toMatchObject({ view: 'wait', reason: 'requests' });
  });

  it('matches the budget flag strictly: a truthy non-boolean is not the flag', () => {
    expect(toDeleteRefusalView(429, { step_up_budget_exhausted: 'true' }, null)).toMatchObject({
      reason: 'requests',
    });
  });

  it.each([null, undefined, 'not json', 42, []])(
    'an unreadable body (%j) is a requests wait',
    (body) => {
      expect(toDeleteRefusalView(429, body, '5')).toEqual({
        view: 'wait',
        reason: 'requests',
        retryAfterSeconds: 5,
      });
    }
  );
});

describe('toDeleteRefusalView — 503', () => {
  it.each([
    ['soft-lock unavailable', { error: 'Soft-lock unavailable' }],
    ['budget unavailable', { error: 'Service unavailable' }],
    ['lock conflict', { error: 'lock_conflict', code: 'lock_conflict' }],
    ['an empty body', {}],
    ['a proxy page', 'not json'],
  ])('%s → unavailable, with no countdown', (_, body) => {
    expect(toDeleteRefusalView(503, body, '30')).toEqual({ view: 'unavailable' });
  });
});

describe('toDeleteRefusalView — 403', () => {
  it('delete_rate_limited + mfa_required → confirm with the offered methods', () => {
    expect(toDeleteRefusalView(403, CONFIRM_BODY, null)).toEqual({
      view: 'confirm',
      methods: ['totp', 'webauthn'],
    });
  });

  it('a bare mfa_required (no delete_rate_limited) still opens the confirm view', () => {
    expect(
      toDeleteRefusalView(
        403,
        { error: 'MFA required', mfa_required: true, methods: ['totp'] },
        null
      )
    ).toEqual({ view: 'confirm', methods: ['totp'] });
  });

  it('delete_rate_limited + password_required → password', () => {
    expect(
      toDeleteRefusalView(
        403,
        { error: 'Password required', delete_rate_limited: true, password_required: true },
        null
      )
    ).toEqual({ view: 'password' });
  });

  // The credential stage words a refused password or code in place from the
  // hook's outcome, so these views carry no copy of their own (picker PR 3).
  it('Invalid password (exact) → the bare password view, with no copy of its own', () => {
    expect(toDeleteRefusalView(403, { error: 'Invalid password' }, null)).toEqual({
      view: 'password',
    });
  });

  it('Invalid password keeps the password view even after a confirm view', () => {
    const prior: DeleteRefusalView = { view: 'confirm', methods: ['totp'] };
    expect(toDeleteRefusalView(403, { error: 'Invalid password' }, null, prior)).toEqual({
      view: 'password',
    });
  });

  it('a spent step-up token (step_up_token_invalid) is the same bare password view', () => {
    expect(
      toDeleteRefusalView(
        403,
        { error: 'Password required', password_required: true, step_up_token_invalid: true },
        null
      )
    ).toEqual({ view: 'password' });
  });

  it('Invalid MFA code after a confirm view stays on confirm, keeping its methods and no error', () => {
    const prior: DeleteRefusalView = { view: 'confirm', methods: ['totp', 'webauthn'] };
    const view = toDeleteRefusalView(403, { error: 'Invalid MFA code' }, null, prior);
    expect(view).toEqual({ view: 'confirm', methods: ['totp', 'webauthn'] });
    expect(view).not.toHaveProperty('error');
  });

  it('Invalid MFA code with no prior view falls back to a failure carrying the retry copy', () => {
    expect(toDeleteRefusalView(403, { error: 'Invalid MFA code' }, null)).toEqual({
      view: 'failed',
      message: RETRY_COPY,
    });
  });

  it('Invalid MFA code after a NON-confirm prior view is a failure, not a confirm', () => {
    const prior: DeleteRefusalView = { view: 'password' };
    expect(toDeleteRefusalView(403, { error: 'Invalid MFA code' }, null, prior)).toEqual({
      view: 'failed',
      message: RETRY_COPY,
    });
  });

  // E8 / D5: the server's enrolment body is `mfa_enrollment_required: true`
  // (stepup.go:65, :342-346). The soft-lock adds `delete_rate_limited` and a
  // `Retry-After` to it; neither may turn the view into a retry loop.
  describe('mfa_enrollment_required → the terminal enroll view', () => {
    const SERVER_TEXT = 'Set up an authenticator app or security key to do this.';

    it('without delete_rate_limited', () => {
      expect(
        toDeleteRefusalView(403, { error: SERVER_TEXT, mfa_enrollment_required: true }, null)
      ).toEqual({ view: 'enroll' });
    });

    it('with delete_rate_limited and a Retry-After: no countdown, no failed view', () => {
      const view = toDeleteRefusalView(
        403,
        { error: SERVER_TEXT, delete_rate_limited: true, mfa_enrollment_required: true },
        '60'
      );
      expect(view).toEqual({ view: 'enroll' });
      expect(view).not.toHaveProperty('retryAfterSeconds');
      expect(view).not.toHaveProperty('message');
    });

    it('wins over a body that also carries mfa_required and a password flag', () => {
      expect(
        toDeleteRefusalView(
          403,
          {
            delete_rate_limited: true,
            mfa_required: true,
            password_required: true,
            methods: ['totp'],
            mfa_enrollment_required: true,
          },
          null
        )
      ).toEqual({ view: 'enroll' });
    });

    it.each<[string, DeleteRefusalView | undefined]>([
      ['no prior view', undefined],
      ['a prior confirm view', { view: 'confirm', methods: ['totp'] }],
      ['a prior password view', { view: 'password' }],
    ])('is the same with %s', (_label, prior) => {
      expect(toDeleteRefusalView(403, { mfa_enrollment_required: true }, null, prior)).toEqual({
        view: 'enroll',
      });
    });

    it('is matched strictly: a truthy non-boolean is not the flag', () => {
      expect(
        toDeleteRefusalView(403, { error: SERVER_TEXT, mfa_enrollment_required: 'true' }, null)
      ).toEqual({ view: 'failed', message: SERVER_TEXT, retryAfterSeconds: undefined });
    });

    it('the retired body shape (a `code` field) is not the flag', () => {
      expect(
        toDeleteRefusalView(403, { error: SERVER_TEXT, code: 'mfa_enrollment_required' }, null)
      ).toEqual({ view: 'failed', message: SERVER_TEXT, retryAfterSeconds: undefined });
    });
  });

  it('a 403 with an unrecognised body → failed, carrying Retry-After when present', () => {
    expect(toDeleteRefusalView(403, { error: 'Forbidden' }, '7')).toEqual({
      view: 'failed',
      message: 'Forbidden',
      retryAfterSeconds: 7,
    });
  });

  it('a 403 with an unreadable body → failed with no message', () => {
    expect(toDeleteRefusalView(403, null, null)).toEqual({
      view: 'failed',
      message: undefined,
      retryAfterSeconds: undefined,
    });
  });
});

describe('toDeleteRefusalView — every other status', () => {
  it.each([401, 404, 409, 500, 502])('%i → failed with the server text', (status) => {
    expect(toDeleteRefusalView(status, { error: '  Something broke  ' }, null)).toEqual({
      view: 'failed',
      message: 'Something broke',
      retryAfterSeconds: undefined,
    });
  });

  it.each([
    ['a blank error', { error: '   ' }],
    ['a non-string error', { error: 42 }],
    ['a null body', null],
    ['an array body', ['nope']],
    ['a string body', 'nope'],
  ])('%s → failed with no message', (_, body) => {
    expect(toDeleteRefusalView(500, body, null)).toEqual({
      view: 'failed',
      message: undefined,
      retryAfterSeconds: undefined,
    });
  });

  it('carries a countdown only when the header is present', () => {
    expect(toDeleteRefusalView(500, {}, '45')).toMatchObject({ retryAfterSeconds: 45 });
    expect(toDeleteRefusalView(500, {}, null)).toMatchObject({ retryAfterSeconds: undefined });
  });
});

describe('toDeleteRefusalView — Retry-After parsing', () => {
  it('reads an integer', () => {
    expect(toDeleteRefusalView(429, {}, '120')).toMatchObject({ retryAfterSeconds: 120 });
  });

  it('accepts zero', () => {
    expect(toDeleteRefusalView(429, {}, '0')).toMatchObject({ retryAfterSeconds: 0 });
  });

  it('an HTTP-date is undefined, not NaN', () => {
    const view = toDeleteRefusalView(429, {}, 'Wed, 21 Oct 2026 07:28:00 GMT');
    expect(view).toEqual({ view: 'wait', reason: 'requests', retryAfterSeconds: undefined });
  });

  it('a negative value is undefined', () => {
    expect(toDeleteRefusalView(429, {}, '-5')).toMatchObject({ retryAfterSeconds: undefined });
  });

  it('an empty header is undefined', () => {
    expect(toDeleteRefusalView(429, {}, '')).toMatchObject({ retryAfterSeconds: undefined });
  });

  it('an absent header is undefined', () => {
    expect(toDeleteRefusalView(429, {}, null)).toMatchObject({ retryAfterSeconds: undefined });
  });

  it('is honoured on the failed view too', () => {
    expect(toDeleteRefusalView(500, {}, 'Wed, 21 Oct 2026 07:28:00 GMT')).toMatchObject({
      view: 'failed',
      retryAfterSeconds: undefined,
    });
  });
});

// The factor hook's reading of the same refusal (D6). The hook knows no
// delete-specific kind, so the soft-lock's `deleteRateLimited` is its
// `mfaRequired`; a refusal no credential can answer is `answered`.
describe('toDeleteSubmitOutcome', () => {
  it('delete_rate_limited + mfa_required → the hook’s mfaRequired with the methods', () => {
    expect(toDeleteSubmitOutcome(403, CONFIRM_BODY)).toEqual({
      kind: 'refusal',
      refusal: { kind: 'mfaRequired', methods: ['totp', 'webauthn'] },
    });
  });

  it('a bare mfa_required is the same kind', () => {
    expect(toDeleteSubmitOutcome(403, { mfa_required: true, methods: ['totp'] })).toEqual({
      kind: 'refusal',
      refusal: { kind: 'mfaRequired', methods: ['totp'] },
    });
  });

  it.each([
    ['password_required', { password_required: true }, { kind: 'passwordRequired' }],
    [
      'a spent token',
      { password_required: true, step_up_token_invalid: true },
      { kind: 'passwordRequired', tokenExpired: true },
    ],
    ['Invalid password', { error: 'Invalid password' }, { kind: 'invalidPassword' }],
    ['Invalid MFA code', { error: 'Invalid MFA code' }, { kind: 'invalidMfaCode' }],
  ])('%s → a refusal the hook words in place', (_label, body, refusal) => {
    expect(toDeleteSubmitOutcome(403, body)).toEqual({ kind: 'refusal', refusal });
  });

  // The #17 loop: a Retry or a countdown here would invite an attempt no input
  // can complete. The hook must see the terminal kind, with or without the flag.
  it.each([
    ['without delete_rate_limited', { mfa_enrollment_required: true }],
    [
      'with delete_rate_limited',
      { delete_rate_limited: true, mfa_enrollment_required: true, error: 'Set up an app' },
    ],
  ])('enrolment %s → the terminal enrollmentRequired refusal', (_label, body) => {
    expect(toDeleteSubmitOutcome(403, body)).toEqual({
      kind: 'refusal',
      refusal: { kind: 'enrollmentRequired' },
    });
  });

  it.each([
    ['an unrecognised 403', 403, { error: 'Forbidden' }],
    ['a 403 with an unreadable body', 403, null],
    ['a 500', 500, { error: 'boom' }],
    ['a 404', 404, {}],
    ['a 503', 503, {}],
    ['a 409 inline-factor-required', 409, { inline_factor_required: true }],
  ])('%s → answered: no credential can resolve it', (_label, status, body) => {
    expect(toDeleteSubmitOutcome(status, body)).toEqual({ kind: 'answered' });
  });
});

// D7: a mint that names its methods moves to the code prompt; any other refusal
// is the password field's copy. `unsent` is the caller's to handle first.
describe('mintRefusalView', () => {
  const refused = (
    reason: Extract<PasswordStepUpMint, { kind: 'refused' }>['reason'],
    extra: Partial<Extract<PasswordStepUpMint, { kind: 'refused' }>> = {}
  ): Extract<PasswordStepUpMint, { kind: 'refused' }> => ({ kind: 'refused', reason, ...extra });

  it('a mint naming its methods is the confirm view', () => {
    expect(mintRefusalView(refused('mfaRequired', { methods: ['totp', 'webauthn'] }))).toEqual({
      view: 'confirm',
      methods: ['totp', 'webauthn'],
    });
  });

  it.each([
    ['names no methods', refused('mfaRequired')],
    ['names an empty list', refused('mfaRequired', { methods: [] })],
  ])('an mfaRequired mint that %s is the password view with the fallback copy', (_l, mint) => {
    expect(mintRefusalView(mint)).toEqual({
      view: 'password',
      error:
        'This account now confirms with an authenticator app or security key. Close this and try again.',
    });
  });

  it('methods on a mint that is not mfaRequired are ignored', () => {
    expect(mintRefusalView(refused('failed', { methods: ['totp'] }))).toEqual({
      view: 'password',
      error: "We couldn't check your password. Try again.",
    });
  });

  it.each([
    ['invalidPassword', undefined, 'That password is not correct.'],
    ['tooManyAttempts', 60, 'Too many attempts. Try again in 60 seconds.'],
    ['tooManyAttempts', undefined, 'Too many attempts. Try again later.'],
    ['unsupported', undefined, "This server doesn't support this confirmation yet."],
    ['failed', undefined, "We couldn't check your password. Try again."],
  ] as const)(
    '%s (%s) → the password view carrying its copy',
    (reason, retryAfterSeconds, error) => {
      expect(mintRefusalView(refused(reason, { retryAfterSeconds }))).toEqual({
        view: 'password',
        error,
      });
    }
  );
});

// G2: the hook's seed for the view that opened the dialog.
describe('softLockSeed', () => {
  it('confirm seeds the mfaRequired its methods came from, never nothing', () => {
    // A deleteRateLimited seed would seed no factors at all.
    expect(softLockSeed({ view: 'confirm', methods: ['totp', 'webauthn'] })).toEqual({
      kind: 'mfaRequired',
      methods: ['totp', 'webauthn'],
    });
  });

  it('a confirm view naming no methods still seeds mfaRequired, with the empty list', () => {
    expect(softLockSeed({ view: 'confirm', methods: [] })).toEqual({
      kind: 'mfaRequired',
      methods: [],
    });
  });

  it('password seeds passwordRequired, whatever copy it carries', () => {
    expect(softLockSeed({ view: 'password' })).toEqual({ kind: 'passwordRequired' });
    expect(softLockSeed({ view: 'password', error: 'That password is not correct.' })).toEqual({
      kind: 'passwordRequired',
    });
  });

  it('enrolment seeds the terminal enrollmentRequired', () => {
    expect(softLockSeed({ view: 'enroll' })).toEqual({ kind: 'enrollmentRequired' });
  });

  it.each<DeleteRefusalView>([
    { view: 'wait', reason: 'requests', retryAfterSeconds: 5 },
    { view: 'wait', reason: 'verification' },
    { view: 'unavailable' },
    { view: 'failed', message: 'x', retryAfterSeconds: 1 },
  ])('the Close-only view %j hosts no credential, so it seeds nothing', (view) => {
    expect(softLockSeed(view)).toBeNull();
  });

  it('round-trips the wire: the soft-lock’s own confirm 403 seeds its methods', () => {
    const view = toDeleteRefusalView(403, CONFIRM_BODY, null);
    expect(softLockSeed(view)).toEqual({ kind: 'mfaRequired', methods: ['totp', 'webauthn'] });
  });

  it('round-trips the wire: an enrolment 403 seeds enrollmentRequired, with the flag present', () => {
    const view = toDeleteRefusalView(
      403,
      { delete_rate_limited: true, mfa_enrollment_required: true },
      '60'
    );
    expect(softLockSeed(view)).toEqual({ kind: 'enrollmentRequired' });
  });
});
