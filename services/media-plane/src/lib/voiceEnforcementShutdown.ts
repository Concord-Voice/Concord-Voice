import { releaseVoiceEnforcementSession } from './voiceEnforcementSession.js';
import type { NativeSessionVerifier, VoiceEnforcementSession } from './voiceEnforcementSession.js';
import type { AuthenticatedSocketData } from '../middleware/auth.js';
import { MAX_SERVER_VOICE_PARTICIPANTS } from './roomManager.js';
import type { VoiceAuthConnections } from './voiceAuthConnection.js';

type ShutdownSocket = { data: AuthenticatedSocketData };

const releaseRetryBatchSize = 64;
// Production shares this total with the private connection+room ledger. The
// standalone compatibility queue also refuses before invoking native/release work.
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

  constructor(
    private readonly verifyNative: NativeSessionVerifier,
    private readonly onSaturated?: (capacity: number, overflowedCount: number) => void,
    private readonly owner?: VoiceAuthConnections
  ) {}

  enqueue(session: VoiceEnforcementSession): boolean {
    if (this.family(session)) return true;
    if (this.owner && !this.reserveLegacy(session)) return true;
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
      if (this.family(session)) continue;
      if (this.owner && !this.reserveLegacy(session)) continue;
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
    if (this.owner?.ownsSession(session)) { await this.owner.releaseRoom(session); return; }
    if (session.connectionGeneration) throw new Error('Voice connection owner required');
    if (this.owner) {
      this.enqueue(session);
      if (!this.held.has(session.sessionGeneration)) await this.owner.releaseLegacyRoom(session);
      return;
    }
    this.enqueue(session);
    if (this.held.has(session.sessionGeneration)) return;
    try {
      await releaseVoiceEnforcementSession(session, this.verifyNative);
      this.pending.delete(session.sessionGeneration);
    } catch {
      this.enqueue(session);
    }
  }

  retryPending(): Promise<void> {
    if (this.owner) {
      for (const session of this.pending.values()) this.owner.releaseLegacyRoom(session);
      return this.owner.retryPending();
    }
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
    // Live/provisional family parents and rooms remain liabilities even when
    // no public socket/session snapshot can find them.
    return this.owner?.retainedIdentityCount() ?? (this.pending.size + this.held.size);
  }

  saturated(): boolean {
    return this.outstandingCount() >= VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY;
  }

  private family(session: VoiceEnforcementSession): boolean {
    if (this.owner?.ownsSession(session)) {
      void this.owner.releaseRoom(session);
      return true;
    }
    if (session.connectionGeneration) throw new Error('Voice connection owner required');
    return false;
  }

  private reserveLegacy(session: VoiceEnforcementSession): boolean {
    const owner = this.owner;
    if (!owner) return false;
    // The callback captures the immutable tuple, never caller-mutable data.
    const exact = { ...session };
    return owner.reserveLegacyRoom(session, async () => {
      await releaseVoiceEnforcementSession(exact, this.verifyNative);
      this.pending.delete(exact.sessionGeneration);
      this.held.delete(exact.sessionGeneration);
    });
  }

  private reserveRetrySlot(): boolean {
    if (this.owner) return true;
    if (this.pending.size + this.held.size >= VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY) {
      this.onSaturated?.(VOICE_ENFORCEMENT_RELEASE_QUEUE_CAPACITY, 1);
      throw new Error('Voice identity capacity exhausted');
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
          await releaseVoiceEnforcementSession(session, this.verifyNative);
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
  sockets: Iterable<ShutdownSocket>,
  owner?: VoiceAuthConnections
): VoiceEnforcementSession[] {
  const sessions = new Map<string, VoiceEnforcementSession>();
  for (const socket of sockets) {
    const session = socket.data.voiceEnforcementSession;
    if (!session) continue;
    if (owner?.ownsSession(session)) continue;
    if (session.connectionGeneration) throw new Error('Voice connection owner required');
    sessions.set(session.sessionGeneration, { ...session });
  }
  return Array.from(sessions.values());
}

// Queues only sessions whose RoomManager transport teardown has completed, then
// drains bounded batches while progress is made. The caller retains the target
// through its bounded shutdown deadline.
export async function releaseVoiceEnforcementSessionsAfterClose(
  sockets: Iterable<ShutdownSocket>,
  releaseQueue: VoiceEnforcementSessionReleaseQueue,
  owner?: VoiceAuthConnections
): Promise<number> {
  // Idle and provisional sockets are in the private ledger, even if Socket.IO
  // never admitted them or removed their public socket map entry.
  const legacy = snapshotVoiceEnforcementSessions(sockets, owner);
  if (owner) void owner.terminateAll();
  releaseQueue.enqueueAll(legacy);
  await releaseQueue.retryPending();
  return releaseQueue.outstandingCount();
}
