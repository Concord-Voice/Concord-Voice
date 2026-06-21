import * as jose from 'jose';
import { GoogleFlowError } from './errors';

export const GOOGLE_ISSUERS = ['https://accounts.google.com', 'accounts.google.com'] as const;
export const GOOGLE_JWKS_URL = 'https://www.googleapis.com/oauth2/v3/certs';

export interface GoogleIDTokenClaims {
  sub: string;
  email?: string;
  email_verified?: boolean;
  name?: string;
  nonce?: string;
}

let remoteJwks: jose.JWTVerifyGetKey | undefined;

export async function verifyGoogleIDToken(opts: {
  idToken: string;
  clientId: string;
  expectedNonce: string;
  signal?: AbortSignal;
  jwks?: jose.JWTVerifyGetKey; // test seam
}): Promise<GoogleIDTokenClaims> {
  if (opts.signal?.aborted) throw new GoogleFlowError('sso_cancelled', 'verify');
  const keySet = opts.jwks ?? (remoteJwks ??= jose.createRemoteJWKSet(new URL(GOOGLE_JWKS_URL)));
  let payload: jose.JWTPayload;
  try {
    ({ payload } = await jose.jwtVerify(opts.idToken, keySet, {
      algorithms: ['RS256'],
      issuer: [...GOOGLE_ISSUERS],
      audience: opts.clientId,
    }));
  } catch (err) {
    if (opts.signal?.aborted) throw new GoogleFlowError('sso_cancelled', 'verify');
    if (
      err instanceof jose.errors.JWKSNoMatchingKey ||
      err instanceof jose.errors.JWKSTimeout ||
      err instanceof TypeError
    ) {
      throw new GoogleFlowError('google_verification_unavailable', 'verify');
    }
    throw new GoogleFlowError('google_id_token_invalid', 'verify');
  }
  if (payload.nonce !== opts.expectedNonce) throw new GoogleFlowError('google_id_token_invalid', 'verify-nonce');
  if (typeof payload.sub !== 'string' || payload.sub.length === 0) {
    throw new GoogleFlowError('google_id_token_invalid', 'verify-sub');
  }
  return {
    sub: payload.sub,
    email: typeof payload.email === 'string' ? payload.email : undefined,
    email_verified: payload.email_verified === true,
    name: typeof payload.name === 'string' ? payload.name : undefined,
    nonce: typeof payload.nonce === 'string' ? payload.nonce : undefined,
  };
}
