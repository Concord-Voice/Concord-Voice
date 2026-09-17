import { apiFetch } from '../system/apiClient';

/**
 * Fetch public keys for a list of user IDs.
 *
 * `keys` maps userId → publicKey and `versions` maps userId → that key's
 * `key_version`. The versions are what activate #2420's recipient-freshness
 * guard: the server runs `recipientKeyFresh` only for recipients named in
 * `wrapped_key_versions`, so omitting them makes every insert take the
 * fail-open branch and a wrap against a since-rotated identity key is stored
 * with no self-heal row enqueued. `GET /public-key` already returns the field
 * and this function used to discard it.
 *
 * `missing` names the users whose fetch failed. A partial map is otherwise
 * indistinguishable from a complete one, so a participant whose request 500'd
 * silently gets no wrapped key and the distribution still reports success.
 */
export interface ParticipantPublicKeys {
  keys: Map<string, string>;
  versions: Map<string, number>;
  missing: string[];
}

export async function fetchParticipantPublicKeys(
  userIds: string[]
): Promise<ParticipantPublicKeys> {
  const results = await Promise.allSettled(
    userIds.map(async (userId) => {
      const pkRes = await apiFetch(`/api/v1/users/${userId}/public-key`);
      if (!pkRes.ok) return { userId, publicKey: null };
      const pkData = await pkRes.json();
      return {
        userId,
        publicKey: (pkData.public_key as string | undefined) ?? null,
        keyVersion:
          // Retain a version only when it is a POSITIVE safe integer. A key
          // version is >= 1 by construction (schema default 1, increment-only),
          // so 0, a negative, or a non-integer is anomalous input; treating it
          // as absent lets both rotation guards refuse it up front instead of
          // posting a wrap the server rejects as a stale recipient.
          typeof pkData.key_version === 'number' &&
          Number.isSafeInteger(pkData.key_version) &&
          pkData.key_version > 0
            ? (pkData.key_version as number)
            : undefined,
      };
    })
  );
  const keys = new Map<string, string>();
  const versions = new Map<string, number>();
  const missing: string[] = [];
  for (const [i, r] of results.entries()) {
    if (r.status === 'fulfilled' && r.value.publicKey) {
      keys.set(r.value.userId, r.value.publicKey);
      if (r.value.keyVersion !== undefined) versions.set(r.value.userId, r.value.keyVersion);
    } else {
      missing.push(r.status === 'fulfilled' ? r.value.userId : userIds[i]);
    }
  }
  return { keys, versions, missing };
}
