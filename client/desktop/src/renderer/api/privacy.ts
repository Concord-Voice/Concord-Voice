/**
 * Privacy API client — wraps the control-plane /api/v1/privacy/* erasure
 * endpoints.
 *
 * PRIVACY CONTRACT: the `clientId` passed to these functions is sensitive
 * transient data. This module MUST NOT:
 *   - log it (no console.* calls)
 *   - persist it (no storage, no Zustand)
 *   - leak it into error messages (see PrivacyApiError — status only)
 *
 * The server upholds the same discipline on the receiving side; see
 * services/control-plane/internal/privacy.
 *
 * The Settings UI that invokes this client is tracked separately in #661.
 */

import { apiFetch } from '../services/apiClient';

export class PrivacyApiError extends Error {
  public readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'PrivacyApiError';
    this.status = status;
  }
}

/**
 * Request full account erasure, optionally scoped to a specific client
 * pairing. Resolves on 204; throws PrivacyApiError on any non-2xx status.
 */
export async function eraseAccount(clientId?: string): Promise<void> {
  const body: Record<string, string> = {};
  if (clientId) {
    body.clientId = clientId;
  }
  const response = await apiFetch('/api/v1/privacy/erase-account', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    throw new PrivacyApiError(
      response.status,
      `erase-account failed with status ${response.status}`
    );
  }
}
