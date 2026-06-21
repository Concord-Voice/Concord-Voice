import { describe, it, expect } from 'vitest';
import {
  E2EEKeyUnavailableError,
  isPendingKeyError,
  classifyError,
  type E2EEKeyErrorCode,
} from '@/renderer/services/e2eeErrors';

describe('E2EEKeyUnavailableError', () => {
  it('carries code and pending flag', () => {
    const err = new E2EEKeyUnavailableError('NO_KEY_YET', true);
    expect(err.code).toBe('NO_KEY_YET');
    expect(err.pending).toBe(true);
    expect(err.name).toBe('E2EEKeyUnavailableError');
    expect(err.message).toContain('NO_KEY_YET');
  });

  it('defaults pending to false when omitted', () => {
    const err = new E2EEKeyUnavailableError('NOT_MEMBER');
    expect(err.pending).toBe(false);
  });

  it('is an instance of Error', () => {
    const err = new E2EEKeyUnavailableError('REVOKED_EPOCH');
    expect(err).toBeInstanceOf(Error);
    expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
  });

  it.each<E2EEKeyErrorCode>([
    'NOT_MEMBER',
    'NO_KEY_YET',
    'REVOKED_EPOCH',
    'INVALID_REQUEST',
    'INTERNAL_ERROR',
    'MALFORMED_PAYLOAD',
  ])('accepts the %s code', (code) => {
    const err = new E2EEKeyUnavailableError(code);
    expect(err.code).toBe(code);
  });
});

describe('isPendingKeyError', () => {
  it('returns true for NO_KEY_YET with pending=true', () => {
    const err = new E2EEKeyUnavailableError('NO_KEY_YET', true);
    expect(isPendingKeyError(err)).toBe(true);
  });

  it('returns false for NO_KEY_YET with pending=false', () => {
    const err = new E2EEKeyUnavailableError('NO_KEY_YET', false);
    expect(isPendingKeyError(err)).toBe(false);
  });

  it.each([
    'NOT_MEMBER',
    'REVOKED_EPOCH',
    'INVALID_REQUEST',
    'INTERNAL_ERROR',
    'MALFORMED_PAYLOAD',
  ] as const)('returns false for %s regardless of pending flag', (code) => {
    const err = new E2EEKeyUnavailableError(code, true);
    expect(isPendingKeyError(err)).toBe(false);
  });

  it('returns false for generic Error', () => {
    expect(isPendingKeyError(new Error('PENDING_KEY'))).toBe(false);
  });

  it('returns false for non-Error values', () => {
    expect(isPendingKeyError(undefined)).toBe(false);
    expect(isPendingKeyError(null)).toBe(false);
    expect(isPendingKeyError('PENDING_KEY')).toBe(false);
    expect(isPendingKeyError({ code: 'NO_KEY_YET', pending: true })).toBe(false);
  });
});

describe('classifyError', () => {
  it('retries NO_KEY_YET when pending=true', () => {
    const c = classifyError(new E2EEKeyUnavailableError('NO_KEY_YET', true));
    expect(c.retryable).toBe(true);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toContain('retrying');
  });

  it('does not retry NO_KEY_YET when pending=false (rate-limited)', () => {
    const c = classifyError(new E2EEKeyUnavailableError('NO_KEY_YET', false));
    expect(c.retryable).toBe(false);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toBe('Key unavailable');
  });

  it('REVOKED_EPOCH is terminal and triggers rekey', () => {
    const c = classifyError(new E2EEKeyUnavailableError('REVOKED_EPOCH'));
    expect(c.retryable).toBe(false);
    expect(c.triggerRekey).toBe(true);
    expect(c.uxMessage).toContain('re-establishing');
  });

  it('NOT_MEMBER is terminal with access-denied copy', () => {
    const c = classifyError(new E2EEKeyUnavailableError('NOT_MEMBER'));
    expect(c.retryable).toBe(false);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toContain("don't have access");
  });

  it('MALFORMED_PAYLOAD is terminal with retry hint', () => {
    const c = classifyError(new E2EEKeyUnavailableError('MALFORMED_PAYLOAD'));
    expect(c.retryable).toBe(false);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toContain('please try again');
  });

  it('INVALID_REQUEST is terminal with generic-invalid copy', () => {
    const c = classifyError(new E2EEKeyUnavailableError('INVALID_REQUEST'));
    expect(c.retryable).toBe(false);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toBe('Invalid request');
  });

  it('retries INTERNAL_ERROR (transient server failure)', () => {
    const c = classifyError(new E2EEKeyUnavailableError('INTERNAL_ERROR'));
    expect(c.retryable).toBe(true);
    expect(c.triggerRekey).toBe(false);
  });

  it.each<E2EEKeyErrorCode>(['NOT_MEMBER', 'MALFORMED_PAYLOAD', 'INVALID_REQUEST'])(
    '%s is terminal and does not trigger rekey (table)',
    (code) => {
      const c = classifyError(new E2EEKeyUnavailableError(code));
      expect(c.retryable).toBe(false);
      expect(c.triggerRekey).toBe(false);
    }
  );

  it('typed error with an out-of-union code falls to the fail-closed default (terminal, generic copy)', () => {
    // Codes can be retired server-side (e.g. the per-channel "not encrypted"
    // code removed in #1650). A stray code outside the union must still
    // classify terminal via the `default:` branch, never return undefined.
    const retiredCode: string = 'LEGACY_REMOVED_CODE';
    const c = classifyError(new E2EEKeyUnavailableError(retiredCode as E2EEKeyErrorCode));
    expect(c.retryable).toBe(false);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toBe('Unable to send message');
  });

  it('non-typed Error classifies as retryable (preserves network-retry behavior)', () => {
    const c = classifyError(new Error('Network error'));
    expect(c.retryable).toBe(true);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toBe('Unable to send message');
  });

  it('non-Error value classifies as retryable', () => {
    const c = classifyError(undefined);
    expect(c.retryable).toBe(true);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toBe('Unable to send message');
  });

  it('string value classifies as retryable', () => {
    const c = classifyError('PENDING_KEY');
    expect(c.retryable).toBe(true);
    expect(c.triggerRekey).toBe(false);
    expect(c.uxMessage).toBe('Unable to send message');
  });

  it('null value classifies as retryable', () => {
    const c = classifyError(null);
    expect(c.retryable).toBe(true);
    expect(c.triggerRekey).toBe(false);
  });
});
