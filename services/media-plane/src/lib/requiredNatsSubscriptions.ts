import type { Subscription } from 'nats';

const unavailable = () => new Error('Required NATS subscriptions unavailable');
const confirmationDeadline = 2000;
const recoveryDelay = 2000;

interface RequiredSubscription {
  install: () => Subscription;
  handle?: Subscription;
}

/** Required core subscribers share one epoch-qualified broker confirmation. */
export class RequiredNatsSubscriptions {
  private readonly registrations = new Map<string, RequiredSubscription>();
  private readonly observers = new Set<(ready: boolean) => void>();
  private epoch = 0;
  private confirmed = false;
  private notified = false;
  private armed = false;
  private stopped = false;
  private attempt?: Promise<void>;
  private flushPending?: Promise<void>;
  private cancelConfirmation?: () => void;
  private retryTimer?: ReturnType<typeof setTimeout>;

  constructor(
    private readonly connected: () => boolean,
    private readonly flush: () => Promise<void>
  ) {}

  register(subject: string, install: () => Subscription): void {
    if (this.stopped || !this.connected() || this.registrations.has(subject)) throw unavailable();
    const registration = { install };
    this.registrations.set(subject, registration);
    this.invalidate();
    this.install(subject, registration);
  }

  private install(subject: string, registration: RequiredSubscription): void {
    if (this.stopped || !this.connected()) throw unavailable();
    const handle = registration.install();
    registration.handle = handle;
    void handle.closed.then(
      () => this.ended(subject, handle),
      () => this.ended(subject, handle)
    );
  }

  ended(subject: string, handle: Subscription): boolean {
    const registration = this.registrations.get(subject);
    if (registration?.handle !== handle) return false;
    registration.handle = undefined;
    try {
      handle.unsubscribe();
    } catch {
      // A terminated handle must not remain an active duplicate on recovery.
    }
    this.invalidate();
    this.scheduleRecovery(recoveryDelay);
    return true;
  }

  private live(): boolean {
    return (
      this.registrations.size > 0 &&
      [...this.registrations.values()].every(({ handle }) => handle && !handle.isClosed())
    );
  }

  ready(): boolean {
    return this.confirmed && this.connected() && this.live();
  }

  observe(observer: (ready: boolean) => void): () => void {
    this.observers.add(observer);
    this.deliver(observer, this.ready());
    return () => this.observers.delete(observer);
  }

  private deliver(observer: (ready: boolean) => void, ready: boolean): void {
    try {
      observer(ready);
    } catch {
      // Observer failures never affect subscription processing or recovery.
    }
  }

  private notify(): void {
    const ready = this.ready();
    if (ready === this.notified) return;
    this.notified = ready;
    for (const observer of this.observers) this.deliver(observer, ready);
  }

  private invalidate(): void {
    this.epoch++;
    this.confirmed = false;
    this.cancelConfirmation?.();
    this.notify();
  }

  connectionChanged(): void {
    this.invalidate();
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
    this.scheduleRecovery(0);
  }

  confirm(): Promise<void> {
    if (this.stopped) return this.ready() ? Promise.resolve() : Promise.reject(unavailable());
    this.armed = true;
    if (this.attempt) return this.attempt;
    if (!this.connected() || !this.live() || this.flushPending) {
      this.scheduleRecovery(recoveryDelay);
      return Promise.reject(unavailable());
    }
    const epoch = this.epoch;
    this.attempt = this.confirmAtEpoch(epoch).finally(() => {
      this.attempt = undefined;
      if (!this.ready()) this.scheduleRecovery(recoveryDelay);
    });
    return this.attempt;
  }

  private async confirmAtEpoch(epoch: number): Promise<void> {
    let deadline: ReturnType<typeof setTimeout> | undefined;
    try {
      const pending = this.flush();
      this.flushPending = pending;
      const settle = () => {
        if (this.flushPending === pending) this.flushPending = undefined;
      };
      void pending.then(settle, settle);
      await new Promise<void>((resolve, reject) => {
        this.cancelConfirmation = () => reject(unavailable());
        deadline = setTimeout(() => reject(unavailable()), confirmationDeadline);
        pending.then(resolve, reject);
      });
      if (this.stopped || epoch !== this.epoch || !this.connected() || !this.live())
        throw unavailable();
      this.confirmed = true;
      this.notify();
    } catch (error) {
      if (!this.stopped && epoch === this.epoch) this.invalidate();
      throw error;
    } finally {
      if (deadline) clearTimeout(deadline);
      this.cancelConfirmation = undefined;
    }
  }

  private scheduleRecovery(delay: number): void {
    if (!this.armed || this.stopped || !this.connected() || this.retryTimer || this.ready()) return;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      void this.recover().catch(() => this.scheduleRecovery(recoveryDelay));
    }, delay);
    this.retryTimer.unref();
  }

  private async recover(): Promise<void> {
    if (this.stopped || !this.connected()) return;
    for (const [subject, registration] of this.registrations) {
      if (!registration.handle || registration.handle.isClosed())
        this.install(subject, registration);
    }
    await this.confirm();
  }

  stop(): void {
    this.stopped = true;
    if (this.retryTimer) clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
    this.cancelConfirmation?.();
  }

  close(): void {
    this.stop();
    this.invalidate();
    this.registrations.clear();
  }
}
