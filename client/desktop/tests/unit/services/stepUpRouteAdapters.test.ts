import { describe, it, expect } from 'vitest';
import {
  adaptBackupCodeRegenerateRefusal,
  adaptDangerousActionRefusal,
  adaptSessionsRefusal,
} from '@/renderer/services/system/stepUpRouteAdapters';
import type { StepUpFactorRefusal } from '@/renderer/hooks/auth/useStepUpFactor';

// The route adapters read two bodies the seam classifier does not own: the
// session revocations (#7) and backup-code regeneration (#8), plus the
// dangerous-action gates (#3456), which share their 429 and 503 with the gated
// routes' own limiters and so own those two statuses only when flagged. They match the
// server's strings EXACTLY and answer `null` for everything else, so the
// surface's own handling runs instead of a guess (C5).
//
// "Mutant:" comments name the production change each test exists to turn red.

const err = (error: unknown) => ({ error });

describe('adaptSessionsRefusal (#7), one case per exact arm', () => {
  // Mutant: an arm deleted, or mapped to another kind.
  it.each<[string, number, unknown, StepUpFactorRefusal]>([
    ['401 of any body', 401, {}, { kind: 'sessionExpired' }],
    ['401 of a non-object body', 401, 'gateway', { kind: 'sessionExpired' }],
    ['403 auth_required', 403, err('auth_required'), { kind: 'mfaRequired', methods: null }],
    ['403 password_required', 403, err('password_required'), { kind: 'passwordRequired' }],
    ['403 Incorrect password', 403, err('Incorrect password'), { kind: 'invalidPassword' }],
    ['403 Invalid MFA code', 403, err('Invalid MFA code'), { kind: 'invalidMfaCode' }],
    ['429 from the limiter', 429, err('Rate limit exceeded'), { kind: 'rateLimited' }],
    ['429 of a non-object body', 429, 'Too Many Requests', { kind: 'rateLimited' }],
  ])('%s', (_name, status, body, expected) => {
    expect(adaptSessionsRefusal(status, body)).toEqual(expected);
  });

  // Mutant: `auth_required` carrying its body's `methods` (the list is
  // users.mfa_methods, not the inline factors the route accepts, C2).
  it('auth_required never reads the body methods', () => {
    expect(
      adaptSessionsRefusal(403, { error: 'auth_required', methods: ['totp', 'webauthn'] })
    ).toEqual({ kind: 'mfaRequired', methods: null });
    expect(adaptSessionsRefusal(403, { error: 'auth_required', methods: ['email'] })).toEqual({
      kind: 'mfaRequired',
      methods: null,
    });
  });

  // Mutant: the arms matched by `.includes`, `startsWith`, trim, or
  // `toLowerCase`, or a miss falling to a factor error instead of `null`.
  it.each([
    ['a trailing period', 'Incorrect password.'],
    ['a different case', 'incorrect password'],
    ['an upper-cased arm', 'AUTH_REQUIRED'],
    ['a capitalised code', 'Auth_required'],
    ['leading whitespace', ' password_required'],
    ['trailing whitespace', 'Invalid MFA code '],
    ['inner extra whitespace', 'Invalid  MFA code'],
    ['a superstring', 'Invalid MFA code: expired'],
    ['a substring', 'MFA code'],
    ['an unrelated string', 'Something went wrong'],
    ['the empty string', ''],
    ['the sibling route string', 'Invalid TOTP code'],
  ])('403 with %s (%j) is null', (_name, error) => {
    expect(adaptSessionsRefusal(403, err(error))).toBeNull();
  });

  // Mutant: the status guard dropped, so a string under the wrong status
  // becomes a factor error.
  it.each([200, 400, 404, 409, 500, 503])(
    'the right strings under status %i are null',
    (status) => {
      for (const error of [
        'auth_required',
        'password_required',
        'Incorrect password',
        'Invalid MFA code',
      ]) {
        expect(adaptSessionsRefusal(status, err(error))).toBeNull();
      }
    }
  );

  // Mutant: reading `error` off a body that is not a plain object, or
  // throwing on one.
  it.each<[string, unknown]>([
    ['null', null],
    ['undefined', undefined],
    ['a string', 'auth_required'],
    ['a number', 7],
    ['an array', ['auth_required']],
    ['an object with no error', {}],
    ['a non-string error', { error: 7 }],
    ['an object-valued error', { error: { message: 'auth_required' } }],
    ['an array-valued error', { error: ['auth_required'] }],
  ])('403 with %s body is null', (_name, body) => {
    expect(adaptSessionsRefusal(403, body)).toBeNull();
  });
});

describe('adaptBackupCodeRegenerateRefusal (#8), one case per exact arm', () => {
  // Mutant: an arm deleted, mapped to another kind, or given a message it
  // must not carry (the missing-field 400 is `failed` with no text).
  it.each<[string, number, unknown, StepUpFactorRefusal]>([
    ['403 Incorrect password', 403, err('Incorrect password'), { kind: 'invalidPassword' }],
    ['403 Invalid TOTP code', 403, err('Invalid TOTP code'), { kind: 'invalidMfaCode' }],
    [
      '400 Password and TOTP code are required',
      400,
      err('Password and TOTP code are required'),
      { kind: 'failed' },
    ],
    ['429 from the limiter', 429, err('Rate limit exceeded'), { kind: 'rateLimited' }],
  ])('%s', (_name, status, body, expected) => {
    expect(adaptBackupCodeRegenerateRefusal(status, body)).toEqual(expected);
  });

  // TOTP turned off is not a refusal of what was entered, and no in-band
  // sentinel carries it: the stage matches that 400 itself (Q7).
  // Mutant: the adapter mapping it to `failed` (with or without a message).
  it('does not own 400 TOTP is not enabled: it is null', () => {
    expect(adaptBackupCodeRegenerateRefusal(400, err('TOTP is not enabled'))).toBeNull();
  });

  // Mutant: a 401 left unowned, which the stage read as a generic failure
  // while it stayed live under a dead session (picker PR 3 review).
  it('a 401 is the dead session, whatever its body says', () => {
    expect(adaptBackupCodeRegenerateRefusal(401, err('Incorrect password'))).toEqual({
      kind: 'sessionExpired',
    });
    expect(adaptBackupCodeRegenerateRefusal(401, {})).toEqual({ kind: 'sessionExpired' });
  });

  // Mutant: substring, prefix, trim or case-folded matching in either status.
  it.each([
    ['a trailing period', 'Incorrect password.', 403],
    ['a different case', 'incorrect password', 403],
    ['an upper-cased TOTP string', 'INVALID TOTP CODE', 403],
    ['trailing whitespace', 'Invalid TOTP code ', 403],
    ['inner extra whitespace', 'Invalid  TOTP code', 403],
    ['a superstring', 'Invalid TOTP code or expired', 403],
    ['the sessions string', 'Invalid MFA code', 403],
    ['the empty string', '', 403],
    ['a trailing period', 'TOTP is not enabled.', 400],
    ['a different case', 'totp is not enabled', 400],
    ['leading whitespace', ' TOTP is not enabled', 400],
    ['a superstring', 'TOTP is not enabled for this account', 400],
    ['a substring', 'TOTP code are required', 400],
    ['a different case', 'password and totp code are required', 400],
    ['a trailing period', 'Password and TOTP code are required.', 400],
    ['an unrelated string', 'Bad request', 400],
  ])('%s (%j) is null under %i', (_name, error, status) => {
    expect(adaptBackupCodeRegenerateRefusal(status, err(error))).toBeNull();
  });

  // Mutant: the status guards crossed, so a 403 string answers under 400
  // (or the reverse), or any status is accepted.
  it('each string answers only under its own status', () => {
    expect(adaptBackupCodeRegenerateRefusal(400, err('Incorrect password'))).toBeNull();
    expect(adaptBackupCodeRegenerateRefusal(400, err('Invalid TOTP code'))).toBeNull();
    expect(adaptBackupCodeRegenerateRefusal(403, err('TOTP is not enabled'))).toBeNull();
    expect(
      adaptBackupCodeRegenerateRefusal(403, err('Password and TOTP code are required'))
    ).toBeNull();
    for (const status of [200, 404, 409, 500, 503]) {
      for (const error of [
        'Incorrect password',
        'Invalid TOTP code',
        'TOTP is not enabled',
        'Password and TOTP code are required',
      ]) {
        expect(adaptBackupCodeRegenerateRefusal(status, err(error))).toBeNull();
      }
    }
  });

  // Mutant: reading `error` off a non-object body, or throwing on one.
  it.each<[string, unknown]>([
    ['null', null],
    ['undefined', undefined],
    ['a string', 'Incorrect password'],
    ['a number', 7],
    ['an array', ['Incorrect password']],
    ['an object with no error', {}],
    ['a non-string error', { error: 7 }],
    ['an object-valued error', { error: { message: 'Incorrect password' } }],
  ])('a %s body is null under both statuses', (_name, body) => {
    expect(adaptBackupCodeRegenerateRefusal(403, body)).toBeNull();
    expect(adaptBackupCodeRegenerateRefusal(400, body)).toBeNull();
  });
});

describe('adaptDangerousActionRefusal (#3456), one case per arm', () => {
  // The dialog's `none` leg renders no password field, so a password refusal
  // there would raise no alert at all. No D1 gate reads a password (V18), and
  // this pins that the adapter never hands one on.
  // Mutant: `passwordRequired` or `invalidPassword` added to the adapter's 403 arm.
  it.each<[string, unknown]>([
    ['password_required', { error: 'Password required', password_required: true }],
    [
      'an expired step-up token',
      { error: 'Password required', password_required: true, step_up_token_invalid: true },
    ],
    ['Invalid password', { error: 'Invalid password' }],
  ])('never hands on a password refusal (%s)', (_name, body) => {
    expect(adaptDangerousActionRefusal(403, body)).toBeNull();
  });

  // Mutant: an arm deleted, or mapped to another kind.
  it.each<[string, number, unknown, StepUpFactorRefusal]>([
    [
      '429 flagged step_up_budget_exhausted',
      429,
      { step_up_budget_exhausted: true },
      { kind: 'rateLimited' },
    ],
    [
      '503 flagged step_up_budget_unavailable',
      503,
      { step_up_budget_unavailable: true },
      { kind: 'unavailable' },
    ],
    ['503 flagged lock_conflict', 503, { lock_conflict: true }, { kind: 'unavailable' }],
    [
      '403 mfa_required with methods',
      403,
      { mfa_required: true, methods: ['totp', 'webauthn'] },
      { kind: 'mfaRequired', methods: ['totp', 'webauthn'] },
    ],
    [
      '403 mfa_enrollment_required',
      403,
      { mfa_enrollment_required: true },
      { kind: 'enrollmentRequired' },
    ],
    ['403 Invalid MFA code', 403, err('Invalid MFA code'), { kind: 'invalidMfaCode' }],
    ['401 of any body', 401, {}, { kind: 'sessionExpired' }],
    ['401 of a non-object body', 401, 'gateway', { kind: 'sessionExpired' }],
    ['401 of a null body', 401, null, { kind: 'sessionExpired' }],
  ])('%s', (_name, status, body, expected) => {
    expect(adaptDangerousActionRefusal(status, body)).toEqual(expected);
  });

  // Mutant: the 429 owned without its flag, so the gated route's own limiter
  // reads as a gate refusal that spent nothing.
  it.each<[string, unknown]>([
    ['an unflagged limiter body', err('Rate limit exceeded')],
    ['an empty object', {}],
    ['the flag false', { step_up_budget_exhausted: false }],
    ['the flag as the string "true"', { step_up_budget_exhausted: 'true' }],
    ['the flag as 1', { step_up_budget_exhausted: 1 }],
    ['a sibling 503 flag', { step_up_budget_unavailable: true }],
    ['a lock_conflict flag', { lock_conflict: true }],
    ['a null body', null],
    ['a string body', 'Too Many Requests'],
    ['an array body', [true]],
  ])('429 with %s is null', (_name, body) => {
    expect(adaptDangerousActionRefusal(429, body)).toBeNull();
  });

  // Mutant: the 503 owned without a flag, `||` turned into `&&`, or either
  // flag read loosely (truthy instead of `=== true`).
  it.each<[string, unknown]>([
    ['an unflagged failure body', err('Service unavailable')],
    ['an empty object', {}],
    ['step_up_budget_unavailable false', { step_up_budget_unavailable: false }],
    ['step_up_budget_unavailable "true"', { step_up_budget_unavailable: 'true' }],
    ['lock_conflict false', { lock_conflict: false }],
    ['lock_conflict "true"', { lock_conflict: 'true' }],
    ['lock_conflict 1', { lock_conflict: 1 }],
    ['both flags false', { step_up_budget_unavailable: false, lock_conflict: false }],
    ['a sibling 429 flag', { step_up_budget_exhausted: true }],
    ['a null body', null],
    ['a string body', 'Bad Gateway'],
    ['an array body', [true]],
  ])('503 with %s is null', (_name, body) => {
    expect(adaptDangerousActionRefusal(503, body)).toBeNull();
  });

  // Mutant: `||` turned into `&&` on the 503 flags.
  it('503 with either flag alone, or both, is unavailable', () => {
    expect(
      adaptDangerousActionRefusal(503, { step_up_budget_unavailable: true, lock_conflict: false })
    ).toEqual({ kind: 'unavailable' });
    expect(
      adaptDangerousActionRefusal(503, { step_up_budget_unavailable: false, lock_conflict: true })
    ).toEqual({ kind: 'unavailable' });
    expect(
      adaptDangerousActionRefusal(503, { step_up_budget_unavailable: true, lock_conflict: true })
    ).toEqual({ kind: 'unavailable' });
  });

  // Mutant: the 403 branch reading the delete-rate-limited flag itself, or
  // dropping the classifier's flag-first order.
  it('403 mfa_enrollment_required is enrollmentRequired with or without delete_rate_limited', () => {
    expect(adaptDangerousActionRefusal(403, { mfa_enrollment_required: true })).toEqual({
      kind: 'enrollmentRequired',
    });
    expect(
      adaptDangerousActionRefusal(403, { mfa_enrollment_required: true, delete_rate_limited: true })
    ).toEqual({ kind: 'enrollmentRequired' });
    // The enrollment flag wins over a co-present mfa_required.
    expect(
      adaptDangerousActionRefusal(403, {
        mfa_enrollment_required: true,
        mfa_required: true,
        delete_rate_limited: true,
        methods: ['totp'],
      })
    ).toEqual({ kind: 'enrollmentRequired' });
  });

  // Mutant: the methods list defaulted to null or passed through unfiltered.
  it('403 mfa_required keeps only the string methods and defaults a missing list to []', () => {
    expect(adaptDangerousActionRefusal(403, { mfa_required: true })).toEqual({
      kind: 'mfaRequired',
      methods: [],
    });
    expect(
      adaptDangerousActionRefusal(403, { mfa_required: true, methods: ['totp', 7, null] })
    ).toEqual({ kind: 'mfaRequired', methods: ['totp'] });
    expect(adaptDangerousActionRefusal(403, { mfa_required: true, methods: 'totp' })).toEqual({
      kind: 'mfaRequired',
      methods: [],
    });
  });

  // Mutant: dangerousActionForbidden's default arm returning the refusal, so
  // the soft-lock's own 403 or a password kind leaks to a D1 dialog.
  it.each<[string, unknown]>([
    [
      'delete_rate_limited with mfa_required (the soft-lock, a distinct kind)',
      { delete_rate_limited: true, mfa_required: true, methods: ['totp'] },
    ],
    ['delete_rate_limited alone', { delete_rate_limited: true }],
    ['password_required', { password_required: true }],
    [
      'password_required with a spent token',
      { password_required: true, step_up_token_invalid: true },
    ],
    ['Invalid password', err('Invalid password')],
    ['an unknown error string', err('Forbidden')],
    ['an empty object', {}],
    ['a null body', null],
    ['a string body', 'Forbidden'],
    ['an array body', [{ mfa_required: true }]],
  ])('403 %s is null', (_name, body) => {
    expect(adaptDangerousActionRefusal(403, body)).toBeNull();
  });

  // Mutant: a flag read loosely (truthy or string-compared), or the exact
  // 'Invalid MFA code' string widened.
  it.each<[string, unknown]>([
    ['mfa_required "true"', { mfa_required: 'true' }],
    ['mfa_required false', { mfa_required: false }],
    ['mfa_required 1', { mfa_required: 1 }],
    ['mfa_enrollment_required "true"', { mfa_enrollment_required: 'true' }],
    ['mfa_enrollment_required 1', { mfa_enrollment_required: 1 }],
    ['mfa_enrollment_required false', { mfa_enrollment_required: false }],
    ['Invalid MFA code with a trailing period', err('Invalid MFA code.')],
    ['invalid mfa code in lower case', err('invalid mfa code')],
    ['Invalid TOTP code (the #8 string)', err('Invalid TOTP code')],
  ])('403 with %s is null', (_name, body) => {
    expect(adaptDangerousActionRefusal(403, body)).toBeNull();
  });

  // Mutant: the status guards dropped or crossed, so a flag or a seam body
  // answers under a status that does not own it. 409 carries the classifier's
  // inline_factor_required arm, which a D1 gate never writes.
  it.each([200, 204, 400, 404, 409, 500, 502, 504])(
    'every body is null under status %i',
    (status) => {
      const bodies: unknown[] = [
        { step_up_budget_exhausted: true },
        { step_up_budget_unavailable: true },
        { lock_conflict: true },
        { mfa_required: true, methods: ['totp'] },
        { mfa_enrollment_required: true },
        { inline_factor_required: true, error: 'Turn off email codes' },
        err('Invalid MFA code'),
        null,
      ];
      for (const body of bodies) {
        expect(adaptDangerousActionRefusal(status, body)).toBeNull();
      }
    }
  );

  // Mutant: the 401 arm left unowned, or gated on the body.
  it('a 401 is the dead session whatever flags the body carries', () => {
    expect(
      adaptDangerousActionRefusal(401, { step_up_budget_exhausted: true, mfa_required: true })
    ).toEqual({ kind: 'sessionExpired' });
    expect(adaptDangerousActionRefusal(401, undefined)).toEqual({ kind: 'sessionExpired' });
  });

  // Mutant: the flag read off an inherited or non-own member, or the helper
  // throwing on a primitive body.
  it('does not throw on primitive bodies under any owned status', () => {
    for (const status of [401, 403, 429, 503]) {
      for (const body of [undefined, null, 0, 'x', true]) {
        expect(() => adaptDangerousActionRefusal(status, body)).not.toThrow();
      }
    }
  });
});
