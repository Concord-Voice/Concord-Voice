import { apiFetch } from '../system/apiClient';

export type ClearFactor = { kind: 'password' | 'mfa'; value: string };

export type ClearHistoryResult =
  | {
      kind:
        | 'success'
        | 'passwordRequired'
        | 'mfaRequired'
        | 'invalidPassword'
        | 'invalidMfaCode'
        | 'stepUpImpossible'
        | 'sessionExpired'
        | 'notFound'
        | 'refused'
        | 'uncertain';
    }
  | { kind: 'rateLimited'; retryAfterSeconds?: number };

export async function hideDMThread(id: string): Promise<boolean> {
  const response = await apiFetch(`/api/v1/dm/conversations/${id}/hide`, { method: 'POST' });
  return response.ok;
}

function clearRequestBody(factor?: ClearFactor): object {
  if (factor?.kind === 'password') return { current_password: factor.value };
  if (factor?.kind === 'mfa') return { mfa_code: factor.value };
  return {};
}

function forbiddenClearResult(
  payload: Record<string, unknown>,
  factor?: ClearFactor
): ClearHistoryResult {
  if (payload.password_required === true && payload.mfa_required === true) {
    return { kind: 'refused' };
  }
  if (payload.password_required === true) return { kind: 'passwordRequired' };
  if (payload.mfa_required === true) return { kind: 'mfaRequired' };
  if (factor?.kind === 'password' && payload.error === 'Invalid password') {
    return { kind: 'invalidPassword' };
  }
  if (factor?.kind === 'mfa' && payload.error === 'Invalid MFA code') {
    return { kind: 'invalidMfaCode' };
  }
  return { kind: 'refused' };
}

function clearResponseResult(
  response: Response,
  payload: Record<string, unknown>,
  id: string,
  factor?: ClearFactor
): ClearHistoryResult {
  if (response.ok) {
    const validReceipt =
      payload.conversation_id === id &&
      typeof payload.cleared_at === 'string' &&
      !Number.isNaN(Date.parse(payload.cleared_at));
    return { kind: validReceipt ? 'success' : 'uncertain' };
  }
  if (response.status === 403) return forbiddenClearResult(payload, factor);
  if (response.status === 429) {
    const seconds = Number.parseInt(response.headers.get('Retry-After') ?? '', 10);
    return {
      kind: 'rateLimited',
      retryAfterSeconds: Number.isFinite(seconds) ? seconds : undefined,
    };
  }
  if (response.status === 401) return { kind: 'sessionExpired' };
  if (response.status === 404) return { kind: 'notFound' };
  if (
    response.status === 400 &&
    typeof payload.error === 'string' &&
    payload.error.startsWith('Clear history requires verification')
  ) {
    return { kind: 'stepUpImpossible' };
  }
  return { kind: response.status >= 500 ? 'uncertain' : 'refused' };
}

export async function clearDMHistory(
  id: string,
  factor?: ClearFactor
): Promise<ClearHistoryResult> {
  let response: Response;
  try {
    response = await apiFetch(`/api/v1/dm/conversations/${id}/clear`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(clearRequestBody(factor)),
    });
  } catch {
    return { kind: 'uncertain' };
  }

  const raw: unknown = await response.json().catch(() => null);
  const payload =
    raw !== null && typeof raw === 'object' && !Array.isArray(raw)
      ? (raw as Record<string, unknown>)
      : {};
  return clearResponseResult(response, payload, id, factor);
}
