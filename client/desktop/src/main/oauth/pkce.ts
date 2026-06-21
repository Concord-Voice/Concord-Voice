/**
 * PKCE primitives shared by the client-driven Apple (#974) and Google (#975)
 * OAuth exchanges. Hoisted to oauth/ from apple/pkce.ts so both flows consume it.
 *
 * RFC 7636: the verifier is 32 CSPRNG bytes base64url-encoded (43 chars —
 * the RFC's recommended entropy); the challenge is
 * BASE64URL(SHA256(ASCII(verifier))) with method S256.
 *
 * node:crypto only (project CSPRNG rule — no custom crypto, no third-party
 * randomness). The verifier is generated inside each flow's closure, never
 * persisted, never IPC'd (spec §Security: PKCE).
 */
import { createHash, randomBytes } from 'node:crypto';

/** 32 CSPRNG bytes → 43-char base64url verifier (RFC 7636 §4.1). */
export function generateCodeVerifier(): string {
  return randomBytes(32).toString('base64url');
}

/** S256 code_challenge per RFC 7636 §4.2. */
export function codeChallengeS256(verifier: string): string {
  return createHash('sha256').update(verifier, 'ascii').digest('base64url');
}
