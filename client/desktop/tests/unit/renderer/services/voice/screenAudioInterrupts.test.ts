/**
 * The replay slot for a live per-process capture's interrupt push (#3394 PR 2 T5a,
 * design section 4.4).
 *
 * Nothing orders a `webContents.send` push against an `invoke` reply, so an interrupt
 * for a child that crashed right after `started` can arrive before the service has
 * claimed its generation. One pending slot is enough because generations are
 * monotonic: a newer claim always supersedes an older pending entry, and a push older
 * than the live claim is stale by construction.
 *
 * `screenAudioInterrupts.ts` does not exist yet. Every test below reaches it through a
 * NON-LITERAL dynamic `import()` so `tsc --noEmit` does not try to resolve the module
 * before it exists (see `[internal]skills/scaffold-component/SKILL.md` conventions and
 * this task's own instructions).
 */
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

interface ScreenAudioInterruptsModule {
  claimInterrupts: (generation: number, handler: (reason: string) => void) => void;
  releaseInterrupts: (generation: number) => void;
  deliverInterrupt: (payload: unknown) => void;
  resetScreenAudioInterruptsForTest: () => void;
}

// Not a literal specifier tsc can resolve -- built from a variable so the not-yet-
// existing module cannot fail a whole-project type-check.
const MOD_PATH = '../../../../../src/renderer/services/voice/screenAudioInterrupts';

async function loadModule(): Promise<ScreenAudioInterruptsModule> {
  return (await import(/* @vite-ignore */ MOD_PATH)) as ScreenAudioInterruptsModule;
}

describe('screenAudioInterrupts', () => {
  let mod: ScreenAudioInterruptsModule;

  beforeEach(async () => {
    mod = await loadModule();
    mod.resetScreenAudioInterruptsForTest();
  });

  // `tests/setup.ts` defines `window.electron` with `writable: true` and no
  // `configurable`, so stubbing it with vi.stubGlobal throws "Cannot redefine property".
  // Assign and restore instead -- the same shape as `machineAudioCapability.test.ts`.
  const originalElectron = (globalThis as { electron?: unknown }).electron;
  const setElectron = (value: unknown): void => {
    (globalThis as { electron?: unknown }).electron = value;
  };

  afterEach(() => {
    setElectron(originalElectron);
    vi.unstubAllGlobals();
  });

  it('replays a push that arrived before the claim', () => {
    mod.deliverInterrupt({ generation: 5, reason: 'child-crash' });
    const handler = vi.fn();

    mod.claimInterrupts(5, handler);

    expect(handler).toHaveBeenCalledExactlyOnceWith('child-crash');
  });

  it('clears an older pending entry on claim', () => {
    mod.deliverInterrupt({ generation: 4, reason: 'child-crash' });
    const handler = vi.fn();

    mod.claimInterrupts(5, handler);

    expect(handler).not.toHaveBeenCalled();
  });

  // Mutation `Me`: delete `if (claim !== null && generation < claim.generation) return;`
  // in `deliverInterrupt`. Without it a STALE push (generation 4, arriving after a
  // NEWER one already claimed the pending slot for generation 6) overwrites that
  // newer pending entry rather than being dropped -- the real harm is losing the
  // reason the NEXT share's claim was waiting to replay. h5 is included only to prove
  // the generation-5 claim this scenario starts from never fires; the mutation this
  // case targets touches the generation-6 path, not h5's.
  //
  // NOTE on the sibling branch in `claimInterrupts`: `if (early.generation < generation)
  // { pending = null; return; }` is an EQUIVALENT mutant if deleted. Generations only
  // increase (a fresh claim's `generation` is always the newest live share), so a
  // pending entry that is OLDER than the generation being claimed can never be the
  // generation a later claim asks for either -- it would just sit there forever,
  // dropped by the `early.generation === generation` check's `else` (implicit no-op)
  // on every future claim just the same. The explicit clear is hygiene (it frees the
  // slot immediately instead of leaving a dead entry until the next claim), not a
  // behavior a test can observe by deleting it. Do not add a case asserting it "does
  // something" -- there is nothing this branch's absence changes.
  it('drops a stale push rather than overwriting a newer pending entry', () => {
    const h5 = vi.fn();
    const h6 = vi.fn();
    mod.claimInterrupts(5, h5);

    mod.deliverInterrupt({ generation: 6, reason: 'child-crash' }); // pending for the next share
    mod.deliverInterrupt({ generation: 4, reason: 'protocol-fault' }); // stale

    mod.claimInterrupts(6, h6);

    expect(h6).toHaveBeenCalledExactlyOnceWith('child-crash');
    expect(h5).not.toHaveBeenCalled();
  });

  it('drops a push older than the claim', () => {
    const handler = vi.fn();
    mod.claimInterrupts(5, handler);

    mod.deliverInterrupt({ generation: 4, reason: 'protocol-fault' });

    expect(handler).not.toHaveBeenCalled();
  });

  it('ignores an unknown reason', () => {
    const handler = vi.fn();
    mod.claimInterrupts(5, handler);

    mod.deliverInterrupt({ generation: 5, reason: 'capture-starved' });

    expect(handler).not.toHaveBeenCalled();
  });

  it('releaseInterrupts stops delivery to the live claim', () => {
    const handler = vi.fn();
    mod.claimInterrupts(5, handler);

    // POSITIVE CONTROL, paired with the negative assertion below (tests.md
    // "Vacuity"): without a release, a push for the live generation IS delivered,
    // so the release case below is not passing because nothing was ever wired up.
    mod.deliverInterrupt({ generation: 5, reason: 'child-crash' });
    expect(handler).toHaveBeenCalledExactlyOnceWith('child-crash');

    mod.releaseInterrupts(5);
    mod.deliverInterrupt({ generation: 5, reason: 'protocol-fault' });

    expect(handler).toHaveBeenCalledTimes(1); // still just the pre-release delivery
  });

  it('still delivers a second push for the same generation after a replay', () => {
    mod.deliverInterrupt({ generation: 5, reason: 'child-crash' });
    const handler = vi.fn();
    mod.claimInterrupts(5, handler); // replays the first push

    expect(handler).toHaveBeenCalledTimes(1); // precondition: the replay landed

    mod.deliverInterrupt({ generation: 5, reason: 'protocol-fault' });

    expect(handler).toHaveBeenCalledTimes(2);
    expect(handler).toHaveBeenLastCalledWith('protocol-fault');
  });

  it('does not throw importing when the shell has no onInterrupted', async () => {
    setElectron({ audiocap: {} });
    vi.resetModules();

    await expect(loadModule()).resolves.toBeDefined();
  });

  describe('onInterrupted subscription', () => {
    it('subscribes exactly once at import when the shell has onInterrupted', async () => {
      const onInterrupted = vi.fn().mockReturnValue(() => {});
      setElectron({ audiocap: { onInterrupted } });
      vi.resetModules();

      await loadModule();

      expect(onInterrupted).toHaveBeenCalledTimes(1);
    });

    it('a payload passed to the registered callback reaches a claimed handler', async () => {
      let registered: ((payload: unknown) => void) | undefined;
      const onInterrupted = vi.fn((cb: (payload: unknown) => void) => {
        registered = cb;
        return () => {};
      });
      setElectron({ audiocap: { onInterrupted } });
      vi.resetModules();

      const freshMod = await loadModule();
      const handler = vi.fn();
      freshMod.claimInterrupts(9, handler);

      // Precondition, not incidental: asserting on a callback the module never
      // registered would pass for the wrong reason (tests.md § Vacuity).
      expect(typeof registered).toBe('function');
      registered?.({ generation: 9, reason: 'capture-interrupted' });

      expect(handler).toHaveBeenCalledExactlyOnceWith('capture-interrupted');
    });
  });
});
