import { describe, expect, it } from 'vitest';
import { VoiceEnforcementLease } from '../src/lib/voiceEnforcementLease.js';

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
});
