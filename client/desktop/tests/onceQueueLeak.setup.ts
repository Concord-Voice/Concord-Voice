/**
 * Always-on guard for `*Once` mock-queue leakage across test boundaries.
 *
 * It runs in every test run. That is deliberate: an opt-in detector nobody
 * invokes is the failure class this repo has already recorded twice (see
 * [internal] on #3198's `status()`, "computed on every callback and observed by
 * nobody", and #3195 deleting an unwired watchdog rather than shipping it
 * inert). Measured cost at introduction: within run-to-run noise on a 697-file
 * suite, and zero false positives across 15,469 tests.
 *
 * `vi.clearAllMocks()` clears call history but does not drain the queue that
 * `mockResolvedValueOnce` and friends push onto, and `clearMocks` is pinned
 * `false` in `vite.config.ts`. A value a test queues and never consumes is
 * therefore served to the NEXT test's first call on that mock, ahead of that
 * test's own fixture — which then goes unread, so the test passes off a value
 * it did not write. See `[internal]rules/tests.md` § The `*Once` queue outlives
 * the test that queued it.
 *
 * Every `*Once` variant reaches `mock.mockImplementationOnce` by property
 * lookup, so patching that one property catches all of them. Detection is
 * observed, not inferred: the queued implementation is wrapped, and it records
 * the test that queued it. When it finally RUNS, a different current test means
 * the value crossed a boundary. Nothing else is tracked — `mockReset()` drops
 * the wrapper from vitest's own queue, so a drained value simply never runs.
 *
 * Deliberately NOT reported:
 *  - Queued-but-never-consumed. That is the common, harmless precondition;
 *    reporting it would bury the real finding (178 occurrences at introduction).
 *  - Anything queued outside a test body (module scope, `beforeAll`). Its first
 *    legitimate consumer is by definition a different "test" than the one that
 *    queued it, so reporting it would be a false positive — and the remedy this
 *    guard prescribes, draining in `beforeEach`, would DELETE a `beforeAll`
 *    fixture rather than fix anything. The cost is a blind spot: a `beforeAll`
 *    value consumed by the wrong test is not caught.
 *
 * Known blind spots, none of which produce a false positive: `withImplementation`
 * swaps vitest's once-queue wholesale; a value consumed by an async continuation
 * after its test ended is attributed to the next test; and `where` is module
 * scope, so `it.concurrent` would interleave it (the suite has zero uses today).
 */
import { afterEach, beforeEach, vi } from 'vitest';

// Escape hatch, exact match so `ONCE_LEAK_GUARD_OFF=0` cannot silently disable it.
if (process.env.ONCE_LEAK_GUARD_OFF !== '1') {
  const OUTSIDE = '<outside any test>';
  const marker = globalThis as Record<string, unknown>;
  marker.__onceQueueLeakGuardInstrumented = 0;

  const INSTRUMENTED = new WeakSet<object>();
  const crossings: string[] = [];
  let where = OUTSIDE;

  /** First stack frame that belongs to the suite rather than to vitest or this file. */
  const siteOf = (err: Error): string =>
    (err.stack ?? '')
      .split('\n')
      .slice(1)
      .find((l) => !/onceQueueLeak|node_modules|node:internal/.test(l))
      ?.trim() ?? '<no stack>';

  const instrument = <T>(mock: T): T => {
    const m = mock as T & { mockImplementationOnce?: (impl?: unknown) => unknown };
    if (
      typeof m !== 'function' ||
      INSTRUMENTED.has(m) ||
      typeof m.mockImplementationOnce !== 'function'
    ) {
      return mock;
    }
    INSTRUMENTED.add(m);
    // Counts ATTACHMENTS, read by tests/unit/services/onceQueueLeakGuard.test.ts.
    // Deliberately not a boolean set at module level: that would prove only that
    // this block ran, so removing instrument() from either factory below would
    // leave the self-test green and the guard detecting nothing. The counter can
    // only move from in here.
    marker.__onceQueueLeakGuardInstrumented =
      ((marker.__onceQueueLeakGuardInstrumented as number) ?? 0) + 1;

    const originalOnce = m.mockImplementationOnce.bind(m);
    m.mockImplementationOnce = (impl?: (...args: unknown[]) => unknown) => {
      const site = siteOf(new Error());
      const queuedIn = where;
      return originalOnce(function consumeOnce(this: unknown, ...args: unknown[]) {
        if (queuedIn !== OUTSIDE && where !== queuedIn) {
          crossings.push(
            `  queued at : ${site}\n  queued by : ${queuedIn}\n  consumed  : ${where}`
          );
        }
        return impl?.apply(this, args);
      });
    };
    return mock;
  };

  const realFn = vi.fn.bind(vi);
  const realSpyOn = vi.spyOn.bind(vi);
  vi.fn = ((...args: Parameters<typeof realFn>) =>
    instrument(realFn(...args))) as unknown as typeof vi.fn;
  vi.spyOn = ((...args: Parameters<typeof realSpyOn>) =>
    instrument(realSpyOn(...args))) as unknown as typeof vi.spyOn;

  beforeEach((ctx) => {
    where = `${ctx.task.file?.name ?? '?'} :: ${ctx.task.name}`;
    // Deliberately no `crossings` reset here: afterEach drains it, and clearing
    // again would discard a crossing recorded BETWEEN two tests (a straggling
    // timer or promise) instead of attributing it to the next one.
  });

  afterEach(() => {
    const found = crossings.splice(0);
    where = OUTSIDE;
    if (found.length) {
      throw new Error(
        `A mock's *Once queue leaked across a test boundary: this test consumed ` +
          `${found.length} value(s) queued by an earlier test, so its own fixture for ` +
          `that mock was never read.\n\n${found.join('\n\n')}\n\n` +
          `Fix: mockReset() that mock in beforeEach, plus an explicit default. See ` +
          `[internal]rules/tests.md § The *Once queue outlives the test that queued it.`
      );
    }
  });
}
