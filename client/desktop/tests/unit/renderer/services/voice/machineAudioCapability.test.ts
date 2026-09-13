import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

const SUBSCRIBER = '../../../../../src/renderer/services/voice/machineAudioCapability';
const STORE = '../../../../../src/renderer/stores/voice/voiceStore';

type CapabilityCb = (data: { perProcessAudio: boolean }) => void;

/** A contract-28 shell: `audiocap.onCapability` present. Returns a push emitter. */
function installShell28() {
  let cb: CapabilityCb | null = null;
  // Clears `cb`, so a push AFTER teardown reaches nothing. A bare `vi.fn()` left the
  // subscription live and made the teardown case assert only that the bridge's unsubscribe
  // was CALLED -- the handshake, not the behaviour (#3198 Phase-8 review).
  const off = vi.fn(() => {
    cb = null;
  });
  (globalThis as { electron?: unknown }).electron = {
    audiocap: {
      getPortMessageTag: () => 'tag',
      onCapability: (fn: CapabilityCb) => {
        cb = fn;
        return off;
      },
    },
  };
  return { push: (data: unknown) => cb?.(data as { perProcessAudio: boolean }), off };
}

/**
 * Load the subscriber AND the store through the SAME fresh module registry.
 *
 * The subscriber's effect is at module scope, so every case needs `vi.resetModules()` to
 * re-evaluate it. That reset also re-evaluates the store the subscriber imports — so a
 * top-level `import { useVoiceStore }` in this file reads a DIFFERENT store instance than
 * the one the subscriber writes to. Reading the stale one makes every `toBeNull()` case
 * here pass against a subscriber that coerces everything, which is the vacuity mode
 * `[internal]rules/tests.md` §57 exists to catch. Resolve both from the same registry.
 */
async function loadSubscriber() {
  const subscriber = await import(SUBSCRIBER);
  const { useVoiceStore } = await import(STORE);
  return { ...subscriber, useVoiceStore };
}

beforeEach(() => {
  vi.resetModules();
});

afterEach(() => {
  // NOT `delete`: `tests/setup.ts` defines `window.electron` with `writable: true` and no
  // `configurable`, so deleting it throws `TypeError` under ESM strict mode. Assigning
  // `undefined` is equivalent for everything under test — the subscriber's guard reaches
  // it through `?.`, which treats absent and undefined identically.
  (globalThis as { electron?: unknown }).electron = undefined;
});

describe('machine audio-capability subscription (#3198)', () => {
  it('writes a pushed true into the store', async () => {
    const shell = installShell28();
    const { useVoiceStore } = await loadSubscriber();

    shell.push({ perProcessAudio: true });

    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);
  });

  it('writes a pushed false into the store', async () => {
    const shell = installShell28();
    const { useVoiceStore } = await loadSubscriber();

    shell.push({ perProcessAudio: false });

    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(false);
  });

  // CAPABILITY, NOT DEMAND (#2967). A shell below contract 28 has no `onCapability`.
  // Nothing subscribes, the store keeps `null`, and the ladder stays on the pre-addon
  // rungs — video-only, never a system mix (C9). This must not throw.
  it('subscribes to nothing on a shell below contract 28', async () => {
    (globalThis as { electron?: unknown }).electron = {
      audiocap: { getPortMessageTag: () => 'tag' },
    };

    await expect(import(SUBSCRIBER)).resolves.toBeDefined();

    const { useVoiceStore } = await import(STORE);
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBeNull();
  });

  it('subscribes to nothing when there is no electron bridge at all (dev/web)', async () => {
    (globalThis as { electron?: unknown }).electron = undefined;

    await expect(import(SUBSCRIBER)).resolves.toBeDefined();

    const { useVoiceStore } = await import(STORE);
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBeNull();
  });

  // Main narrows this at `isAudiocapHello`, so a non-boolean arriving here means main's
  // own guard failed. Coercing it would hide that; the store keeps its last honest value.
  it.each([
    ['a truthy 1', { perProcessAudio: 1 }],
    ['a string', { perProcessAudio: 'true' }],
    ['a missing field', {}],
    ['null', null],
    ['undefined', undefined],
  ])('ignores %s rather than coercing it', async (_label, payload) => {
    const shell = installShell28();
    const { useVoiceStore } = await loadSubscriber();

    // ESTABLISH LIVE AND HONEST FIRST. Asserting `toBeNull()` after the bad push tested the
    // store's INITIAL value, which two independent branches produce: the subscriber
    // correctly ignoring the payload, and the subscriber never having subscribed at all
    // (#3198 Phase-8 review). Seeding a real `true` pins liveness, non-coercion and
    // retention in one case -- and would catch a coercing implementation, which the
    // `toBeNull()` form could not.
    shell.push({ perProcessAudio: true });
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);

    shell.push(payload);

    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);
  });

  // THE HMR REGISTRATION. `apiClient.ts:188-194` wires its unsubscribe into
  // `import.meta.hot.dispose()` so a Vite hot reload does not accumulate a second
  // `ipcRenderer.on('audiocap:capability', …)` on top of the first; this module cited that
  // precedent while copying only half of it (#3198 Phase-8 review). Parameterised so the
  // registration is testable - `import.meta.hot` is undefined under Vitest, so an inlined
  // `if` would leave the true branch permanently uncovered.
  it('registers an HMR dispose that unsubscribes, and no-ops without a hot handle', async () => {
    const shell = installShell28();
    const { registerHmrDispose, useVoiceStore } = await loadSubscriber();

    shell.push({ perProcessAudio: true });
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);

    let disposeCb: (() => void) | null = null;
    registerHmrDispose({
      dispose: (cb: () => void) => {
        disposeCb = cb;
      },
    });
    expect(disposeCb).toBeTypeOf('function');

    // The CONSUMER, not the handshake: firing dispose must stop the store changing.
    disposeCb!();
    shell.push({ perProcessAudio: false });
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);

    // Production passes `import.meta.hot`, which is undefined outside dev. Must not throw.
    expect(() => registerHmrDispose(undefined)).not.toThrow();
  });

  // The teardown export's only caller. Without it the subscription outlives the module
  // that owns it, and an exported function nothing calls is the "shipped with no caller"
  // defect #3197's review forced closed one layer down.
  it('stops writing to the store after the test-only teardown', async () => {
    const shell = installShell28();
    const { stopMachineAudioCapabilitySubscription, useVoiceStore } = await loadSubscriber();

    shell.push({ perProcessAudio: true });
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);

    stopMachineAudioCapabilitySubscription();

    expect(shell.off).toHaveBeenCalledTimes(1);

    // THE CONSUMER, NOT THE HANDSHAKE (`tests.md` § "Test the consumer"). That `off` was
    // called is the handshake; that the store STOPS CHANGING is the behaviour this case is
    // named for, and nothing asserted it. A no-op unsubscribe passed.
    shell.push({ perProcessAudio: false });
    expect(useVoiceStore.getState().machineScreenAudioCapable).toBe(true);

    // Idempotent: a second call must not re-invoke the bridge's unsubscribe.
    stopMachineAudioCapabilitySubscription();
    expect(shell.off).toHaveBeenCalledTimes(1);
  });
});
