/**
 * Self-test for the always-on `*Once` leak guard (tests/onceQueueLeak.setup.ts).
 *
 * The guard cannot prove itself: it reports by THROWING, so a committed leaky
 * fixture would redden the suite. What it can do is fail open — if a future
 * @vitest/spy stops routing `*Once` through `mock.mockImplementationOnce`, the
 * patch attaches to nothing, every run stays green, and the detector is dead
 * with no signal. These cases pin the three facts the guard and the whole
 * `mockReset()` convention rest on, so that breakage is loud.
 *
 * See [internal]rules/tests.md § The `*Once` queue outlives the test that queued it.
 */
import { describe, it, expect, vi } from 'vitest';

describe('*Once leak guard — self-test', () => {
  const instrumentedCount = () =>
    (globalThis as Record<string, unknown>).__onceQueueLeakGuardInstrumented as number | undefined;

  it('is armed', () => {
    // undefined means the guard block never ran at all.
    expect(typeof instrumentedCount()).toBe('number');
  });

  it('instruments mocks produced by BOTH vi.fn and vi.spyOn', () => {
    // Counting attachments, not just checking a flag: a flag set at module level
    // would stay true if instrument() were dropped from either factory, leaving
    // the guard inert and this file green -- the same assert-something-adjacent
    // defect this PR exists to document.
    const before = instrumentedCount() ?? 0;
    vi.fn();
    expect(instrumentedCount()).toBe(before + 1);

    const obj = { method: () => undefined };
    vi.spyOn(obj, 'method');
    expect(instrumentedCount()).toBe(before + 2);
  });

  it('every *Once variant still dispatches through mockImplementationOnce', () => {
    // The guard patches exactly one property. This is the premise that makes
    // that sufficient, and it is a @vitest/spy implementation detail, so it is
    // exactly the thing a version bump can silently take away.
    for (const queue of [
      (m: ReturnType<typeof vi.fn>) => m.mockResolvedValueOnce('x'),
      (m: ReturnType<typeof vi.fn>) => m.mockRejectedValueOnce(new Error('x')),
      (m: ReturnType<typeof vi.fn>) => m.mockReturnValueOnce('x'),
      (m: ReturnType<typeof vi.fn>) => m.mockThrowOnce(new Error('x')),
    ]) {
      const mock = vi.fn();
      const spy = vi.fn(mock.mockImplementationOnce.bind(mock));
      mock.mockImplementationOnce = spy as unknown as typeof mock.mockImplementationOnce;
      queue(mock);
      expect(spy).toHaveBeenCalledTimes(1);
    }
    // Swallow the rejection queued above so it cannot surface as an unhandled one.
    expect.assertions(4);
  });

  it('mockClear leaves the *Once queue intact and mockReset drains it', async () => {
    // The defect itself. If vitest ever makes mockClear drain, the rule and
    // every mockReset() added for it become unnecessary — and this goes red
    // rather than the convention quietly rotting.
    const cleared = vi.fn().mockReturnValue('default');
    cleared.mockReturnValueOnce('queued');
    cleared.mockClear();
    expect(cleared()).toBe('queued');

    const reset = vi.fn();
    reset.mockReturnValueOnce('queued');
    reset.mockReset();
    reset.mockReturnValue('default');
    expect(reset()).toBe('default');
  });

  it('mockReset restores an implementation given to vi.fn(impl) but not one attached after', () => {
    // Why the fix is always "mockReset() PLUS an explicit default" for one shape
    // and "mockReset()" alone for the other.
    const withImpl = vi.fn(() => 'from vi.fn(impl)');
    withImpl.mockReset();
    expect(withImpl()).toBe('from vi.fn(impl)');

    const withAttached = vi.fn().mockReturnValue('attached later');
    withAttached.mockReset();
    expect(withAttached()).toBeUndefined();
  });
});
