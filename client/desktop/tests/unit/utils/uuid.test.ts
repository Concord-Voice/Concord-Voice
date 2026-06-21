import { describe, it, expect } from 'vitest';
import { isValidUUID, assertValidUUID } from '@/renderer/utils/uuid';

describe('isValidUUID', () => {
  it('returns true for a canonical UUID shape', () => {
    // Validator is intentionally version-agnostic — matches any well-formed
    // 8-4-4-4-12 hex pattern regardless of version nibble.
    expect(isValidUUID('123e4567-e89b-12d3-a456-426614174000')).toBe(true);
  });

  it('returns true for uppercase UUID hex', () => {
    expect(isValidUUID('123E4567-E89B-12D3-A456-426614174000')).toBe(true);
  });

  it('returns false for empty string', () => {
    expect(isValidUUID('')).toBe(false);
  });

  it('returns false for UUID with missing hyphens', () => {
    expect(isValidUUID('123e4567e89b12d3a456426614174000')).toBe(false); // pragma: allowlist secret
  });

  it('returns false for UUID with wrong segment lengths', () => {
    expect(isValidUUID('123e456-e89b-12d3-a456-426614174000')).toBe(false);
  });

  it('returns false for UUID with non-hex characters', () => {
    expect(isValidUUID('123g4567-e89b-12d3-a456-426614174000')).toBe(false);
  });

  it('returns false for path traversal injection', () => {
    expect(isValidUUID('../../etc/passwd')).toBe(false);
  });

  it('returns false for URL-encoded path traversal', () => {
    expect(isValidUUID('%2e%2e%2f%2e%2e%2fetc%2fpasswd')).toBe(false);
  });

  it('returns false for HTML-like content', () => {
    expect(isValidUUID('<script>alert(1)</script>')).toBe(false);
  });

  it('returns false for string longer than UUID length', () => {
    expect(isValidUUID('123e4567-e89b-12d3-a456-426614174000-extra')).toBe(false);
  });

  it('returns false for string shorter than UUID length', () => {
    expect(isValidUUID('123e4567-e89b-12d3-a456')).toBe(false);
  });
});

describe('assertValidUUID', () => {
  it('returns the input string when valid', () => {
    const uuid = '123e4567-e89b-12d3-a456-426614174000';
    expect(assertValidUUID(uuid, 'testField')).toBe(uuid);
  });

  it('throws with field name for malformed UUID', () => {
    expect(() => assertValidUUID('not-a-uuid', 'requestId')).toThrow(
      'requestId is not a valid UUID'
    );
  });

  it('throws for empty string', () => {
    expect(() => assertValidUUID('', 'requestId')).toThrow('requestId is not a valid UUID');
  });
});
