// @vitest-environment node
/**
 * Apple id_token verifier tests (#974): signed JWTs minted with jose itself.
 * Node environment — jose's webapi build needs globalThis.crypto.subtle.
 */
import * as jose from 'jose';
import { beforeAll, describe, expect, it } from 'vitest';

import { AppleFlowError } from '@/main/oauth/apple/errors';
import { APPLE_ISSUER, verifyAppleIDToken } from '@/main/oauth/apple/idTokenVerifier';

const CLIENT_ID = 'chat.concordvoice.signin';
const NONCE = 'expected-nonce';
const KID = 'apple-kid-1';

let privateKey: jose.CryptoKey;
let jwks: jose.JWTVerifyGetKey;
let foreignKey: jose.CryptoKey;

beforeAll(async () => {
  const pair = await jose.generateKeyPair('RS256', { extractable: true });
  privateKey = pair.privateKey;
  const publicJwk = await jose.exportJWK(pair.publicKey);
  jwks = jose.createLocalJWKSet({ keys: [{ ...publicJwk, kid: KID, alg: 'RS256', use: 'sig' }] });

  const foreign = await jose.generateKeyPair('RS256');
  foreignKey = foreign.privateKey;
});

interface MintOpts {
  key?: jose.CryptoKey;
  kid?: string;
  issuer?: string;
  audience?: string;
  nonce?: string;
  nonceSupported?: boolean | string;
  expiresIn?: string;
  extraClaims?: Record<string, unknown>;
}

async function mint(opts: MintOpts = {}): Promise<string> {
  return new jose.SignJWT({
    nonce: opts.nonce ?? NONCE,
    nonce_supported: opts.nonceSupported ?? true,
    email: 'jane@example.com',
    email_verified: true,
    ...opts.extraClaims,
  })
    .setProtectedHeader({ alg: 'RS256', kid: opts.kid ?? KID })
    .setSubject('001234.aabbccddeeff.1234')
    .setIssuer(opts.issuer ?? APPLE_ISSUER)
    .setAudience(opts.audience ?? CLIENT_ID)
    .setIssuedAt()
    .setExpirationTime(opts.expiresIn ?? '1h')
    .sign(opts.key ?? privateKey);
}

function verify(idToken: string) {
  return verifyAppleIDToken({ idToken, clientId: CLIENT_ID, expectedNonce: NONCE, jwks });
}

async function expectCode(p: Promise<unknown>, code: string) {
  const err = (await p.then(
    () => null,
    (e) => e
  )) as AppleFlowError | null;
  expect(err).toBeInstanceOf(AppleFlowError);
  expect(err?.code).toBe(code);
}

describe('verifyAppleIDToken', () => {
  it('returns normalized claims for a valid token', async () => {
    const claims = await verify(await mint());
    expect(claims.sub).toBe('001234.aabbccddeeff.1234');
    expect(claims.email).toBe('jane@example.com');
    expect(claims.emailVerified).toBe(true);
  });

  it('rejects a bad signature (foreign key, same kid)', async () => {
    await expectCode(verify(await mint({ key: foreignKey })), 'apple_id_token_invalid');
  });

  it('rejects a wrong issuer', async () => {
    await expectCode(
      verify(await mint({ issuer: 'https://evil.example' })),
      'apple_id_token_invalid'
    );
  });

  it('rejects a wrong audience', async () => {
    await expectCode(verify(await mint({ audience: 'com.evil.app' })), 'apple_id_token_invalid');
  });

  it('rejects an expired token', async () => {
    await expectCode(verify(await mint({ expiresIn: '-1h' })), 'apple_id_token_invalid');
  });

  it('rejects a nonce mismatch and never retries the token', async () => {
    await expectCode(verify(await mint({ nonce: 'other-nonce' })), 'apple_id_token_invalid');
  });

  it('rejects nonce_supported=false (boolean and Apple string-bool form)', async () => {
    await expectCode(verify(await mint({ nonceSupported: false })), 'apple_id_token_invalid');
    await expectCode(verify(await mint({ nonceSupported: 'false' })), 'apple_id_token_invalid');
  });

  it('rejects an HS256-signed token (algorithm-confusion allowlist)', async () => {
    const hsToken = await new jose.SignJWT({ nonce: NONCE, nonce_supported: true })
      .setProtectedHeader({ alg: 'HS256', kid: KID })
      .setSubject('001234.aabbccddeeff.1234')
      .setIssuer(APPLE_ISSUER)
      .setAudience(CLIENT_ID)
      .setIssuedAt()
      .setExpirationTime('1h')
      .sign(new TextEncoder().encode('shared-secret-shared-secret-1234'));
    await expectCode(verify(hsToken), 'apple_id_token_invalid');
  });

  it('maps a kid miss to apple_verification_unavailable (retryable — JWKS refetch)', async () => {
    await expectCode(
      verify(await mint({ kid: 'kid-not-published' })),
      'apple_verification_unavailable'
    );
  });

  it('rejects a token without sub', async () => {
    const noSub = await new jose.SignJWT({ nonce: NONCE, nonce_supported: true })
      .setProtectedHeader({ alg: 'RS256', kid: KID })
      .setIssuer(APPLE_ISSUER)
      .setAudience(CLIENT_ID)
      .setIssuedAt()
      .setExpirationTime('1h')
      .sign(privateKey);
    await expectCode(verify(noSub), 'apple_id_token_invalid');
  });

  it('maps an aborted flow signal to sso_cancelled (JWKS fetch path)', async () => {
    // No jwks seam — the remote JWKS path runs, but the pre-aborted signal
    // merged via [jose.customFetch] rejects the fetch before any network
    // I/O, so the test stays deterministic and offline (#1486 review fix).
    const controller = new AbortController();
    controller.abort();
    await expectCode(
      verifyAppleIDToken({
        idToken: await mint(),
        clientId: CLIENT_ID,
        expectedNonce: NONCE,
        signal: controller.signal,
      }),
      'sso_cancelled'
    );
  });
});
