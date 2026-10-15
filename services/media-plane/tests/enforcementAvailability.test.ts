import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  ENFORCEMENT_OUTAGE_GRACE_MS,
  EnforcementAvailability,
} from '../src/lib/enforcementAvailability.js';

function deferred() {
  let resolve!: () => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<void>((success, failure) => {
    resolve = success;
    reject = failure;
  });
  return { promise, resolve, reject };
}

describe('bounded enforcement availability', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('stays unavailable at startup without arming a grace deadline for clients', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);

    expect(availability.isReady()).toBe(false);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(expire).not.toHaveBeenCalled();
    expect(availability.isReady()).toBe(false);

    availability.setAvailable(true);
    expect(availability.isReady()).toBe(true);
    availability.stop();
  });

  it('denies readiness immediately and expires existing clients at exactly 30 seconds', async () => {
    expect(ENFORCEMENT_OUTAGE_GRACE_MS).toBe(30_000);
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);

    availability.setAvailable(false);
    expect(availability.isReady()).toBe(false);
    await vi.advanceTimersByTimeAsync(29_000);
    expect(expire).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1000);
    expect(expire).toHaveBeenCalledTimes(1);
    expect(availability.isReady()).toBe(false);

    await vi.advanceTimersByTimeAsync(60_000);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).toHaveBeenCalledTimes(1);
    availability.stop();
  });

  it('does not extend an outage deadline when more loss notifications arrive', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(29_000);

    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(999);
    expect(expire).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(expire).toHaveBeenCalledTimes(1);
    availability.stop();
  });

  it('cancels teardown when complete availability recovers just before the deadline', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(29_999);

    availability.setAvailable(true);
    expect(availability.isReady()).toBe(true);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(expire).not.toHaveBeenCalled();
    availability.stop();
  });

  it('starts teardown on a recovery signal at the absolute deadline before its timer runs', async () => {
    let now = 0;
    const teardown = deferred();
    const expire = vi.fn(() => teardown.promise);
    const availability = new EnforcementAvailability(expire, () => now);
    availability.setAvailable(true);
    availability.setAvailable(false);

    // Simulate a delayed timer: monotonic time reaches the deadline before
    // the event loop delivers the scheduled timeout callback.
    now = 30_000;
    availability.setAvailable(true);
    expect(availability.isReady()).toBe(false);
    await Promise.resolve();
    expect(expire).toHaveBeenCalledTimes(1);
    const joined = availability.awaitTeardown();
    teardown.resolve();
    await joined;
    expect(availability.isReady()).toBe(true);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).toHaveBeenCalledTimes(1);
    availability.stop();
  });

  it('starts overdue teardown on a readiness check before its timer gets an event-loop turn', async () => {
    let now = 0;
    const teardown = deferred();
    const expire = vi.fn(() => teardown.promise);
    const availability = new EnforcementAvailability(expire, () => now);
    availability.setAvailable(true);
    availability.setAvailable(false);

    now = 30_001;
    expect(availability.isReady()).toBe(false);
    await Promise.resolve();
    expect(expire).toHaveBeenCalledTimes(1);
    availability.setAvailable(true);
    expect(availability.isReady()).toBe(false);
    const joined = availability.awaitTeardown();
    teardown.resolve();
    await joined;
    expect(availability.isReady()).toBe(true);
    availability.stop();
  });

  it('rearms a timer that wakes just before the fractional monotonic deadline', async () => {
    let now = 0;
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire, () => now);
    availability.setAvailable(true);
    availability.setAvailable(false);

    now = 29_999.5;
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).not.toHaveBeenCalled();
    expect(availability.isReady()).toBe(false);
    now = 30_000;
    await vi.advanceTimersByTimeAsync(1);
    expect(expire).toHaveBeenCalledTimes(1);
    availability.stop();
  });

  it('gives a new outage its own full deadline after a successful recovery', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(20_000);
    availability.setAvailable(true);
    await vi.advanceTimersByTimeAsync(20_000);

    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(29_999);
    expect(expire).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(expire).toHaveBeenCalledTimes(1);
    availability.stop();
  });

  it('keeps readiness closed when transport recovers during in-flight teardown', async () => {
    const teardown = deferred();
    const expire = vi.fn(() => teardown.promise);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).toHaveBeenCalledTimes(1);

    availability.setAvailable(true);
    expect(availability.isReady()).toBe(false);
    let settled = false;
    const joined = availability.awaitTeardown().then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);
    expect(expire).toHaveBeenCalledTimes(1);

    teardown.resolve();
    await joined;
    expect(availability.isReady()).toBe(true);
    availability.stop();
  });

  it('does not restore readiness after successful teardown while transport remains unavailable', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    await availability.awaitTeardown();

    expect(expire).toHaveBeenCalledTimes(1);
    expect(availability.isReady()).toBe(false);
    availability.setAvailable(true);
    expect(availability.isReady()).toBe(true);
    availability.stop();
  });

  it('retries failed teardown before allowing recovered transport to become ready', async () => {
    const recoveredTeardown = deferred();
    const expire = vi
      .fn<() => Promise<void>>()
      .mockRejectedValueOnce(new Error('native teardown incomplete'))
      .mockImplementationOnce(() => recoveredTeardown.promise);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).toHaveBeenCalledTimes(1);

    availability.setAvailable(true);
    expect(availability.isReady()).toBe(false);
    const joined = availability.awaitTeardown();
    expect(expire).toHaveBeenCalledTimes(2);
    expect(availability.isReady()).toBe(false);
    recoveredTeardown.resolve();
    await joined;
    expect(availability.isReady()).toBe(true);
    availability.stop();
  });

  it('shares one recovery teardown among concurrent waiters and propagates retry failure', async () => {
    const retry = deferred();
    const expire = vi
      .fn<() => Promise<void>>()
      .mockRejectedValueOnce(new Error('first teardown incomplete'))
      .mockImplementationOnce(() => retry.promise);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);

    const first = availability.awaitTeardown();
    const second = availability.awaitTeardown();
    const outcomes = Promise.allSettled([first, second]);
    expect(expire).toHaveBeenCalledTimes(2);
    retry.reject(new Error('retry teardown incomplete'));
    expect((await outcomes).map((result) => result.status)).toEqual(['rejected', 'rejected']);
    expect(availability.isReady()).toBe(false);
    availability.stop();
  });

  it('retains failed teardown when its rejection reason is undefined', async () => {
    const expire = vi
      .fn<() => Promise<void>>()
      .mockRejectedValueOnce(undefined)
      .mockResolvedValueOnce(undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);

    availability.setAvailable(true);
    expect(availability.isReady()).toBe(false);
    await availability.awaitTeardown();
    expect(expire).toHaveBeenCalledTimes(2);
    expect(availability.isReady()).toBe(true);
    availability.stop();
  });

  it('cancels a pending deadline at stop and ignores late recovery notifications', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(29_999);

    availability.stop();
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(expire).not.toHaveBeenCalled();
    expect(availability.isReady()).toBe(false);
  });

  it('retains in-flight teardown at stop and never restores readiness after it finishes', async () => {
    const teardown = deferred();
    const expire = vi.fn(() => teardown.promise);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).toHaveBeenCalledTimes(1);

    availability.stop();
    availability.setAvailable(true);
    let settled = false;
    const joined = availability.awaitTeardown().then(() => {
      settled = true;
    });
    await Promise.resolve();
    expect(settled).toBe(false);
    expect(availability.isReady()).toBe(false);
    teardown.resolve();
    await joined;
    expect(expire).toHaveBeenCalledTimes(1);
    expect(availability.isReady()).toBe(false);
  });

  it('enforces another bounded outage after deadline teardown and complete recovery', async () => {
    const expire = vi.fn(async () => undefined);
    const availability = new EnforcementAvailability(expire);
    availability.setAvailable(true);
    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    await availability.awaitTeardown();
    availability.setAvailable(true);
    expect(availability.isReady()).toBe(true);

    availability.setAvailable(false);
    await vi.advanceTimersByTimeAsync(30_000);
    expect(expire).toHaveBeenCalledTimes(2);
    expect(availability.isReady()).toBe(false);
    availability.stop();
  });
});
