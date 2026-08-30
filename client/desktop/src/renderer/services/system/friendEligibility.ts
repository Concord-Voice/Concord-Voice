import { apiFetch } from './apiClient';

/**
 * Whether the viewer may send this user a friend request (#1241).
 *
 * `unknown` is NOT a third privacy state — it means the client could not get an
 * answer (old server, rate limit, transport failure). Callers degrade OPEN on
 * it: the server is the only authority, so showing a button that may 403 is a
 * recoverable annoyance, while hiding one that would have worked is an
 * undiagnosable dead end.
 */
export type EligibilityVerdict = 'eligible' | 'ineligible' | 'unknown';

/**
 * The 404 body a server that HAS this route returns for an unknown user.
 *
 * This is an unlinked cross-service contract: the producer is
 * `services/control-plane/internal/friends/handlers.go`. Rewording it there
 * without changing it here sends the first stale member-list entry down the
 * route-missing branch and disables the gate for the whole latch window. Kept
 * as a named constant so the coupling is greppable from both ends.
 */
const ROUTE_PRESENT_404_ERROR = 'User not found';

/**
 * userId -> in-flight-or-resolved verdict.
 *
 * The promise is inserted SYNCHRONOUSLY before the await. That is load-bearing,
 * not an optimisation: `MemberProfileCard` renders `SendFriendRequestButton`, so
 * `useFriendRequestState` runs twice concurrently for the same userId in one
 * render tree, and a boolean-valued cache cannot dedupe that first pair.
 */
const cache = new Map<string, Promise<EligibilityVerdict>>();

/**
 * Settled verdicts, so {@link peekEligibility} can answer without awaiting.
 *
 * INVARIANT: `resolved` holds a key iff `cache` holds it AND it settled to a
 * real verdict. A settled `unknown` is evicted from `cache` and never entered
 * here. All module state resets together in {@link clearFriendEligibilityCache}.
 */
const resolved = new Map<string, { verdict: EligibilityVerdict; at: number }>();

/**
 * Set once a control-plane answers 404 — it predates #1240 and has no such
 * route. Process-wide, or a member-list scroll fires one doomed request per
 * card. Keyed on HTTP status ALONE: a pre-#1240 server's NoRoute body is not
 * part of any contract, so matching on body shape would be guessing.
 */
/**
 * When the route-missing latch expires. AR-6 accepts a gate bypass lasting a
 * DEPLOY, not a session — but a desktop client runs for days, and the latch was
 * cleared only by logout, so one 404 during a rolling restart disabled the gate
 * for the client's whole lifetime. Expiring it keeps the stampede protection the
 * latch exists for (one doomed probe per window, not one per card) while letting
 * the gate re-arm on its own.
 */
let unsupportedUntil = 0;
const UNSUPPORTED_LATCH_MS = 10 * 60_000;

/**
 * How long a settled verdict may be trusted.
 *
 * Eligibility under `mutual_servers` is a function of shared-server membership,
 * which changes mid-session: a viewer told `ineligible`, who then joins a server
 * the target is in, would otherwise keep the affordance hidden until logout — a
 * hide no longer backed by a current authoritative `false`. Bounded staleness
 * beats unbounded.
 */
const VERDICT_TTL_MS = 5 * 60_000;

/** No response is also not a verdict — an unsettled probe must not pin the cache. */
const ELIGIBILITY_TIMEOUT_MS = 8_000;

/**
 * Identity generation, bumped by {@link clearFriendEligibilityCache}.
 *
 * Clearing is synchronous but a probe already in flight is not: its `.then`
 * continuation runs AFTER the clear and would write straight back into the
 * structures the clear just emptied. That is a cross-account leak — account A's
 * verdict, and account A's server's 404 latch, surviving into account B's
 * session on the same process. Every continuation captures the generation it
 * started under and writes nothing if the identity has since changed.
 */
let identityGeneration = 0;

async function requestVerdict(userId: string, gen: number): Promise<EligibilityVerdict> {
  try {
    const response = await apiFetch(`/api/v1/users/${userId}/friend-request-eligibility`, {
      // Without this a half-open socket (captive portal, corporate proxy, an
      // OS suspend/resume) never settles, the promise stays pinned in `cache`,
      // and every inline surface hides the affordance for that user for the
      // rest of the session — on something that is not an authoritative
      // `false`. The abort lands in the catch below and degrades open.
      signal: AbortSignal.timeout(ELIGIBILITY_TIMEOUT_MS),
    });
    if (response.status === 404) {
      // A 404 means two DIFFERENT things and conflating them defeats the whole
      // gate. A server that HAS this route answers 404 {"error":"User not
      // found"} for an unknown user (spec §6.1, and AR-1 records account
      // existence as deliberately distinguishable). A control-plane predating
      // #1240 has no such route at all. Latching on the first kind disabled the
      // presentation gate process-wide for the rest of the session because ONE
      // member-list entry was stale — and member and voice-participant lists
      // routinely lag actual account state.
      //
      // So match the CONTRACTUAL shape and treat its absence as route-missing.
      // That is the inverse of guessing at an old server's body: we assert only
      // what our own spec guarantees, and an unrecognised 404 is the evidence
      // that this is not our endpoint.
      const body: { error?: unknown } | null = await response.json().catch(() => null);
      if (body?.error !== ROUTE_PRESENT_404_ERROR && gen === identityGeneration) {
        // Fenced on the generation too: a 404 from the PREVIOUS session's
        // server must not latch the gate off for the next one.
        unsupportedUntil = Date.now() + UNSUPPORTED_LATCH_MS;
      }
      return 'unknown';
    }
    if (!response.ok) return 'unknown';
    const data: { eligible?: unknown } = await response.json();
    // Strict, both ways. The contract is that ONLY an explicit `false` hides the
    // affordance, so a 200 whose body lacks the key — a gateway shim, an SSO
    // interstitial, a future envelope change — must read as `unknown` and
    // degrade OPEN. A truthiness test would have mapped absent/null/0/"" onto
    // `ineligible`, which is the one fail-CLOSED hole in an otherwise
    // fail-open matrix.
    if (data.eligible === true) return 'eligible';
    if (data.eligible === false) return 'ineligible';
    return 'unknown';
  } catch {
    // No response at all. Nothing is known, so nothing is hidden.
    return 'unknown';
  }
}

export function fetchEligibility(userId: string): Promise<EligibilityVerdict> {
  if (Date.now() < unsupportedUntil) return Promise.resolve('unknown');

  // A verdict past its TTL is discarded rather than served, so a hide cannot
  // outlive the shared-server membership that produced it.
  const settled = resolved.get(userId);
  if (settled && Date.now() - settled.at >= VERDICT_TTL_MS) {
    resolved.delete(userId);
    cache.delete(userId);
  }

  const cached = cache.get(userId);
  if (cached) return cached;

  const gen = identityGeneration;
  const inFlight = requestVerdict(userId, gen).then((verdict) => {
    // The session that started this probe is gone. Hand the verdict back to the
    // original caller (whose component is unmounting anyway) but write nothing
    // to module state — the next account must start from an empty cache.
    if (gen !== identityGeneration) return verdict;

    if (verdict === 'unknown') {
      // Not a verdict — a rate-limited or failed probe must not stick, or one
      // 429 during a member-list scroll would pin that user as unknown for the
      // whole session. Evicting lets the next mount retry.
      cache.delete(userId);
    } else {
      resolved.set(userId, { verdict, at: Date.now() });
    }
    return verdict;
  });

  cache.set(userId, inFlight);
  return inFlight;
}

/** Warm the cache without awaiting — used on context-menu open-intent. */
export function prefetchEligibility(userId: string): void {
  void fetchEligibility(userId);
}

/**
 * Synchronous cache read. Never issues a request, so it is safe in a render
 * path. `pending` means "not known yet", including "never asked".
 */
export function peekEligibility(userId: string): EligibilityVerdict | 'pending' {
  if (Date.now() < unsupportedUntil) return 'unknown';
  const settled = resolved.get(userId);
  if (!settled) return 'pending';
  // Expired reads as not-yet-known, so the caller re-probes rather than acting
  // on a verdict whose cause may no longer hold.
  return Date.now() - settled.at < VERDICT_TTL_MS ? settled.verdict : 'pending';
}

/**
 * Registered in `resetService` — a module-scope cache that survives logout
 * would serve the previous account's verdicts to the next user on a shared
 * device (risk: privacy, cross-account leak).
 */
export function clearFriendEligibilityCache(): void {
  // Bump FIRST: any continuation that lands after this point is fenced out.
  identityGeneration++;
  cache.clear();
  resolved.clear();
  unsupportedUntil = 0;
}
