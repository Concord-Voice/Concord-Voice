import { net } from 'electron';
import { collectAttestationSignals, AttestationSignals } from './attestationSignals';

let cachedToken: { token: string; expiresAt: Date } | null = null;

interface VerifyResponseBody {
  attestation_token: string;
  expires_at: string;
}

export class AttestationError extends Error {
  constructor(
    public status: number,
    public code: string,
    public body: Record<string, unknown>
  ) {
    super(`Attestation failed: ${code}`);
  }
}

export async function attest(opts: {
  apiBaseUrl: string;
  jwt: string;
  sessionId: string;
  platform: AttestationSignals['platform'];
  version: string;
}): Promise<string> {
  const signals = await collectAttestationSignals({
    platform: opts.platform,
    version: opts.version,
  });
  const res = await net.fetch(`${opts.apiBaseUrl}/api/v1/attestation/verify`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${opts.jwt}`,
      'X-Session-ID': opts.sessionId,
      'X-Machine-Id': signals.machine_id ?? '',
    },
    body: JSON.stringify(signals),
  });

  if (!res.ok) {
    const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
    const code = typeof body.code === 'string' ? body.code : 'ATTESTATION_ERROR';
    throw new AttestationError(res.status, code, body);
  }

  const data = (await res.json()) as Partial<VerifyResponseBody>;
  if (typeof data.attestation_token !== 'string' || typeof data.expires_at !== 'string') {
    throw new AttestationError(res.status, 'ATTESTATION_MALFORMED_RESPONSE', { ...data });
  }

  const expiresAt = new Date(data.expires_at);
  // Fail-closed: a malformed expires_at (NaN) must never produce a cached token
  // that getAttestationToken() treats as perpetually valid (NaN < Date.now() === false).
  if (Number.isNaN(expiresAt.getTime())) {
    throw new AttestationError(res.status, 'ATTESTATION_MALFORMED_RESPONSE', {
      reason: 'expires_at is not a valid date',
      expires_at: data.expires_at,
    });
  }

  cachedToken = { token: data.attestation_token, expiresAt };
  return data.attestation_token;
}

export function getAttestationToken(): string | null {
  if (!cachedToken) return null;
  // Belt-and-suspenders: treat NaN expiry as expired so the getter never
  // fails open even if a malformed date somehow reached the cache. The
  // boundary uses <= so the exact expiry instant reads as expired, not valid.
  if (
    Number.isNaN(cachedToken.expiresAt.getTime()) ||
    cachedToken.expiresAt.getTime() <= Date.now()
  ) {
    cachedToken = null;
    return null;
  }
  return cachedToken.token;
}

export function clearAttestationToken(): void {
  cachedToken = null;
}
