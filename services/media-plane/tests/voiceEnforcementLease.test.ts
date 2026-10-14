import { describe, expect, it } from 'vitest';
import {
  VoiceEnforcementExpiryFence,
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
