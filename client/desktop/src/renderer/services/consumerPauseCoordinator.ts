/** A reason a remote consumer may be paused. Each maps to a scope below. */
export type PauseReason = 'visibility' | 'ignis' | 'manual';

/**
 * Reason scope:
 *  - 'both'  → pause server forwarding (egress) AND local decode.
 *  - 'local' → pause local decode only (decoder relief; SFU keeps forwarding).
 *
 * 'manual' is an explicit external pause via voiceService.pauseConsumer/resumeConsumer
 * (today's sole caller: PiP ownership-transfer in pipSignalingProxy).
 */
const REASON_SCOPE: Record<PauseReason, 'both' | 'local'> = {
  visibility: 'both',
  manual: 'both',
  ignis: 'local',
};

/** Side effects the coordinator drives. Injected so the coordinator stays pure/testable. */
export interface PauseEffects {
  pauseLocalDecode(consumerId: string): void;
  resumeLocalDecode(consumerId: string): void;
  pauseServerForwarding(consumerId: string): void;
  resumeServerForwarding(consumerId: string): void;
}

interface AppliedState {
  local: boolean;
  server: boolean;
}

/**
 * Single owner of per-consumer pause state across all pause reasons. De-conflates the
 * mediasoup `consumer.paused` boolean (which previously encoded two independent intents)
 * into an egress decision and a local-decode decision derived from a reason set. See
 * [internal]specs/2026-06-15-1541-visibility-pause-coordination-design.md.
 */
export class ConsumerPauseCoordinator {
  private readonly reasons = new Map<string, Set<PauseReason>>();
  private readonly applied = new Map<string, AppliedState>();

  constructor(private readonly effects: PauseEffects) {}

  addReason(consumerId: string, reason: PauseReason): void {
    const set = this.reasons.get(consumerId) ?? new Set<PauseReason>();
    set.add(reason);
    this.reasons.set(consumerId, set);
    this.reconcile(consumerId);
  }

  removeReason(consumerId: string, reason: PauseReason): void {
    const set = this.reasons.get(consumerId);
    if (!set?.has(reason)) return;
    set.delete(reason);
    if (set.size === 0) this.reasons.delete(consumerId);
    this.reconcile(consumerId);
  }

  /** Drop all reasons for a torn-down consumer. Emits NO resume effects (the consumer is gone). */
  clearConsumer(consumerId: string): void {
    this.reasons.delete(consumerId);
    this.applied.delete(consumerId);
  }

  /** Drop all state (channel leave / emergency cleanup). */
  reset(): void {
    this.reasons.clear();
    this.applied.clear();
  }

  hasReason(consumerId: string, reason: PauseReason): boolean {
    return this.reasons.get(consumerId)?.has(reason) ?? false;
  }

  consumersWithReason(reason: PauseReason): string[] {
    const out: string[] = [];
    for (const [id, set] of this.reasons) {
      if (set.has(reason)) out.push(id);
    }
    return out;
  }

  private reconcile(consumerId: string): void {
    const set = this.reasons.get(consumerId);
    const desiredLocal = set ? set.size > 0 : false;
    const desiredServer = set ? [...set].some((r) => REASON_SCOPE[r] === 'both') : false;

    const prev = this.applied.get(consumerId) ?? { local: false, server: false };

    // Server forwarding: on resume, emitting resume-consumer makes mediasoup request a
    // key frame, so re-show is not a long keyframe wait.
    if (desiredServer && !prev.server) {
      this.effects.pauseServerForwarding(consumerId);
    } else if (!desiredServer && prev.server) {
      this.effects.resumeServerForwarding(consumerId);
    }

    if (desiredLocal && !prev.local) {
      this.effects.pauseLocalDecode(consumerId);
    } else if (!desiredLocal && prev.local) {
      this.effects.resumeLocalDecode(consumerId);
    }

    if (desiredLocal || desiredServer) {
      this.applied.set(consumerId, { local: desiredLocal, server: desiredServer });
    } else {
      this.applied.delete(consumerId);
    }
  }
}
