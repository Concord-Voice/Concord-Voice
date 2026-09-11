/**
 * An async barrier for tests that assert a COUNT reached across a MessagePort.
 *
 * `await new Promise((r) => setTimeout(r, 0))` is not a barrier for port
 * delivery, and the audiocap child tests were built on it. The chain a credit
 * drives is two hops: the test posts to the child's port end, the child's
 * handler drains and posts N quanta back, and only then does the test's own
 * `onmessage` run N times. The `setTimeout(0)` is queued BEFORE any of those
 * N posts exist, so whether it fires after them is a scheduling question, not
 * an ordering guarantee — it holds on an idle machine and stops holding on a
 * loaded CI runner.
 *
 * Observed as a red `main` on PR #3261's first CI attempt (run 34538229277
 * attempt 1): `expected +0 to be 8`. Not an off-by-one — ZERO posts had been
 * delivered when the timer fired. Reproduced locally at 1/12 under CPU
 * saturation and 0/15 unloaded, which is the signature of this race rather
 * than of a wrong expected value.
 *
 * TWO PHASES, and both are load-bearing:
 *
 *  1. A POSITIVE gate — wait until the count REACHES `atLeast`. Quiescence
 *     alone cannot do this: a value that has not started moving is indistinguishable
 *     from one that has finished, and "0, 0, 0" reads as settled. This is the
 *     phase whose absence caused the flake.
 *  2. QUIESCENCE — then let further work land, so a following exact-equality or
 *     emptiness assertion is not merely early. Without it, `toBe(8)` passes on a
 *     build that posts nine, because the ninth had not arrived yet — the test
 *     goes vacuous exactly when the property it guards is broken.
 *
 * This is the "supply the missing gate" shape `[internal]rules/tests.md` requires,
 * NOT `waitFor` on a negative: the assertion is still made once, synchronously,
 * by the caller. Nothing here retries an expectation until it stops throwing.
 */

const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

export interface SettledOptions {
  /** Consecutive unchanged turns that count as quiescent. */
  quietTurns?: number;
  /** Overall budget for BOTH phases. */
  timeoutMs?: number;
}

/**
 * Resolve once `count()` has reached `atLeast` AND then stopped changing.
 *
 * Throws — naming the observed value — rather than resolving early, so a
 * genuine regression fails with a diagnosis instead of reappearing as a flake.
 *
 * Pass `atLeast: 0` when the expected final count is zero and some EARLIER
 * positive gate has already established that the system acted; phase 1 is then
 * trivially satisfied and only quiescence applies.
 */
export async function settled(
  count: () => number,
  atLeast: number,
  { quietTurns = 5, timeoutMs = 5_000 }: SettledOptions = {}
): Promise<void> {
  const deadline = Date.now() + timeoutMs;

  while (count() < atLeast) {
    if (Date.now() > deadline) {
      throw new Error(
        `settled(): timed out after ${timeoutMs}ms waiting for at least ${atLeast}; observed ${count()}`
      );
    }
    await tick();
  }

  let last = count();
  let stable = 0;
  while (stable < quietTurns) {
    await tick();
    const now = count();
    if (now === last) {
      stable += 1;
    } else {
      last = now;
      stable = 0;
    }
    if (Date.now() > deadline) {
      throw new Error(
        `settled(): still changing after ${timeoutMs}ms; last observed ${now} (never quiet for ${quietTurns} turns)`
      );
    }
  }
}
