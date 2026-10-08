import { revokeAbortedSession } from '../../services/system/apiClient';

/**
 * Revocation of a session a sign-in answer issued but the client will not
 * keep: the login page's malformed or abandoned completions, and a modal
 * verify accepted after the server selection moved. One copy, so both
 * surfaces read the same evidence the same way.
 */

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function malformedLoginSession(data: unknown, apiBase: string) {
  const record = isRecord(data) ? data : {};
  return {
    accessToken: typeof record.access_token === 'string' ? record.access_token : null,
    refreshToken: typeof record.refresh_token === 'string' ? record.refresh_token : null,
    sessionId: typeof record.session_id === 'string' ? record.session_id : null,
    apiBase,
  };
}

export function responseIssuedSessionID(response: Response): string | null {
  return response.headers?.get('X-Concord-Session-ID')?.trim() || null;
}

export async function revokeMalformedLoginSession(
  data: unknown,
  apiBase: string,
  issuedSessionID: string | null
): Promise<void> {
  const session = malformedLoginSession(data, apiBase);
  // When present, the response header is the backend's authoritative refresh
  // row ID. Prefer it to any partially decoded body value; this is precisely
  // the malformed-success path the header exists to recover.
  if (issuedSessionID !== null) session.sessionId = issuedSessionID;
  if (
    session.refreshToken !== null ||
    (session.accessToken !== null && session.sessionId !== null)
  ) {
    await revokeAbortedSession(session);
  } else if (issuedSessionID !== null) {
    await revokeAbortedSession({
      accessToken: null,
      sessionId: issuedSessionID,
      cookieBound: true,
      apiBase,
    });
  }
}
