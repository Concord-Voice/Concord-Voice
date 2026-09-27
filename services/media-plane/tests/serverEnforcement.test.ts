import { describe, expect, it, vi } from 'vitest';
import { enqueueServerEnforcement } from '../src/lib/serverEnforcement.js';

function deferred(): { promise: Promise<void>; resolve: () => void } {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

describe('enqueueServerEnforcement', () => {
  it('serializes mute/deafen work for one participant without blocking another', async () => {
    const chains = new Map<string, Promise<void>>();
    const firstGate = deferred();
    const started: string[] = [];
    const finished: string[] = [];

    const first = enqueueServerEnforcement(chains, 'room-1', 'user-1', async () => {
      started.push('mute');
      await firstGate.promise;
      finished.push('mute');
    });
    const second = enqueueServerEnforcement(chains, 'room-1', 'user-1', async () => {
      started.push('deafen');
      finished.push('deafen');
    });
    const independent = enqueueServerEnforcement(chains, 'room-1', 'user-2', async () => {
      started.push('other');
      finished.push('other');
    });

    await vi.waitFor(() => expect(started).toEqual(['mute', 'other']));
    firstGate.resolve();
    await Promise.all([first, second, independent]);
    expect(finished).toEqual(['other', 'mute', 'deafen']);
    expect(chains).toEqual(new Map());
  });

  it('continues after a rejected predecessor', async () => {
    const chains = new Map<string, Promise<void>>();
    const failed = enqueueServerEnforcement(chains, 'room-1', 'user-1', async () => {
      throw new Error('pause failed');
    });
    const successor = vi.fn(async () => undefined);
    const next = enqueueServerEnforcement(chains, 'room-1', 'user-1', successor);

    await expect(failed).rejects.toThrow('pause failed');
    await expect(next).resolves.toBeUndefined();
    expect(successor).toHaveBeenCalledOnce();
  });
});
