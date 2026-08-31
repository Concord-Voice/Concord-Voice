/**
 * Auth-admission gate predicate (#2346).
 *
 * App's passive "/" route navigates a just-authenticated session from the auth
 * screen into the app. Password login publishes the access token BEFORE it
 * unwraps E2EE keys, so without a hold the route would navigate immediately and
 * unmount Login before an inline unwrap failure could surface the consented
 * key-recovery prompt (Login-local state) — stranding the user
 * authenticated-but-undecryptable.
 *
 * `pendingE2EEUnlockGeneration` (authStore) carries the `authGeneration` of a
 * session whose inline E2EE unlock is still pending. The hold is GENERATION-
 * BOUND: it applies only while that value equals the current `authGeneration`,
 * so a superseded or aborted flow's stale value can never gate a successor
 * session (SSO / session-restore navigate via the same route, and any token
 * change bumps `authGeneration` past it). Aligns with the owner/generation-bound
 * admission invariant tracked for the SSO paths in #2424.
 *
 * Pure so both App.tsx (the live gate) and the router-level regression test
 * consume the identical predicate — no drifting copy of the condition.
 */
export function isE2EEUnlockPending(
  pendingE2EEUnlockGeneration: number | null,
  authGeneration: number
): boolean {
  return pendingE2EEUnlockGeneration !== null && pendingE2EEUnlockGeneration === authGeneration;
}
