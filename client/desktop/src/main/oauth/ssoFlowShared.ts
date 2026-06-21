/**
 * Shared pure helpers for the Apple (#974) and Google (#975) SSO flow
 * orchestrators. Extracted to eliminate verbatim duplication between
 * appleFlow.ts and googleFlow.ts (SonarCloud new_duplicated_lines_density gate).
 *
 * All helpers are behaviour-identical across providers — only the log prefix
 * and provider-specific Error class differ, so those are passed as parameters.
 */
import { timingSafeEqual } from 'node:crypto';

import type { SSOSignInErrorCode, SSOSignInResult } from '../../shared/sso';

/**
 * Constant-time string equality via Node crypto.timingSafeEqual.
 * Length is not secret (both values are CSPRNG-shaped); the over-equal-length
 * compare is what must be constant-time (CWE-385 defence).
 */
export function constantTimeEquals(a: string, b: string): boolean {
  const ab = Buffer.from(a, 'utf8');
  const bb = Buffer.from(b, 'utf8');
  if (ab.length !== bb.length) return false;
  return timingSafeEqual(ab, bb);
}

/**
 * Maps a raw /session response body (Callback-shape parity) into the
 * IPC-safe discriminated SSOSignInResult union.
 *
 * AppleSignInResult is a re-export alias of SSOSignInResult (./shared/appleSso),
 * so this function satisfies both flow return types.
 */
export function mapSessionResponse(body: Record<string, unknown>): SSOSignInResult {
  if (typeof body.access_token === 'string' && body.access_token.length > 0) {
    return { kind: 'tokens', accessToken: body.access_token };
  }
  if (typeof body.mfa_challenge_token === 'string' && body.mfa_challenge_token.length > 0) {
    return {
      kind: 'mfa_challenge',
      mfaChallengeToken: body.mfa_challenge_token,
      methods: Array.isArray(body.methods) ? (body.methods as string[]) : [],
      recoveryOnlyMethods: Array.isArray(body.recovery_only_methods)
        ? (body.recovery_only_methods as string[])
        : undefined,
      webauthnOptions: body.webauthn_options,
    };
  }
  if (
    body.sso_registration_required === true &&
    typeof body.sso_token === 'string' &&
    typeof body.email === 'string'
  ) {
    return {
      kind: 'sso_token',
      branch: 'new_user',
      ssoToken: body.sso_token,
      email: body.email,
      name: typeof body.name === 'string' && body.name.length > 0 ? body.name : undefined,
    };
  }
  if (
    body.account_link_available === true &&
    typeof body.sso_token === 'string' &&
    typeof body.masked_email === 'string'
  ) {
    return {
      kind: 'sso_token',
      branch: 'account_link',
      ssoToken: body.sso_token,
      maskedEmail: body.masked_email,
    };
  }
  return { kind: 'error', code: 'sso_session_rejected' };
}

/**
 * Flow-failure logger: emits [<prefix>] stage + taxonomy code only.
 * Never logs raw errors (Error.cause may carry token material;
 * [internal]rules/observability.md).
 */
export function logFlowFailure(prefix: string, stage: string, code: string): void {
  console.error(`[${prefix}] sign-in failed at ${stage}: ${code}`);
}

/** Type constraint shared by AppleFlowError and GoogleFlowError. */
export interface FlowError {
  readonly code: SSOSignInErrorCode;
  readonly stage: string;
}

/**
 * Converts an unknown caught value to the provider's error-code taxonomy.
 * @param err       The caught value.
 * @param isFlowErr Predicate returning true when err is the provider's FlowError.
 * @param prefix    Log prefix for this provider (e.g. 'appleFlow').
 */
export function toErrorCode(
  err: unknown,
  isFlowErr: (e: unknown) => e is FlowError,
  prefix: string
): SSOSignInErrorCode {
  if (isFlowErr(err)) {
    logFlowFailure(prefix, err.stage, err.code);
    return err.code;
  }
  const message = err instanceof Error ? err.message : 'unknown';
  if (message === 'oauth_cancelled' || message === 'oauth_timeout') {
    // Loopback teardown paths (renderer cancel, deadline, window close).
    logFlowFailure(prefix, 'loopback', 'sso_cancelled');
    return 'sso_cancelled';
  }
  if (message.startsWith('oauth_provider_error:')) {
    logFlowFailure(prefix, 'loopback', message);
    return message as SSOSignInErrorCode;
  }
  // Unknown failure — fold into the generic flow-setup code. Only the
  // stable message string is logged, never the error object.
  logFlowFailure(prefix, 'unknown', 'sso_initiate_failed');
  return 'sso_initiate_failed';
}
