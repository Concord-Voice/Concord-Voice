import { describe, it, expect, vi } from 'vitest';
import jwt from 'jsonwebtoken';
import './mocks/logger.js';

// Mock config — mirrors tests/auth.test.ts.
vi.mock('@/config/index.js', () => ({
  config: {
    jwtSecret: ['vitest', 'mock', 'jwt'].join('-'),
    controlPlaneUrl: 'http://localhost:8080',
  },
}));

const TEST_SIGNING_KEY = ['vitest', 'mock', 'jwt'].join('-'); // NOSONAR — test-only mock

import { createAuthMiddleware } from '../src/middleware/auth.js';
import { isCanonicalEnforcementUUID } from '../src/lib/enforcementCommand.js';

const CANONICAL = 'd8fe119d-a28c-4573-be19-7847469aa59e';

function authenticate(userIdClaim: string) {
  const token = jwt.sign({ user_id: userIdClaim, tier: 'free' }, TEST_SIGNING_KEY, {
    algorithm: 'HS256',
    issuer: 'concordvoice-control-plane',
    expiresIn: '15m',
  });
  const socket = {
    id: 'socket-identity-canon',
    handshake: { auth: { token, username: 'canon' }, address: '127.0.0.1' },
    data: {} as Record<string, unknown>,
  };
  const mw = createAuthMiddleware();
  let err: unknown = 'not-called';
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mw(socket as any, (e?: unknown) => {
    err = e;
  });
  return { socket, err };
}

// #3362 (media-plane half). The socket's session identity is the key a
// participant is stored under — `roomManager.participants.set(userId, …)` — while
// the mid-session enforcement rail refuses to act on any userId
// `isCanonicalEnforcementUUID` rejects. Admitting a spelling that predicate
// refuses therefore seats a participant NOTHING the control plane can address:
// no kick, no mute, no permission revocation. That is an enforcement bypass, not
// a cosmetic mismatch.
//
// The control plane canonicalizes at its admission choke point, but that is not
// this service's choke point — the media plane verifies the same token itself
// and never passes through AuthRequired — so the invariant is re-established
// here, by REFUSING what enforcement cannot reach. The single shared predicate
// is what makes the invariant provable: admission and enforcement cannot drift
// apart, because they call the same function.
describe('#3362 — media-plane session identity is enforcement-addressable', () => {
  it('admits a canonical claim and stores an enforceable identity', () => {
    const { socket, err } = authenticate(CANONICAL);

    expect(err).toBeUndefined();
    expect(socket.data.userId).toBe(CANONICAL);
    expect(isCanonicalEnforcementUUID(socket.data.userId)).toBe(true);
  });

  // Every spelling below is one the FIXED control plane admits with a 200 — it
  // normalizes them. The media plane cannot normalize without a UUID parser it
  // does not depend on, so it refuses instead; see the comment at the guard for
  // why that asymmetry is deliberate and what it costs.
  const refused: Array<[string, string]> = [
    ['urn:uuid: (the control plane widening)', `urn:uuid:${CANONICAL}`],
    ['URN:UUID: (EqualFold prefix)', `URN:UUID:${CANONICAL}`],
    ['braced', `{${CANONICAL}}`],
    ['free-form delimiters (uuid.Parse len-38 arm)', `X${CANONICAL}Y`],
    ['UPPERCASE', CANONICAL.toUpperCase()],
    ['dash-less', CANONICAL.replace(/-/g, '')],
  ];

  it.each(refused)('refuses %s rather than seating an unaddressable participant', (_label, claim) => {
    // Guard the fixture: a "variant" that equals the canonical form would make
    // this a second control asserting the opposite of what it claims.
    expect(claim).not.toBe(CANONICAL);
    expect(isCanonicalEnforcementUUID(claim)).toBe(false);

    const { socket, err } = authenticate(claim);

    expect(err).toBeInstanceOf(Error);
    // No session identity is published at all — the refusal happens before the
    // socket.data assignment, so there is no participant key to leak.
    expect(socket.data.userId).toBeUndefined();
  });
});
