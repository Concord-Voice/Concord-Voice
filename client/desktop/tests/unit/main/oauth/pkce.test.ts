// @vitest-environment node
/**
 * PKCE primitive tests (#974). Node environment: these are main-process
 * modules exercising node:crypto, not DOM code.
 */
import { describe, expect, it } from 'vitest';

import { codeChallengeS256, generateCodeVerifier } from '@/main/oauth/pkce';

describe('generateCodeVerifier', () => {
  it('produces 43-char base64url output (32 CSPRNG bytes, RFC 7636)', () => {
    const v = generateCodeVerifier();
    expect(v).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });

  it('never repeats across 1000 generations (entropy smoke)', () => {
    const seen = new Set<string>();
    for (let i = 0; i < 1000; i++) seen.add(generateCodeVerifier());
    expect(seen.size).toBe(1000);
  });
});

describe('codeChallengeS256', () => {
  it('matches the RFC 7636 appendix B reference vector', () => {
    expect(codeChallengeS256('dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk')).toBe(
      'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM'
    );
  });

  it('produces 43-char base64url output for generated verifiers', () => {
    expect(codeChallengeS256(generateCodeVerifier())).toMatch(/^[A-Za-z0-9_-]{43}$/);
  });
});
