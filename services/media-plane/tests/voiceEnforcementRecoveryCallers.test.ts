import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { createRecoveryCallerFixture } from './helpers/voiceEnforcementRecoveryCallers.js';

async function settle() { for (let i = 0; i < 50; i++) await Promise.resolve(); }

beforeEach(() => { vi.useFakeTimers(); });
afterEach(() => { vi.restoreAllMocks(); vi.clearAllTimers(); vi.useRealTimers(); });

describe('actual index recovery caller composition', () => {
  it.each(['failure', 'superseded'] as const)('keeps availability episode across %s retry and creates a new obligation next outage', async cause => {
    const f = await createRecoveryCallerFixture();
    const invalidate = vi.spyOn(f.fence, 'invalidate');
    f.available(true); f.available(false); f.now(30_000);
    expect(f.availability.isReady()).toBe(false);
    const initial = f.availability.awaitTeardown();
    const rejected = expect(initial).rejects.toThrow();
    expect(invalidate).toHaveBeenCalledTimes(1);
    if (cause === 'failure') f.proofs[0].reject(new Error('native proof failed'));
    else { f.lease.renew(); f.lease.expire(); f.proofs[0].resolve(); }
    await rejected;
    f.available(true);
    const retry = f.availability.awaitTeardown();
    expect(invalidate).toHaveBeenCalledTimes(1);
    expect(f.proofs).toHaveLength(2);
    await f.finish(); await retry;
    expect(f.availability.isReady()).toBe(true);
    expect(f.fence.ready()).toBe(true);
    f.available(false); f.now(60_000);
    expect(f.availability.isReady()).toBe(false);
    const next = f.availability.awaitTeardown();
    expect(invalidate).toHaveBeenCalledTimes(2);
    expect(f.proofs).toHaveLength(3);
    await f.finish(); await next;
    f.availability.stop();
  });

  it('dispatcher continuity uses owner notification without a synthetic quarantine episode', async () => {
    const f = await createRecoveryCallerFixture();
    const invalidate = vi.spyOn(f.fence, 'invalidate');
    f.continuity.mockRejectedValueOnce(new Error('continuity unavailable'));
    const dispatch = f.dispatcher.handleCompatibility({});
    const rejected = expect(dispatch).rejects.toThrow('Voice command authority unavailable');
    await settle();
    expect(f.owner.valid()).toBe(false);
    expect(invalidate).toHaveBeenCalledTimes(1);
    expect(f.proofs).toHaveLength(1);
    await f.finish(); await rejected;
    expect(f.fence.ready()).toBe(true);
    f.availability.stop();
  });

  it('native failures invalidate despite coalesced owner notification without adding pending-task waiters', async () => {
    const f = await createRecoveryCallerFixture();
    f.owner.invalidate(); // schedules exactly one retained proof
    const old = f.fence.enforce();
    const rejected = expect(old).rejects.toThrow();
    const then = vi.spyOn(old, 'then');
    for (let i = 0; i < 50; i++) f.failure();
    expect(then).not.toHaveBeenCalled();
    expect(f.proofs).toHaveLength(1);
    expect(f.fence.pending()).toBe(true);
    await f.finish(); await rejected;
    expect(f.fence.ready()).toBe(false);
    const next = f.fence.enforce(); await f.finish(); await next;
    expect(f.proofs).toHaveLength(2);
    expect(f.fence.ready()).toBe(true);
    f.availability.stop();
  });
});
