import { apiFetch } from './apiClient';

/**
 * Which servers the viewer shares with another user (#2372).
 *
 * The invite picker reads it to grey a server the person it is about to invite
 * is already in. `null` is NOT "no shared servers" — it means the client could
 * not get an answer (older self-hosted server, rate limit, transport failure).
 * Callers degrade OPEN on it: greying a server you could have invited to is an
 * undiagnosable dead end, while leaving one ungreyed costs a 409 the user can
 * read.
 *
 * Shaped after {@link ./friendEligibility}, which solved the same problem —
 * a per-user probe against a route an older control plane may not have — and
 * whose failure modes were learned the hard way. Diverges in one place, noted
 * at {@link requestMutualServers}.
 */
export type MutualServersResult = readonly string[] | null;

/** How long a settled answer may be trusted. Membership changes mid-session. */
const RESULT_TTL_MS = 5 * 60_000;

/** No response is not an answer — an unsettled probe must not pin the cache. */
const REQUEST_TIMEOUT_MS = 8_000;

/**
 * How long a route-missing verdict suppresses further probes.
 *
 * Bounded rather than permanent for the reason #1241 records: a desktop client
 * runs for days, and one 404 during a rolling restart would otherwise disable
 * greying for the client's whole lifetime.
 */
const UNSUPPORTED_LATCH_MS = 10 * 60_000;

/** userId -> in-flight-or-resolved probe, inserted synchronously before the await. */
const inflight = new Map<string, Promise<MutualServersResult>>();

/** Settled answers, so a repeat open costs nothing inside the TTL. */
const resolved = new Map<string, { servers: readonly string[]; at: number }>();

let unsupportedUntil = 0;

/**
 * Bumped by {@link clearMutualServersCache}. A probe already in flight when an
 * account switch happens resolves AFTER the clear, and would otherwise write
 * account A's shared-server list into account B's session on the same process.
 */
let identityGeneration = 0;

export function clearMutualServersCache(): void {
  identityGeneration += 1;
  inflight.clear();
  resolved.clear();
  unsupportedUntil = 0;
}

/**
 * One probe. Every failure degrades to `null`.
 *
 * **A 404 here is unambiguous, and that is a consequence of the route's privacy
 * design rather than luck.** `friendEligibility` has to inspect the 404 BODY to
 * tell "this server has no such route" from "this server has the route and the
 * user does not exist", because that endpoint answers 404 for an unknown user
 * and latching on the wrong one disables its gate process-wide. This route
 * answers `200 {"server_ids": []}` for an unknown user — deliberately, so it
 * cannot be used as a user-existence oracle — so it never 404s for a reason of
 * its own, and any 404 is the route being absent. Do not add body matching here
 * "for symmetry": it would be matching on a shape this endpoint never emits.
 */
async function requestMutualServers(userId: string, gen: number): Promise<MutualServersResult> {
  try {
    const response = await apiFetch(`/api/v1/users/${userId}/mutual-servers`, {
      // A half-open socket (captive portal, corporate proxy, OS suspend) never
      // settles, and without this the promise stays pinned in `inflight` and the
      // picker waits on it forever.
      signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
    });
    if (response.status === 404) {
      // Fenced on the generation: a 404 from the PREVIOUS session's server must
      // not latch the next one.
      if (gen === identityGeneration) unsupportedUntil = Date.now() + UNSUPPORTED_LATCH_MS;
      return null;
    }
    if (!response.ok) return null;
    const data: { server_ids?: unknown } = await response.json();
    // Strict. A 200 whose body lacks the key — a gateway shim, an SSO
    // interstitial, a future envelope change — is `null` and degrades open, not
    // an empty list that would quietly mean "shares nothing".
    if (!Array.isArray(data.server_ids)) return null;
    return data.server_ids.filter((id): id is string => typeof id === 'string');
  } catch {
    return null;
  }
}

/**
 * Shared servers with `userId`, cached and deduped.
 *
 * The promise is inserted into `inflight` SYNCHRONOUSLY before the await, which
 * is load-bearing rather than an optimisation: the picker asks about every
 * recipient of a group DM in one pass, and a value-keyed cache cannot dedupe
 * that first burst.
 */
export function getMutualServers(userId: string): Promise<MutualServersResult> {
  if (Date.now() < unsupportedUntil) return Promise.resolve(null);

  const settled = resolved.get(userId);
  if (settled && Date.now() - settled.at < RESULT_TTL_MS) {
    return Promise.resolve(settled.servers);
  }

  const existing = inflight.get(userId);
  if (existing) return existing;

  const gen = identityGeneration;
  const probe = requestMutualServers(userId, gen).then((result) => {
    // A continuation that outlived its account writes nothing — and evicts
    // nothing either. The delete sits BELOW this guard on purpose: the only
    // thing that empties `inflight` is `clearMutualServersCache`, which bumps
    // the generation FIRST, so a stale continuation reaching an unconditional
    // delete would remove the SUCCESSOR probe a new account had already
    // inserted under the same key. That successor's own result stays correct
    // (this fence guards the write, not the map), but its dedupe promise is
    // gone, so the next caller opens a third request on the one route whose
    // rate-limit budget the group-DM fan-out actually stresses.
    if (gen !== identityGeneration) return null;
    inflight.delete(userId);
    if (result !== null) resolved.set(userId, { servers: result, at: Date.now() });
    return result;
  });
  inflight.set(userId, probe);
  return probe;
}
