// The approved outage budget is a ceiling. Existing durable-authority fencing
// may terminalize media earlier; connectivity never replaces its signed proof.
export const ENFORCEMENT_OUTAGE_GRACE_MS = 30_000;

export class EnforcementAvailability {
  private available = false;
  private established = false;
  private outage = false;
  private stopped = false;
  private deadline: number | undefined;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private teardownRequired = false;
  private teardown: Promise<void> | undefined;

  constructor(
    private readonly onExpire: () => Promise<void>,
    private readonly now: () => number = () => performance.now()
  ) {}

  setAvailable(available: boolean): void {
    if (this.stopped) return;
    this.enforceDeadline();
    this.available = available;
    if (available) {
      this.established = true;
      if (!this.teardownRequired) {
        this.clearDeadline();
        this.outage = false;
      }
    } else if (this.established && !this.outage) {
      this.outage = true;
      this.deadline = this.now() + ENFORCEMENT_OUTAGE_GRACE_MS;
      this.armDeadline();
    }
  }

  isReady(): boolean {
    this.enforceDeadline();
    return !this.stopped && this.available && !this.teardownRequired;
  }

  private enforceDeadline(): void {
    if (this.stopped || this.deadline === undefined || this.now() < this.deadline) return;
    this.clearDeadline();
    this.teardownRequired = true;
    // The timer owns no rejection. Signed recovery explicitly awaits/retries
    // incomplete teardown instead of inferring success from elapsed time.
    void this.awaitTeardown().catch(() => undefined);
  }

  private armDeadline(): void {
    if (this.deadline === undefined || this.stopped) return;
    this.timer = setTimeout(
      () => {
        this.timer = undefined;
        this.enforceDeadline();
        // A timer can wake just before a fractional monotonic deadline.
        if (this.deadline !== undefined) this.armDeadline();
      },
      Math.max(0, Math.ceil(this.deadline - this.now()))
    );
  }

  awaitTeardown(): Promise<void> {
    if (this.teardown) return this.teardown;
    if (!this.teardownRequired) return Promise.resolve();

    let complete!: () => void;
    let fail!: (error: unknown) => void;
    const pending = new Promise<void>((resolve, reject) => {
      complete = resolve;
      fail = reject;
    });
    this.teardown = pending;
    const settled = (succeeded: boolean, error?: unknown): void => {
      this.teardown = undefined;
      if (!succeeded) {
        fail(error);
        return;
      }
      this.teardownRequired = false;
      if (this.available) this.outage = false;
      complete();
    };
    void (async () => {
      try {
        await this.onExpire();
        settled(true);
      } catch (error) {
        settled(false, error);
      }
    })();
    return pending;
  }

  stop(): void {
    this.stopped = true;
    this.available = false;
    this.clearDeadline();
  }

  private clearDeadline(): void {
    if (this.timer) clearTimeout(this.timer);
    this.timer = undefined;
    this.deadline = undefined;
  }
}
