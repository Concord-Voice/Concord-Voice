import { describe, it, expect } from 'vitest';
import {
  adaptBackupCodeRegenerateRefusal,
  adaptSessionsRefusal,
} from '@/renderer/services/system/stepUpRouteAdapters';
import type { StepUpFactorRefusal } from '@/renderer/hooks/auth/useStepUpFactor';

// The route adapters read two bodies the seam classifier does not own: the
// session revocations (#7) and backup-code regeneration (#8). They match the
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
