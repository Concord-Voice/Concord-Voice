/**
 * The SPA cold-start timing budget, shared by the loader that spends it and the
 * deep-link carry fence that must outlast it (#2363).
 *
 * Its own module because both consumers need the real numbers at import time and
 * `spaLoader` is mocked wholesale by several main-process test files — deriving
 * DEEP_LINK_CARRY_MAX_AGE_MS from a mocked export made it NaN under test. A
 * module with no imports of its own is never worth mocking, so both sides always
 * see the same values and the derivation cannot silently drift.
 */

/**
 * How long one `resolveSpaSource()` attempt may spend producing a decision.
 *
 * A budget for the WHOLE operation, headers and body — `fetchJsonWithTimeout`
 * holds its deadline open across `response.json()`. That is what makes the
 * derivation below arithmetic rather than a guess: a version that stopped the
 * clock when headers arrived left one attempt unbounded, and the fence derived
 * from it meaningless (#2363, Codex).
 */
export const CONFIG_TIMEOUT_MS = 5_000;

/**
 * Delays before each cold-start retry of `resolveSpaSource()`. SEQUENTIAL, and
 * each attempt first awaits the fetch above AND an `applySpaDecision` whose
 * `captureSpaHash` is bounded by the same timeout — and the chain advances only
 * as each `applySpaDecision` RESOLVES. So the worst case is
 * `sum(delays) + (2 * delays.length + 1) * CONFIG_TIMEOUT_MS`, the extra one
 * being the hash capture before the chain starts.
 *
 * Keep this in step with `DEEP_LINK_CARRY_MAX_AGE_MS`, which derives from it.
 * An earlier version of this comment stopped at `delays.length *
 * CONFIG_TIMEOUT_MS`, and a reader re-deriving the fence from it reproduces the
 * exact under-sizing that made the fence decline a live invite.
 */
export const SPA_RETRY_DELAYS_MS = [4_000, 10_000];
