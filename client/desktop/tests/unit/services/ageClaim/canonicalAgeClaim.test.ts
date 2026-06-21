import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, it, expect } from 'vitest';
import {
  buildCanonicalBytes,
  validateAgeClaim,
  type AgeClaim,
} from '@/renderer/services/ageClaim/canonicalAgeClaim';

// The A↔B parity fixture shipped by #1623 (server-owned single source of truth).
// Read via node:fs (NOT an import) so Vite's server.fs.allow does not apply.
const FIXTURE = JSON.parse(
  readFileSync(
    resolve(
      __dirname,
      '../../../../../../services/control-plane/internal/age/testdata/age-claim-canonical-v1.json'
    ),
    'utf8'
  )
);

function claimFromFixture(): AgeClaim {
  const c = FIXTURE.claim;
  return {
    canonicalVersion: c.canonical_version,
    userId: c.user_id,
    validAge: c.valid_age,
    nsfwAuth: c.nsfw_auth,
    jurisdictionObligation: c.jurisdiction_obligation,
    nonce: c.nonce,
    timestamp: c.timestamp,
    keyVersion: c.key_version,
    clientVersion: c.client_version,
  };
}

function badClaim(overrides: Record<string, unknown>): AgeClaim {
  return { ...claimFromFixture(), ...overrides } as unknown as AgeClaim;
}

describe('buildCanonicalBytes', () => {
  it('is byte-identical to the server fixture canonical_utf8', () => {
    const bytes = buildCanonicalBytes(claimFromFixture());
    const expected = new TextEncoder().encode(FIXTURE.canonical_utf8);
    expect(Array.from(bytes)).toEqual(Array.from(expected));
  });
});

describe('validateAgeClaim', () => {
  it('accepts the fixture claim', () => {
    expect(() => validateAgeClaim(claimFromFixture())).not.toThrow();
  });

  it.each([
    ['canonical_version', { canonicalVersion: 2 }],
    ['user_id', { userId: 'NOT-A-UUID' }],
    ['user_id uppercase', { userId: 'AAAAAAAA-1111-4111-8111-111111111111' }],
    ['jurisdiction_obligation', { jurisdictionObligation: 3 }],
    ['jurisdiction_obligation non-integer', { jurisdictionObligation: 1.5 }],
    ['nonce', { nonce: 'short' }],
    ['timestamp', { timestamp: 0 }],
    ['key_version', { keyVersion: 0 }],
    ['client_version', { clientVersion: 'has space' }],
  ])('rejects bad %s', (_label, overrides) => {
    expect(() => validateAgeClaim(badClaim(overrides))).toThrow();
  });
});
