/**
 * Worker placement for mediasoup routers (#3149).
 *
 * A leaf module on purpose: it imports nothing, so tests exercise selection
 * without mocking the mediasoup native binding.
 */

/**
 * Thrown when placement is attempted with no workers in the pool.
 *
 * The #2178 config guard fatal-exits at startup unless NUM_WORKERS is in
 * [1, 32], so this is not the primary defense against misconfiguration. It is
 * still genuinely reachable: MediasoupService.close() empties the pool, so a
 * getOrCreateRouter racing shutdown lands here.
 */
export class NoWorkersAvailableError extends Error {
  readonly code = 'no_workers_available' as const;
  constructor() {
    super('Cannot select a mediasoup worker: the worker pool is empty');
    this.name = 'NoWorkersAvailableError';
  }
}

/**
 * Thrown when placement is attempted before the room-consumer-count provider
 * is installed.
 *
 * Fails closed deliberately. Degrading to all-zero load would let the
 * lowest-index tie-break silently pin every room to worker 0 — worse than the
 * round-robin this replaces, and invisible in logs.
 */
export class RoomConsumerCountsUnavailableError extends Error {
  readonly code = 'room_consumer_counts_unavailable' as const;
  constructor() {
    super('Cannot select a mediasoup worker: no room-consumer-count provider installed');
    this.name = 'RoomConsumerCountsUnavailableError';
  }
}

/**
 * Index of the least-loaded worker. Pure: same inputs, same output, no side
 * effect, and neither input array is mutated.
 *
 * Primary key is consumer load. **Ties break on the fewest assigned routers**,
 * and only then on the lowest index.
 *
 * The rooms tie-break is load-bearing, not a refinement (#3157 review). A
 * room's consumer count is 0 from the moment its router is created until a
 * second participant joins AND completes a client-driven `consume` — seconds,
 * not microseconds. Without the tie-break, every room created during that fill
 * window reads `[0, 0, …]`, takes the lowest index, and pins to worker 0 for
 * its lifetime; a meeting-start burst is then strictly WORSE than the
 * round-robin this replaced, whose one virtue was being correct under zero
 * information. Breaking ties on room count recovers exactly that virtue: when
 * the load signal carries no information, placement degenerates to spreading.
 *
 * Either array may be shorter than `workerCount`; a missing entry counts as
 * zero, which is what a worker whose rooms were all removed looks like. The
 * coalesce inside the loop is load-bearing (a short `roomsByIndex` must still
 * break ties); the two on the seeds are symmetry with it — an absent index 0
 * already yields 0 through the comparison, so they cannot change an answer.
 */
export function selectLeastLoadedWorkerIndex(
  workerCount: number,
  loadByIndex: readonly number[],
  roomsByIndex: readonly number[] = []
): number {
  if (workerCount <= 0) {
    throw new NoWorkersAvailableError();
  }

  let bestIndex = 0;
  let bestLoad = loadByIndex[0] ?? 0;
  let bestRooms = roomsByIndex[0] ?? 0;

  // Strict `<` on both keys is what keeps the final tie-break the lowest index.
  for (let i = 1; i < workerCount; i++) {
    const load = loadByIndex[i] ?? 0;
    const rooms = roomsByIndex[i] ?? 0;
    if (load < bestLoad || (load === bestLoad && rooms < bestRooms)) {
      bestIndex = i;
      bestLoad = load;
      bestRooms = rooms;
    }
  }

  return bestIndex;
}
