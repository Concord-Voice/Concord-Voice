// A monotonic, expired-by-default authority lease. NATS connectivity alone is
// not authority: only the signed command handled by the exact target subscriber
// may renew it.
export class VoiceEnforcementLease {
  private renewedAt = Number.NEGATIVE_INFINITY;

  constructor(
    private readonly now: () => number = () => performance.now(),
    private readonly ttlMs = 30_000
  ) {}

  renew(): void {
    this.renewedAt = this.now();
  }
  valid(): boolean {
    return this.now() - this.renewedAt <= this.ttlMs;
  }
}

// Shares terminalization between the watchdog and the signed-health consumer.
// Renewal belongs to the caller: this fence only makes an expired process close
// its local media state once before that caller may renew the lease.
export class VoiceEnforcementExpiryFence {
  private teardown: Promise<void> | undefined;

  constructor(
    private readonly lease: VoiceEnforcementLease,
    private readonly closeExpiredMedia: () => Promise<void>
  ) {}

  enforce(): Promise<void> {
    if (this.lease.valid()) return Promise.resolve();
    if (this.teardown) return this.teardown;

    this.teardown = this.closeExpiredMedia().finally(() => {
      this.teardown = undefined;
    });
    return this.teardown;
  }
}
