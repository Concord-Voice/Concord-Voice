import { describe, it, expect } from 'vitest';
import {
  toDeleteRefusalView,
  type DeleteRefusalView,
} from '@/renderer/services/messaging/deleteRefusal';

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

  it('Invalid password (exact) → password with a per-field error', () => {
    expect(toDeleteRefusalView(403, { error: 'Invalid password' }, null)).toEqual({
      view: 'password',
      error: 'That password is not correct.',
    });
  });

  it('Invalid password keeps the password view even after a confirm view', () => {
    const prior: DeleteRefusalView = { view: 'confirm', methods: ['totp'] };
    expect(toDeleteRefusalView(403, { error: 'Invalid password' }, null, prior)).toEqual({
      view: 'password',
      error: 'That password is not correct.',
    });
  });

  it('Invalid MFA code after a confirm view stays on confirm, keeping its methods', () => {
    const prior: DeleteRefusalView = { view: 'confirm', methods: ['totp', 'webauthn'] };
    expect(toDeleteRefusalView(403, { error: 'Invalid MFA code' }, null, prior)).toEqual({
      view: 'confirm',
      methods: ['totp', 'webauthn'],
      error: RETRY_COPY,
    });
  });

  it('Invalid MFA code with no prior view falls back to a failure carrying the same copy', () => {
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

  it('mfa_enrollment_required → failed with the server text and no countdown when absent', () => {
    expect(
      toDeleteRefusalView(
        403,
        {
          error: 'Set up an authenticator app or security key to do this.',
          code: 'mfa_enrollment_required',
        },
        null
      )
    ).toEqual({
      view: 'failed',
      message: 'Set up an authenticator app or security key to do this.',
      retryAfterSeconds: undefined,
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
