/**
 * The post-rotation continuation token pair (#2201 / PR #2397), appended
 * best-effort by the server to a committed ChangePassword or ReplaceMyKeys
 * response (`internal/users/handlers.go:1630`).
 *
 * ABSENCE IS A SECURITY OUTCOME, NOT AN ERROR. The server omits the fields when
 * a concurrent destructive flow advanced the credential epoch in the post-commit
 * window, deliberately withholding a session that the later flow intended to
 * terminate. Callers must fail closed to re-authentication and must NEVER retry
 * — the epoch signal is unrecoverable from any later 401, whose body is
 * deliberately generic so it cannot serve as an epoch oracle
 * (`internal/middleware/auth.go:70-74`).
 */
export interface ContinuationPair {
  accessToken: string;
  refreshToken: string;
  sessionId: string;
}

function nonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.length > 0;
}

/**
 * All-or-nothing. `appendContinuationPair` sets all three fields or none, so a
 * partial set means a proxy stripped a field or the server is version-skewed —
 * both fail closed.
 *
 * Never throws. The ChangePassword caller sits inside a fail-closed `try` whose
 * `catch` maps to a DIFFERENT diagnostic (`userStore.ts` → `committed-unreadable`),
 * so a throw here would misattribute a deliberately-absent pair to an unreadable
 * response body and destroy that distinction.
 */
export function parseContinuationPair(data: Record<string, unknown>): ContinuationPair | null {
  const accessToken = data.access_token;
  const refreshToken = data.refresh_token;
  const sessionId = data.session_id;
  if (!nonEmptyString(accessToken) || !nonEmptyString(refreshToken) || !nonEmptyString(sessionId)) {
    return null;
  }
  return { accessToken, refreshToken, sessionId };
}
