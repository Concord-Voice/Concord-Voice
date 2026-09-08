import { describe, it, expect } from 'vitest';
import {
  selectLeastLoadedWorkerIndex,
  NoWorkersAvailableError,
} from '../src/lib/workerSelection.js';

describe('selectLeastLoadedWorkerIndex', () => {
  it('throws a typed error when the worker pool is empty', () => {
    expect(() => selectLeastLoadedWorkerIndex(0, [])).toThrow(NoWorkersAvailableError);
    expect(() => selectLeastLoadedWorkerIndex(0, [])).toThrow(/worker pool is empty/);
  });

  it('selects the only worker when there is one', () => {
    expect(selectLeastLoadedWorkerIndex(1, [17])).toBe(0);
  });

  it('selects the worker with the fewest consumers', () => {
    expect(selectLeastLoadedWorkerIndex(3, [40, 5, 22])).toBe(1);
  });

  it('breaks ties to the lowest index when all loads are equal', () => {
    expect(selectLeastLoadedWorkerIndex(3, [7, 7, 7])).toBe(0);
  });

  it('breaks ties to the lowest index when the minimum is not unique', () => {
    expect(selectLeastLoadedWorkerIndex(4, [9, 2, 2, 9])).toBe(1);
  });

  it('treats a worker with no load entry as idle', () => {
    // A worker whose rooms were all removed since the last selection has no
    // entry in the load array at all.
    expect(selectLeastLoadedWorkerIndex(3, [4, 6])).toBe(2);
  });

  it('treats an entirely empty load array as an all-idle pool', () => {
    // Every worker lost its last room since the previous selection, so there
    // is no entry even at index 0.
    //
    // This does NOT pin the seed's `?? 0`: dropping it is behaviourally
    // equivalent, because `load < undefined` is false for every load, so the
    // function still returns 0. The seed coalesce is symmetry with the loop's,
    // which IS load-bearing and IS pinned below.
    expect(selectLeastLoadedWorkerIndex(3, [])).toBe(0);
  });

  it('breaks a load tie on the worker with the fewest assigned routers', () => {
    // The #3157 fix. Consumer load is 0 across the board during the fill
    // window; without this, every room in a burst takes index 0 and pins there.
    expect(selectLeastLoadedWorkerIndex(3, [0, 0, 0], [2, 1, 3])).toBe(1);
  });

  it('prefers real load over room count — rooms only break ties', () => {
    // Worker 2 holds the most rooms but the fewest consumers. Load wins.
    expect(selectLeastLoadedWorkerIndex(3, [9, 4, 1], [0, 1, 7])).toBe(2);
  });

  it('falls back to the lowest index when load AND room count tie', () => {
    expect(selectLeastLoadedWorkerIndex(3, [5, 5, 5], [2, 2, 2])).toBe(0);
  });

  it('treats a short roomsByIndex as zero rooms for the missing workers', () => {
    // Loop coalesce, load-bearing: without `?? 0` on `rooms`, `undefined < 2`
    // is false and the tie never breaks, leaving everything on worker 0.
    expect(selectLeastLoadedWorkerIndex(3, [], [2])).toBe(1);
  });

  it('omitting roomsByIndex entirely still selects by load', () => {
    // The parameter is optional; existing two-argument callers keep working.
    expect(selectLeastLoadedWorkerIndex(3, [7, 2, 9])).toBe(1);
  });

  it('is pure — repeated calls agree and the input is not mutated', () => {
    const load = [3, 1, 2];
    const first = selectLeastLoadedWorkerIndex(3, load);
    const second = selectLeastLoadedWorkerIndex(3, load);
    expect(second).toBe(first);
    expect(load).toEqual([3, 1, 2]);
  });
});
