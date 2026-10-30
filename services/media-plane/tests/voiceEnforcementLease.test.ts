import { describe, expect, it, vi } from 'vitest';
import './mocks/logger.js';
import { logger } from '../src/lib/logger.js';
import {
  FailureStateLogger,
  VoiceEnforcementExpiryFence,
  VoiceEnforcementHealth,
  VoiceEnforcementLease,
} from '../src/lib/voiceEnforcementLease.js';

describe('VoiceEnforcementLease', () => {
  it('starts expired, renews only explicitly, and expires on monotonic time', () => {
    let now = 0;
    const lease = new VoiceEnforcementLease(() => now, 30_000);
    expect(lease.valid()).toBe(false);
    lease.renew();
    expect(lease.valid()).toBe(true);
    now = 30_001;
    expect(lease.valid()).toBe(false);
  });

  it('expires immediately and keeps media fenced until teardown finishes before a later grant', async () => {
    const lease = new VoiceEnforcementLease(() => 0, 30_000);
    const media = { forwarding: true, connected: true };
    let finishTeardown!: () => void;
    const terminalization = new Promise<void>((resolve) => {
      finishTeardown = resolve;
    });
    let teardowns = 0;
    const fence = new VoiceEnforcementExpiryFence(lease, async () => {
      teardowns++;
      await terminalization;
      media.forwarding = false;
      media.connected = false;
    });
    lease.renew();
    expect(lease.valid()).toBe(true);

    lease.expire();
    expect(lease.valid()).toBe(false);
    const watchdog = fence.enforce();
    const signedGrantTeardown = fence.enforce();
    expect(watchdog).toBe(signedGrantTeardown);
    expect(teardowns).toBe(1);
    expect(lease.valid()).toBe(false);
    expect(media).toEqual({ forwarding: true, connected: true });

    finishTeardown();
    await signedGrantTeardown;
    expect(media).toEqual({ forwarding: false, connected: false });
    expect(lease.valid()).toBe(false);

    // The signed-health caller may grant authority only after awaiting the same
    // terminalization fence; closing media never itself grants a new lease.
    lease.renew();
    expect(lease.valid()).toBe(true);
    await fence.enforce();
    expect(teardowns).toBe(1);
  });
});


describe('failure state logging', () => {
  it('logs a fixed message once per failing state and re-arms after success', () => {
    vi.mocked(logger.error).mockClear();
    const state = new FailureStateLogger('fixed message');
    state.failed();
    state.failed();
    state.failed();
    expect(logger.error).toHaveBeenCalledTimes(1);
    expect(logger.error).toHaveBeenCalledWith('fixed message');
    state.succeeded();
    state.failed();
    expect(logger.error).toHaveBeenCalledTimes(2);
  });

  it('logs recovery failure once per state change and stays fail-closed', async () => {
    vi.mocked(logger.error).mockClear();
    let fail = true;
    const health = new VoiceEnforcementHealth({
      lease: new VoiceEnforcementLease(() => 0), nodeBootId: 'boot',
      localReady: () => true, transportReady: () => true,
      poll: () => Promise.reject(new Error('poll down')),
      recover: async () => { if (fail) throw new Error('secret-identifier-123'); },
      onTransport: () => undefined,
    });
    await health.poll();
    await vi.waitFor(() => expect(logger.error).toHaveBeenCalledTimes(1));
    await health.poll();
    await health.poll();
    await new Promise((resolve) => setImmediate(resolve));
    expect(logger.error).toHaveBeenCalledTimes(1);
    expect(JSON.stringify(vi.mocked(logger.error).mock.calls)).not.toContain('secret-identifier');
    expect(health.status().voiceEnforcementAdmissionGranted).toBe(false);
    fail = false;
    await health.poll();
    await new Promise((resolve) => setImmediate(resolve));
    fail = true;
    await health.poll();
    await vi.waitFor(() => expect(logger.error).toHaveBeenCalledTimes(2));
  });
});
