import { describe, it, expect } from 'vitest';
import {
  classifyStepUpRefusal,
  INLINE_FACTOR_REQUIRED_MESSAGE,
  isStepUpFactorRefusal,
} from '@/renderer/services/system/stepUpRefusal';

// The one reader of the internal/stepup refusal bodies. Every status in the
// wire contract gets a case, and each case asserts the whole result so a
// consumer's union cannot silently gain or lose a field.

describe('classifyStepUpRefusal — the wire contract, one case per row', () => {
  it('403 password_required → passwordRequired', () => {
    expect(
      classifyStepUpRefusal(403, { error: 'Update Concord Voice.', password_required: true })
    ).toEqual({ kind: 'passwordRequired' });
  });

  it('403 Invalid password (exact) → invalidPassword', () => {
    expect(classifyStepUpRefusal(403, { error: 'Invalid password' })).toEqual({
      kind: 'invalidPassword',
    });
  });

  it('403 mfa_required → mfaRequired, keeping only string methods', () => {
    expect(
      classifyStepUpRefusal(403, {
        error: 'MFA verification required',
        mfa_required: true,
        methods: ['totp', 7, 'webauthn'],
      })
    ).toEqual({ kind: 'mfaRequired', methods: ['totp', 'webauthn'] });
  });

  it('403 Invalid MFA code (exact) → invalidMfaCode', () => {
    expect(classifyStepUpRefusal(403, { error: 'Invalid MFA code' })).toEqual({
      kind: 'invalidMfaCode',
    });
  });

  it('flags are matched strictly: a truthy non-boolean is not the flag', () => {
    // pragma: allowlist nextline secret
    expect(classifyStepUpRefusal(403, { password_required: 'yes', error: 'x' })).toEqual({
      kind: 'failed',
      message: 'x',
    });
  });

  it('429 → rateLimited', () => {
    expect(classifyStepUpRefusal(429, { error: 'Too many verification attempts' })).toEqual({
      kind: 'rateLimited',
    });
  });

  it('401 → sessionExpired', () => {
    expect(classifyStepUpRefusal(401, {})).toEqual({ kind: 'sessionExpired' });
  });

  it('503 → unavailable', () => {
    expect(classifyStepUpRefusal(503, { error: 'whatever a proxy says' })).toEqual({
      kind: 'unavailable',
    });
  });

  it('409 inline_factor_required → inlineFactorRequired with the server text', () => {
    expect(
      classifyStepUpRefusal(409, { error: 'server copy', inline_factor_required: true })
    ).toEqual({ kind: 'inlineFactorRequired', message: 'server copy' });
  });

  it('409 inline_factor_required with no text falls back to the contract copy', () => {
    expect(classifyStepUpRefusal(409, { inline_factor_required: true })).toEqual({
      kind: 'inlineFactorRequired',
      message: INLINE_FACTOR_REQUIRED_MESSAGE,
    });
  });

  it('a 409 without the flag is an ordinary failure', () => {
    expect(classifyStepUpRefusal(409, { error: 'conflict' })).toEqual({
      kind: 'failed',
      message: 'conflict',
    });
  });

  it('500 → failed carrying the trimmed server text', () => {
    expect(classifyStepUpRefusal(500, { error: '  Verification failed  ' })).toEqual({
      kind: 'failed',
      message: 'Verification failed',
    });
  });

  it('a body that is not an object is never trusted', () => {
    expect(classifyStepUpRefusal(403, null)).toEqual({ kind: 'failed', message: undefined });
    expect(classifyStepUpRefusal(400, 'Invalid password')).toEqual({
      kind: 'failed',
      message: undefined,
    });
    expect(classifyStepUpRefusal(403, { error: 42 })).toEqual({
      kind: 'failed',
      message: undefined,
    });
  });
});

describe('isStepUpFactorRefusal', () => {
  it('is true only for the four field-owned kinds', () => {
    expect(isStepUpFactorRefusal({ kind: 'passwordRequired' })).toBe(true);
    expect(isStepUpFactorRefusal({ kind: 'mfaRequired', methods: [] })).toBe(true);
    expect(isStepUpFactorRefusal({ kind: 'invalidPassword' })).toBe(true);
    expect(isStepUpFactorRefusal({ kind: 'invalidMfaCode' })).toBe(true);
    expect(isStepUpFactorRefusal({ kind: 'rateLimited' })).toBe(false);
    expect(isStepUpFactorRefusal({ kind: 'failed' })).toBe(false);
  });
});
