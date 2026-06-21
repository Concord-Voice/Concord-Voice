export interface ActiveSpeakerDelta {
  /** producer IDs newly forwarded (resume their consumers) */
  added: string[];
  /** producer IDs newly paused (pause their consumers) */
  removed: string[];
}

/**
 * Pure, deterministic N-slot active-speaker set with leave-hysteresis for audio
 * last-N (#1544). No timers, no mediasoup — the clock is injected via `nowMs`,
 * so it is fully unit-testable. `current().size <= capacity` ALWAYS (the egress
 * bound). A speaker is admitted the instant it is loud; it leaves only after
 * `holdMs` of silence (brief dips do not flap), OR immediately when the set is
 * full and a louder newcomer needs its slot (evict longest-silent).
 */
export class ActiveSpeakerSet {
  private readonly lastSeen = new Map<string, number>();

  constructor(
    private readonly capacity: number,
    private readonly holdMs: number
  ) {}

  update(ranked: readonly string[], nowMs: number): ActiveSpeakerDelta {
    const before = new Set(this.lastSeen.keys());
    // 1. Refresh: everyone currently loud is (re)admitted and timestamped.
    for (const id of ranked) this.lastSeen.set(id, nowMs);
    // 2. Hysteresis evict, then 3. hard-cap evict.
    this.evictSilent(new Set(ranked), nowMs);
    this.evictToCapacity();
    // 4. Delta vs. `before`.
    return this.computeDelta(before);
  }

  /** Evict producers that are not currently loud and have been silent > holdMs. */
  private evictSilent(rankedSet: ReadonlySet<string>, nowMs: number): void {
    for (const [id, seen] of this.lastSeen) {
      if (!rankedSet.has(id) && nowMs - seen > this.holdMs) this.lastSeen.delete(id);
    }
  }

  /** Hard cap: evict the longest-silent (oldest lastSeen) until size <= capacity. */
  private evictToCapacity(): void {
    while (this.lastSeen.size > this.capacity) {
      const oldest = this.oldestId();
      if (oldest === undefined) break;
      this.lastSeen.delete(oldest);
    }
  }

  /** The producer with the oldest lastSeen timestamp (longest-silent), or undefined. */
  private oldestId(): string | undefined {
    let oldestId: string | undefined;
    let oldestSeen = Infinity;
    for (const [id, seen] of this.lastSeen) {
      if (seen < oldestSeen) {
        oldestSeen = seen;
        oldestId = id;
      }
    }
    return oldestId;
  }

  /** Membership delta of the current set vs. a prior snapshot. */
  private computeDelta(before: ReadonlySet<string>): ActiveSpeakerDelta {
    const added: string[] = [];
    const removed: string[] = [];
    for (const id of this.lastSeen.keys()) if (!before.has(id)) added.push(id);
    for (const id of before) if (!this.lastSeen.has(id)) removed.push(id);
    return { added, removed };
  }

  current(): ReadonlySet<string> {
    return new Set(this.lastSeen.keys());
  }

  /** Producer-close cleanup. */
  remove(producerId: string): void {
    this.lastSeen.delete(producerId);
  }
}
