import type { Command, HealthBootstrapResult } from './voiceAuthProtocol.js';
import { logger } from './logger.js';

/**
 * Logs a fixed message once when an operation starts failing and stays quiet
 * while it keeps failing; `succeeded()` re-arms it. Never takes identifiers or
 * the underlying error, so nothing sensitive can reach the log.
 */
export class FailureStateLogger {
  private failing = false;
  constructor(private readonly message: string) {}
  failed(): void {
    if (this.failing) return;
    this.failing = true;
    logger.error(this.message);
  }
  succeeded(): void { this.failing = false; }
}

// A monotonic, expired-by-default authority lease. NATS connectivity alone is
// not authority: production renews only after the signed protocol4 challenge
// and its matching strictly verified bootstrap response.
export class VoiceEnforcementLease {
  private renewedAt = Number.NEGATIVE_INFINITY;
  private productiveGeneration: object = Object.freeze({});

  constructor(
    private readonly now: () => number = () => performance.now(),
    private readonly ttlMs = 30_000
  ) {}

  renew(): void {
    this.renewAt(this.now());
  }
  renewAt(startedAt: number): void {
    if (!Number.isFinite(startedAt) || startedAt > this.now()) throw new Error('Invalid voice lease origin');
    this.renewedAt = startedAt;
    this.productiveGeneration = Object.freeze({});
  }
  generation(): object { return this.productiveGeneration; }
  expire(): void {
    this.renewedAt = Number.NEGATIVE_INFINITY;
  }
  valid(): boolean {
    return this.now() - this.renewedAt < this.ttlMs;
  }
}

interface ExpiryProofKey {
  lease: object;
  reservation: object | undefined;
  obligation: object;
}

// Shares terminalization between the watchdog and the signed-health consumer.
// Renewal belongs to the caller: this fence only makes an expired process close
// its local media state once before that caller may renew the lease.
export class VoiceEnforcementExpiryFence {
  private teardown: Promise<void> | undefined;
  private teardownRequired = true;
  private obligation: object = Object.freeze({});
  private completed: ExpiryProofKey | undefined;

  constructor(
    private readonly lease: VoiceEnforcementLease,
    private readonly closeExpiredMedia: () => Promise<void>,
    private readonly reservationGeneration: () => object | undefined = () => undefined
  ) {}

  pending(): boolean { return this.teardown !== undefined; }
  ready(): boolean {
    return !this.teardownRequired && !this.teardown &&
      (this.lease.valid() || this.matches(this.completed));
  }

  invalidate(): void {
    this.obligation = Object.freeze({});
    this.teardownRequired = true;
  }

  private matches(key: ExpiryProofKey | undefined): boolean {
    return key?.lease === this.lease.generation() &&
      key.reservation === this.reservationGeneration() && key.obligation === this.obligation;
  }

  enforce(): Promise<void> {
    if (this.teardown) return this.teardown;
    if (!this.lease.valid() && !this.matches(this.completed)) this.teardownRequired = true;
    if (!this.teardownRequired) return Promise.resolve();

    const key: ExpiryProofKey = {
      lease: this.lease.generation(), reservation: this.reservationGeneration(), obligation: this.obligation,
    };

    let done: () => void = () => undefined;
    let failed: (error: unknown) => void = () => undefined;
    const task = new Promise<void>((resolve, reject) => { done = resolve; failed = reject; });
    // Install the barrier BEFORE callbacks can disconnect and reenter a gate.
    this.teardown = task;
    this.teardownRequired = true;
    const settled = (success: boolean, error?: unknown) => {
      if (this.teardown !== task) { failed(new Error('Voice enforcement teardown superseded')); return; }
      this.teardown = undefined;
      if (!success) { failed(error); return; }
      // The callback only proves the horizon captured before synchronous teardown.
      if (!this.matches(key)) { failed(new Error('Voice enforcement teardown superseded')); return; }
      this.completed = key;
      this.teardownRequired = false;
      done();
    };
    // The async wrapper runs teardown synchronously and turns a throw into a rejection.
    void (async () => this.closeExpiredMedia())()
      .then(() => settled(true), (error: unknown) => settled(false, error));
    return task;
  }
}

export interface VoiceEnforcementHealthOptions {
  lease: VoiceEnforcementLease;
  nodeBootId: string;
  localReady: () => boolean;
  transportReady: () => boolean;
  poll: () => Promise<HealthBootstrapResult>;
  recover: () => Promise<void>;
  onTransport: () => void;
  now?: () => number;
}

/** Recovery never occupies the synchronous challenge lane or the bootstrap poll. */
export class VoiceEnforcementHealth {
  private polling: Promise<void> | undefined;
  private recovering: Promise<void> | undefined;
  private transportOK = false;
  private recoveryStopped = false;
  private pollingStopped = false;
  private readonly now: () => number;
  private readonly recoveryFailures = new FailureStateLogger('Voice enforcement recovery failed; retrying');

  constructor(private readonly options: VoiceEnforcementHealthOptions) {
    this.now = options.now ?? (() => performance.now());
  }

  healthSnapshot(command: Command): boolean {
    const health = command.health();
    return !this.recoveryStopped && !this.pollingStopped && health?.nodeBootId === this.options.nodeBootId && health.activation === 'active' &&
      health.admissionAllowed && this.options.transportReady() && this.options.localReady();
  }

  status() {
    const transportOK = this.transportOK && this.options.transportReady();
    return {
      voiceEnforcementProtocol: 4 as const,
      nodeBootId: this.options.nodeBootId,
      voiceEnforcementTransportOK: transportOK,
      voiceEnforcementAdmissionGranted: !this.recoveryStopped && !this.pollingStopped &&
        transportOK && this.options.lease.valid() && this.options.localReady(),
    };
  }

  // Fence synchronously before joining: a pre-signal recovery may be suspended
  // inside NATS and must finish before the caller finally stops that consumer.
  fenceRecoveryAndJoin(): Promise<void> {
    this.recoveryStopped = true;
    this.options.lease.expire();
    return this.recovering ?? Promise.resolve();
  }

  stopPollingAndJoin(): Promise<void> {
    this.pollingStopped = true;
    this.options.lease.expire();
    return this.polling ?? Promise.resolve();
  }

  poll(): Promise<void> {
    if (this.pollingStopped) return this.polling ?? Promise.resolve();
    if (!this.recoveryStopped && !this.recovering) {
      this.recovering = Promise.resolve().then(this.options.recover)
        .then(() => this.recoveryFailures.succeeded(), () => this.recoveryFailures.failed())
        .finally(() => { this.recovering = undefined; });
    }
    if (this.polling) return this.polling;
    const startedAt = this.now();
    this.polling = this.pollOnce(startedAt).finally(() => { this.polling = undefined; });
    return this.polling;
  }

  private async pollOnce(startedAt: number): Promise<void> {
    try {
      const result = await this.options.poll();
      if (result.nodeBootId !== this.options.nodeBootId || result.voiceEnforcementProtocol !== 4 ||
          result.transportOK !== true) throw new Error('Invalid voice health result');
      this.transportOK = true;
      // A verified false grant still drives release, including during hung recovery.
      this.options.onTransport();
      if (!this.recoveryStopped && !this.pollingStopped && result.activation === 'active' && result.admissionGranted &&
          this.options.transportReady() && this.options.localReady()) {
        this.options.lease.renewAt(startedAt);
      } else {
        this.options.lease.expire();
      }
    } catch {
      this.transportOK = false;
      this.options.lease.expire();
    }
  }
}
