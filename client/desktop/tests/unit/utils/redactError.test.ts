import { describe, it, expect } from 'vitest';
import { errorMessage, errorName } from '@/renderer/utils/redactError';

describe('errorMessage', () => {
  it('returns Error.message for Error instances', () => {
    expect(errorMessage(new Error('failed'))).toBe('failed');
    expect(errorMessage(new TypeError('bad type'))).toBe('bad type');
  });

  it('returns fallback for non-Error values', () => {
    expect(errorMessage('string thrown')).toBe('unknown_error');
    expect(errorMessage(42)).toBe('unknown_error');
    expect(errorMessage(null)).toBe('unknown_error');
    expect(errorMessage(undefined)).toBe('unknown_error');
    expect(errorMessage({ message: 'duck-typed' })).toBe('unknown_error');
  });
});

describe('errorName', () => {
  it('returns Error.name for Error subclasses', () => {
    expect(errorName(new Error('e'))).toBe('Error');
    expect(errorName(new TypeError('e'))).toBe('TypeError');
    expect(errorName(new SyntaxError('e'))).toBe('SyntaxError');
  });

  it('returns fallback for non-Error values', () => {
    expect(errorName('string thrown')).toBe('unknown');
    expect(errorName(42)).toBe('unknown');
    expect(errorName(null)).toBe('unknown');
    expect(errorName(undefined)).toBe('unknown');
  });
});
