import { describe, it, expect } from 'vitest';
import { parseContinuationPair } from '@/renderer/utils/crypto/continuationPair';

describe('parseContinuationPair', () => {
  it('returns the triple when all three fields are non-empty strings', () => {
    expect(
      parseContinuationPair({
        access_token: 'at',
        refresh_token: 'rt',
        session_id: 'sid',
        presence_override_version: 4,
      })
    ).toEqual({ accessToken: 'at', refreshToken: 'rt', sessionId: 'sid' });
  });

  it('returns null when the fields are absent (a deliberate server skip)', () => {
    expect(parseContinuationPair({ presence_override_version: 4 })).toBeNull();
  });

  it('returns null for a partial set — a healthy server never emits one', () => {
    expect(parseContinuationPair({ access_token: 'at', refresh_token: 'rt' })).toBeNull();
    expect(parseContinuationPair({ access_token: 'at', session_id: 'sid' })).toBeNull();
    expect(parseContinuationPair({ refresh_token: 'rt', session_id: 'sid' })).toBeNull();
  });

  it('returns null when any field is an empty string or a non-string', () => {
    expect(
      parseContinuationPair({ access_token: '', refresh_token: 'rt', session_id: 'sid' })
    ).toBeNull();
    expect(
      parseContinuationPair({ access_token: 1, refresh_token: 'rt', session_id: 'sid' })
    ).toBeNull();
    expect(
      parseContinuationPair({ access_token: 'at', refresh_token: 'rt', session_id: null })
    ).toBeNull();
  });

  it('does not throw on a hostile shape', () => {
    expect(() => parseContinuationPair({ access_token: { nested: true } })).not.toThrow();
    expect(parseContinuationPair({})).toBeNull();
  });
});
