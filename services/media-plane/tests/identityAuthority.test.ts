import { describe, it, expect } from 'vitest';
import { getIdentityAuthorityReasonCode } from '../src/lib/identityAuthority.js';
import type { ParticipantIdentity } from '../src/middleware/auth.js';

// `resolveParticipantIdentity` coerces an absent server username to '' and
// leaves the optional fields undefined -- it never re-opens a field to the
// handshake value (CV-CAN-017). These fixtures mirror that exactly; a fixture
// that invented a fully-populated server identity would make the regression
// below unreproducible.
const server = (o: Partial<ParticipantIdentity> = {}): ParticipantIdentity => ({
  username: 'alice',
  ...o,
});
const client = (o: Partial<ParticipantIdentity> = {}): ParticipantIdentity => ({
  username: 'alice',
  ...o,
});

describe('getIdentityAuthorityReasonCode', () => {
  it('reports missing authority when the control-plane returned no identity', () => {
    expect(getIdentityAuthorityReasonCode(false, server(), client())).toBe(
      'identity_authority_missing'
    );
    expect(getIdentityAuthorityReasonCode(undefined, server(), client())).toBe(
      'identity_authority_missing'
    );
  });

  it('reports nothing when the client mirrors the server exactly', () => {
    const same = { username: 'alice', displayName: 'Alice', avatarUrl: 'https://x/a.png' };
    expect(getIdentityAuthorityReasonCode(true, same, { ...same })).toBeUndefined();
  });

  // ── The #3136 regression. Each of these fired a denied/medium event. ──

  it('does NOT flag a client that omitted displayName the server has', () => {
    // The single most common ordinary join: the client never sent the field.
    expect(
      getIdentityAuthorityReasonCode(true, server({ displayName: 'Alice' }), client())
    ).toBeUndefined();
  });

  it('does NOT flag a client that omitted avatarUrl the server has', () => {
    expect(
      getIdentityAuthorityReasonCode(true, server({ avatarUrl: 'https://x/a.png' }), client())
    ).toBeUndefined();
  });

  it('does NOT flag a client that omitted every optional field', () => {
    expect(
      getIdentityAuthorityReasonCode(
        true,
        server({ displayName: 'Alice', avatarUrl: 'https://x/a.png' }),
        client()
      )
    ).toBeUndefined();
  });

  it('does NOT flag an empty-string field — absence, not an assertion', () => {
    // A client that sends '' has supplied no identity, however the wire encodes it.
    expect(
      getIdentityAuthorityReasonCode(
        true,
        server({ displayName: 'Alice' }),
        client({ displayName: '' })
      )
    ).toBeUndefined();
  });

  it('does NOT flag when the server itself has no username (coerced to empty)', () => {
    // authUsername ?? '' is the server's own fail-closed shape, not a conflict.
    expect(
      getIdentityAuthorityReasonCode(true, { username: '' }, { username: '' })
    ).toBeUndefined();
  });

  // ── What it must still catch. ──

  it('flags a client asserting a conflicting displayName', () => {
    expect(
      getIdentityAuthorityReasonCode(
        true,
        server({ displayName: 'Alice' }),
        client({ displayName: 'Administrator' })
      )
    ).toBe('identity_authority_mismatch');
  });

  it('flags a client asserting a conflicting username', () => {
    expect(getIdentityAuthorityReasonCode(true, server(), client({ username: 'mallory' }))).toBe(
      'identity_authority_mismatch'
    );
  });

  it('flags a client asserting a conflicting avatarUrl', () => {
    expect(
      getIdentityAuthorityReasonCode(
        true,
        server({ avatarUrl: 'https://x/a.png' }),
        client({ avatarUrl: 'https://evil/a.png' })
      )
    ).toBe('identity_authority_mismatch');
  });

  it('flags a value the server does not have at all', () => {
    // Server username is empty; the client asserts one. That is a conflict,
    // not an absence -- the asymmetry with the empty-client case above is the
    // point, so a single "either side is empty" shortcut would be wrong.
    expect(getIdentityAuthorityReasonCode(true, { username: '' }, { username: 'alice' })).toBe(
      'identity_authority_mismatch'
    );
  });

  it('still flags a stale-but-present value — staleness is not distinguishable here', () => {
    // Documented non-goal, pinned so nobody "fixes" it into silence: from this
    // vantage a stale cached displayName and a spoof are the same bytes.
    expect(
      getIdentityAuthorityReasonCode(
        true,
        server({ displayName: 'Alice Cooper' }),
        client({ displayName: 'Alice' })
      )
    ).toBe('identity_authority_mismatch');
  });
});
