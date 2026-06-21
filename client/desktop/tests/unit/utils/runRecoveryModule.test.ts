import { vi, describe, it, expect, beforeEach } from 'vitest';

// triggerChunkSelfHeal is the self-heal entry point the guard must invoke on a
// chunk-load failure (the bare import().then() would have lost it — see the
// item-1/item-4 coupling). Mock it so we can assert it fires.
const mockTriggerChunkSelfHeal = vi.fn();
vi.mock('@/renderer/spaSelfHealClient', () => ({
  triggerChunkSelfHeal: (...args: unknown[]) => mockTriggerChunkSelfHeal(...args),
}));

import { runRecoveryModule } from '@/renderer/utils/runRecoveryModule';

beforeEach(() => {
  vi.clearAllMocks();
});

describe('runRecoveryModule', () => {
  it('runs the action with the resolved module and does NOT trigger self-heal on success', async () => {
    const mod = { doThing: vi.fn() };
    const run = vi.fn((m: typeof mod) => m.doThing());

    await runRecoveryModule(() => Promise.resolve(mod), run, 'doThing');

    expect(run).toHaveBeenCalledWith(mod);
    expect(mod.doThing).toHaveBeenCalledOnce();
    expect(mockTriggerChunkSelfHeal).not.toHaveBeenCalled();
  });

  it('swallows a stale-chunk import rejection, skips the action, and triggers self-heal', async () => {
    const run = vi.fn();
    const importer = () =>
      Promise.reject(
        new Error('Failed to fetch dynamically imported module: resetService-abc123.js')
      );

    // Resolving (never rejecting) is the whole point: the call site can float
    // this promise without producing an "Uncaught (in promise)".
    await expect(runRecoveryModule(importer, run, 'gracefulReset')).resolves.toBeUndefined();

    expect(run).not.toHaveBeenCalled();
    expect(mockTriggerChunkSelfHeal).toHaveBeenCalledWith('chunk-import-rejected');
  });
});
