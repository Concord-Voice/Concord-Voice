/**
 * Reading the answer of an MFA enrolment request that is not step-up-gated
 * (TOTP verify-setup and confirm-setup, security-key register finish, email
 * verify). The gated routes go through `mfaStepUp.ts` instead.
 */

/**
 * A refused answer's `error` text, or `fallback`. A body that is not JSON — a
 * proxy's HTML error page — reads as `{}`, never as the parse error.
 */
export async function refusalText(res: Response, fallback: string): Promise<string> {
  const body: unknown = await res.json().catch(() => ({}));
  if (typeof body !== 'object' || body === null || !('error' in body)) return fallback;
  return typeof body.error === 'string' && body.error !== '' ? body.error : fallback;
}
