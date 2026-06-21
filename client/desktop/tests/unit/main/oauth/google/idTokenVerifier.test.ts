// @vitest-environment node
import { describe, it, expect, beforeAll } from 'vitest';
import * as jose from 'jose';
import { verifyGoogleIDToken, GOOGLE_ISSUERS } from '../../../../../src/main/oauth/google/idTokenVerifier';
import { GoogleFlowError } from '../../../../../src/main/oauth/google/errors';

const CLIENT_ID = 'test-client.apps.googleusercontent.com';
const NONCE = 'nonce-123';
let priv: jose.KeyLike;
let jwks: jose.JWTVerifyGetKey;

beforeAll(async () => {
  const kp = await jose.generateKeyPair('RS256');
  priv = kp.privateKey;
  const pubJwk = await jose.exportJWK(kp.publicKey);
  pubJwk.kid = 'k1';
  pubJwk.alg = 'RS256';
  jwks = jose.createLocalJWKSet({ keys: [pubJwk] });
});

async function mint(over: Record<string, unknown> = {}, alg = 'RS256'): Promise<string> {
  return new jose.SignJWT({ nonce: NONCE, sub: 'g-sub-1', email: 'a@b.com', email_verified: true, ...over })
    .setProtectedHeader({ alg, kid: 'k1' })
    .setIssuer('https://accounts.google.com')
    .setAudience(CLIENT_ID)
    .setExpirationTime('5m')
    .setIssuedAt()
    .sign(priv);
}

const expectCode = async (p: Promise<unknown>) =>
  p.then(() => null, (e) => (e instanceof GoogleFlowError ? e.code : `unexpected:${e}`));

describe('verifyGoogleIDToken', () => {
  it('accepts a valid token with both issuer forms', async () => {
    for (const iss of GOOGLE_ISSUERS) {
      const t = await new jose.SignJWT({ nonce: NONCE, sub: 's', email_verified: true })
        .setProtectedHeader({ alg: 'RS256', kid: 'k1' })
        .setIssuer(iss).setAudience(CLIENT_ID).setExpirationTime('5m').setIssuedAt().sign(priv);
      const claims = await verifyGoogleIDToken({ idToken: t, clientId: CLIENT_ID, expectedNonce: NONCE, jwks });
      expect(claims.sub).toBe('s');
    }
  });
  it('rejects wrong audience', async () =>
    expect(await expectCode(verifyGoogleIDToken({ idToken: await mint({}), clientId: 'other', expectedNonce: NONCE, jwks }))).toBe('google_id_token_invalid'));
  it('rejects nonce mismatch', async () =>
    expect(await expectCode(verifyGoogleIDToken({ idToken: await mint({ nonce: 'x' }), clientId: CLIENT_ID, expectedNonce: NONCE, jwks }))).toBe('google_id_token_invalid'));
  it('rejects HS256 algorithm confusion', async () => {
    const hs = await new jose.SignJWT({ nonce: NONCE, sub: 's' })
      .setProtectedHeader({ alg: 'HS256' }).setIssuer('https://accounts.google.com')
      .setAudience(CLIENT_ID).setExpirationTime('5m').sign(new TextEncoder().encode('shared'));
    expect(await expectCode(verifyGoogleIDToken({ idToken: hs, clientId: CLIENT_ID, expectedNonce: NONCE, jwks }))).toBe('google_id_token_invalid');
  });
  it('rejects expired token', async () => {
    // Build expired token directly (setExpirationTime in mint() would override exp payload)
    const expired = await new jose.SignJWT({ nonce: NONCE, sub: 'g-sub-1', email: 'a@b.com', email_verified: true })
      .setProtectedHeader({ alg: 'RS256', kid: 'k1' })
      .setIssuer('https://accounts.google.com')
      .setAudience(CLIENT_ID)
      .setExpirationTime(Math.floor(Date.now() / 1000) - 10)
      .setIssuedAt(Math.floor(Date.now() / 1000) - 20)
      .sign(priv);
    expect(await expectCode(verifyGoogleIDToken({ idToken: expired, clientId: CLIENT_ID, expectedNonce: NONCE, jwks }))).toBe('google_id_token_invalid');
  });
  it('rejects missing sub', async () => {
    const t = await new jose.SignJWT({ nonce: NONCE }).setProtectedHeader({ alg: 'RS256', kid: 'k1' })
      .setIssuer('https://accounts.google.com').setAudience(CLIENT_ID).setExpirationTime('5m').sign(priv);
    expect(await expectCode(verifyGoogleIDToken({ idToken: t, clientId: CLIENT_ID, expectedNonce: NONCE, jwks }))).toBe('google_id_token_invalid');
  });
});
