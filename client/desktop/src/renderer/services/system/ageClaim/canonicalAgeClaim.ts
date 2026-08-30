// Mirror of services/control-plane/internal/age/canonical.go CanonicalBytes (#1623).
// Byte-parity with the server is the contract (docs/age-claim-canonical-form.md);
// the test asserts it against the committed A↔B fixture. Do NOT reorder/alter fields.
export interface AgeClaim {
  canonicalVersion: 1;
  userId: string; // lowercase RFC4122 uuid
  validAge: boolean;
  nsfwAuth: boolean;
  jurisdictionObligation: 0 | 1 | 2;
  nonce: string; // exactly 64 lowercase hex chars
  timestamp: number; // unix seconds, integer
  keyVersion: number; // positive integer
  clientVersion: string; // [0-9A-Za-z.+-], 1..32 chars
}

export function buildCanonicalBytes(claim: AgeClaim): Uint8Array {
  const lines = [
    'age-claim/v1',
    `canonical_version=${claim.canonicalVersion}`,
    `user_id=${claim.userId}`,
    `valid_age=${claim.validAge}`,
    `nsfw_auth=${claim.nsfwAuth}`,
    `jurisdiction_obligation=${claim.jurisdictionObligation}`,
    `nonce=${claim.nonce}`,
    `timestamp=${claim.timestamp}`,
    `key_version=${claim.keyVersion}`,
    `client_version=${claim.clientVersion}`,
  ];
  return new TextEncoder().encode(lines.join('\n'));
}

const NONCE_RE = /^[0-9a-f]{64}$/;
const CLIENT_VERSION_RE = /^[0-9A-Za-z.+-]{1,32}$/;
const UUID_LOWER_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

export function validateAgeClaim(claim: AgeClaim): void {
  if (claim.canonicalVersion !== 1) throw new Error('age-claim: bad canonical_version');
  if (!UUID_LOWER_RE.test(claim.userId)) throw new Error('age-claim: bad user_id');
  if (
    !Number.isInteger(claim.jurisdictionObligation) ||
    claim.jurisdictionObligation < 0 ||
    claim.jurisdictionObligation > 2
  )
    throw new Error('age-claim: bad jurisdiction_obligation');
  if (!NONCE_RE.test(claim.nonce)) throw new Error('age-claim: bad nonce');
  if (!Number.isInteger(claim.timestamp) || claim.timestamp <= 0)
    throw new Error('age-claim: bad timestamp');
  if (!Number.isInteger(claim.keyVersion) || claim.keyVersion <= 0)
    throw new Error('age-claim: bad key_version');
  if (!CLIENT_VERSION_RE.test(claim.clientVersion))
    throw new Error('age-claim: bad client_version');
}
