import { describe, it, expect } from 'vitest';
import type { SSOSignInResult, SSOSignInErrorCode } from '../../../src/shared/sso';

describe('SSOSignInResult', () => {
  it('accepts each discriminated variant', () => {
    const variants: SSOSignInResult[] = [
      { kind: 'tokens', accessToken: 'a' },
      { kind: 'mfa_challenge', mfaChallengeToken: 't', methods: ['totp'] },
      { kind: 'sso_token', branch: 'new_user', ssoToken: 's', email: 'e@x.com' },
      { kind: 'sso_token', branch: 'account_link', ssoToken: 's', maskedEmail: 'e***@x.com' },
      { kind: 'error', code: 'google_id_token_invalid' },
    ];
    expect(variants).toHaveLength(5);
  });

  it('error union includes both apple and google codes', () => {
    const codes: SSOSignInErrorCode[] = ['apple_exchange_rejected', 'google_exchange_rejected'];
    expect(codes).toHaveLength(2);
  });
});
