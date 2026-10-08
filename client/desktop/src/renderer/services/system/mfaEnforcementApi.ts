/**
 * The server's "require MFA for dangerous actions" setting (#3453, #3456 §3.5):
 *
 *   GET /api/v1/servers/:id/mfa-enforcement
 *   PUT /api/v1/servers/:id/mfa-enforcement   {enabled, mfa_code?}
 *
 * The owner and a raw-bit Administrator may read or change it; anyone else is
 * refused with the same 403 as any permission refusal, which is why a 403 and
 * a 404 (a server that predates the route) both read as "not yours to see".
 *
 * Neither function throws. Neither logs, and nothing here records whether an
 * account is enrolled or a server enforces: that is posture.
 */

import { apiFetch } from './apiClient';
import { apiFetchInContext, isAbortError, type ApiRequestContext } from './requestContext';

export function mfaEnforcementPath(serverId: string): string {
  return `/api/v1/servers/${encodeURIComponent(serverId)}/mfa-enforcement`;
}

export type MfaEnforcementRead =
  | { kind: 'ok'; enforcing: boolean }
  /** 403 or 404: this member may not see the setting, or the server has no such route. */
  | { kind: 'absent' }
  /** Any other status, a transport failure, or a 200 whose body is not the setting. */
  | { kind: 'unavailable' }
  /** The signal, or `apiFetch`'s pre-dispatch fence, cancelled it. Discard; never render. */
  | { kind: 'aborted' };

/** The `enforce_mfa_dangerous_actions` of a 200 body, or null when it is not a boolean. */
async function enforcingOf(res: Response): Promise<boolean | null> {
  const body: unknown = await res.json().catch(() => null);
  if (typeof body !== 'object' || body === null || !('enforce_mfa_dangerous_actions' in body)) {
    return null;
  }
  return typeof body.enforce_mfa_dangerous_actions === 'boolean'
    ? body.enforce_mfa_dangerous_actions
    : null;
}

async function readResult(res: Response): Promise<MfaEnforcementRead> {
  if (res.status === 403 || res.status === 404) return { kind: 'absent' };
  if (!res.ok) return { kind: 'unavailable' };
  const enforcing = await enforcingOf(res);
  return enforcing === null ? { kind: 'unavailable' } : { kind: 'ok', enforcing };
}

/**
 * Reads the setting. `apiFetch` throws an AbortError when `signal` aborts and
 * before dispatch when the account or server changed under it; both are
 * `aborted`. Any other rejection is `unavailable`.
 */
export async function fetchMfaEnforcement(
  serverId: string,
  signal: AbortSignal
): Promise<MfaEnforcementRead> {
  try {
    const res = await apiFetch(mfaEnforcementPath(serverId), { method: 'GET', signal });
    const result = await readResult(res);
    return signal.aborted ? { kind: 'aborted' } : result;
  } catch (err) {
    return signal.aborted || isAbortError(err) ? { kind: 'aborted' } : { kind: 'unavailable' };
  }
}

export interface MfaEnforcementUpdate {
  enabled: boolean;
  /** The confirmation, for an OFF only. Omitted or empty, no code is sent. */
  mfaCode?: string;
}

/**
 * What a PUT came to. The same four kinds `DangerousActionStepUpDialog`'s
 * `send` resolves to, so the OFF confirmation passes this straight through;
 * the type is restated here because a service does not import a component.
 */
export type MfaEnforcementWrite =
  | { kind: 'ok' }
  /** Any non-2xx, with its parsed body (null when it did not parse). */
  | { kind: 'refused'; status: number; body: unknown }
  /** No response arrived. The server may or may not have acted. */
  | { kind: 'transport' }
  /** The request never left: the account or server changed since `context`. */
  | { kind: 'aborted' };

/**
 * Turns the setting on or off, admitted against `context`. The code is sent
 * only when non-empty: an empty string reads to the server as a supplied and
 * wrong code, and is charged to the step-up budget as one.
 */
export async function putMfaEnforcement(
  serverId: string,
  { enabled, mfaCode }: MfaEnforcementUpdate,
  context: ApiRequestContext
): Promise<MfaEnforcementWrite> {
  const payload = mfaCode ? { enabled, mfa_code: mfaCode } : { enabled };
  try {
    const res = await apiFetchInContext(
      mfaEnforcementPath(serverId),
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      },
      context
    );
    if (res.ok) return { kind: 'ok' };
    const body: unknown = await res.json().catch(() => null);
    return { kind: 'refused', status: res.status, body };
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'transport' };
  }
}
