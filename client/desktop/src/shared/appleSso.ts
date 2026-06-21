// Back-compat shim (#975): the Apple SSO types were generalized to SSOSignInResult
// / SSOSignInErrorCode in ./sso.ts. New code should import from './sso'.
export type {
  SSOSignInResult as AppleSignInResult,
  SSOSignInErrorCode as AppleSignInErrorCode,
} from './sso';
