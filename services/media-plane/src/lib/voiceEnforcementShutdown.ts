import { releaseVoiceEnforcementSession } from './voiceEnforcementSession.js';
import type { VoiceEnforcementSession } from './voiceEnforcementSession.js';
import type { AuthenticatedSocketData } from '../middleware/auth.js';
import { MAX_SERVER_VOICE_PARTICIPANTS } from './roomManager.js';

type ShutdownSocket = { data: AuthenticatedSocketData };

const releaseRetryBatchSize = 64;
// A process may serve several rooms, but one full supported room is enough
// retained release work to make a control-plane outage visible.  Overflow is
// fail-closed: its durable control-plane rows are never released here and this
// process stops admitting new voice sessions until operator recovery.
export const VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY = MAX_SERVER_VOICE_PARTICIPANTS;

// The control plane is allowed to be unavailable while local transport teardown
// completes. Retain only immutable, already-terminal exact sessions and retry
// them after the target health round trip proves the control plane is reachable.
// This queue never creates a release from a lease expiry alone.
export class VoiceEnforcementSessionReleaseQueue {
  private readonly pending = new Map<string, VoiceEnforcementSession>();
  private readonly held = new Map<string, VoiceEnforcementSession>();
  private retrying: Promise<void> | undefined;
  private retryRequested = false;
  private queueSaturated = false;
  private overflowedCount = 0;

  constructor(private readonly onSaturated?: (capacity: number, overflowedCount: number) => void) {}

  enqueue(session: VoiceEnforcementSession): boolean {
    if (this.held.has(session.sessionGeneration)) return true;
    if (this.pending.has(session.sessionGeneration)) {
      this.pending.set(session.sessionGeneration, { ...session });
      return true;
    }
    if (!this.reserveRetrySlot()) return false;
    this.pending.set(session.sessionGeneration, { ...session });
    return true;
  }

  enqueueAll(sessions: Iterable<VoiceEnforcementSession>): void {
    for (const session of sessions) this.enqueue(session);
  }

  // Lease expiry proves neither control-plane reachability nor release. Hold
  // only terminal exact identities until the same target subscriber receives a
  // fresh signed health command.
  holdAll(sessions: Iterable<VoiceEnforcementSession>): void {
    for (const session of sessions) {
      if (this.held.has(session.sessionGeneration)) continue;
      if (!this.pending.has(session.sessionGeneration) && !this.reserveRetrySlot()) continue;
      this.held.set(session.sessionGeneration, { ...session });
      this.pending.delete(session.sessionGeneration);
    }
  }

  promoteHeld(): void {
    for (const session of this.held.values()) {
      this.pending.set(session.sessionGeneration, session);
    }
    this.held.clear();
  }

  async releaseAfterTerminalClose(session: VoiceEnforcementSession): Promise<void> {
    if (this.held.has(session.sessionGeneration)) return;
    try {
      await releaseVoiceEnforcementSession(session);
      this.pending.delete(session.sessionGeneration);
    } catch {
      this.enqueue(session);
    }
  }

  retryPending(): Promise<void> {
    this.retryRequested = true;
    if (this.retrying) return this.retrying;
    this.retrying = this.retryRequestedBatches().finally(() => {
      this.retrying = undefined;
    });
    return this.retrying;
  }

  pendingCount(): number {
    return this.pending.size;
  }

  heldCount(): number {
    return this.held.size;
  }

  outstandingCount(): number {
    // Overflowed rows deliberately remain durable control-plane obligations.
    // Keep them in the shutdown failure count even if retained rows later drain.
    return this.pending.size + this.held.size + this.overflowedCount;
  }

  saturated(): boolean {
    return this.queueSaturated;
  }

  private reserveRetrySlot(): boolean {
    if (
      this.queueSaturated ||
      this.pending.size + this.held.size >= VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY
    ) {
      this.overflowedCount += 1;
      if (!this.queueSaturated) {
        this.queueSaturated = true;
        this.onSaturated?.(VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY, this.overflowedCount);
      }
      return false;
    }
    return true;
  }

  private async retryRequestedBatches(): Promise<void> {
    do {
      this.retryRequested = false;
      await this.retryAvailable();
    } while (this.retryRequested && this.pending.size > 0);
  }

  private async retryAvailable(): Promise<void> {
    while (await this.retryBatch()) {
      // Keep draining only while a batch released at least one exact row. A
      // control-plane outage therefore consumes one bounded batch per verified
      // health round trip instead of retry-spinning the same failures.
    }
  }

  private async retryBatch(): Promise<boolean> {
    // Rotating failed rows prevents an unavailable prefix from starving later
    // terminal sessions while retaining every exact identity for a future poll.
    const batch = Array.from(this.pending.values()).slice(0, releaseRetryBatchSize);
    let released = 0;
    await Promise.all(
      batch.map(async (session) => {
        try {
          await releaseVoiceEnforcementSession(session);
          if (this.held.has(session.sessionGeneration)) return;
          this.pending.delete(session.sessionGeneration);
          released += 1;
        } catch {
          if (this.held.has(session.sessionGeneration)) return;
          if (this.pending.delete(session.sessionGeneration))
            this.pending.set(session.sessionGeneration, session);
        }
      })
    );
    return released > 0 && this.pending.size > 0;
  }
}

export function snapshotVoiceEnforcementSessions(
  sockets: Iterable<ShutdownSocket>
): VoiceEnforcementSession[] {
  const sessions = new Map<string, VoiceEnforcementSession>();
  for (const socket of sockets) {
    const session = socket.data.voiceEnforcementSession;
    if (session) sessions.set(session.sessionGeneration, { ...session });
  }
  return Array.from(sessions.values());
}

// Queues only sessions whose RoomManager transport teardown has completed, then
// drains bounded batches while progress is made. The caller retains the target
// through its bounded shutdown deadline.
export async function releaseVoiceEnforcementSessionsAfterClose(
  sockets: Iterable<ShutdownSocket>,
  releaseQueue = new VoiceEnforcementSessionReleaseQueue()
): Promise<number> {
  releaseQueue.enqueueAll(snapshotVoiceEnforcementSessions(sockets));
  await releaseQueue.retryPending();
  return releaseQueue.outstandingCount();
}
