import { useVoiceStore } from '../../stores/voice/voiceStore';

/**
 * Subscribe the voice store to main's machine-capability push (#3198, contract 28).
 *
 * MODULE SCOPE with a feature-detect guard, on the `apiClient.ts` precedent: the push can
 * arrive on `did-finish-load`, which is before any React tree mounts, so a component
 * effect would miss it and wait for the next change that may never come.
 *
 * The guard is the whole compatibility story. A shell below contract 28 has no
 * `onCapability`, nothing subscribes, the store keeps `null`, and the ladder stays on the
 * pre-addon rungs — video-only, never a system mix (C9). Capability, not demand (#2967).
 */
let unsubscribe: (() => void) | null = null;

if (typeof globalThis.electron?.audiocap?.onCapability === 'function') {
  unsubscribe = globalThis.electron.audiocap.onCapability((data) => {
    // Pass-through of a boolean main already narrowed at `isAudiocapHello`. A non-boolean
    // arriving here would mean main's own guard failed, and coercing it would hide that.
    if (typeof data?.perProcessAudio !== 'boolean') return;
    useVoiceStore.getState().setMachineScreenAudioCapable(data.perProcessAudio);
  });
}

/** Test-only teardown. Not called in production: the subscription lives as long as the renderer. */
export function stopMachineAudioCapabilitySubscription(): void {
  unsubscribe?.();
  unsubscribe = null;
}

// The other half of the `apiClient.ts` precedent this module cites (`apiClient.ts:188-194`).
// Without it, every Vite hot reload of this module registers a second
// `ipcRenderer.on('audiocap:capability', …)` without removing the first — the preload's
// `ipcRenderer` binding survives renderer-module HMR, so the listeners accumulate. Benign in
// effect today, because the write is idempotent, but the module's own docstring claims
// fidelity to a precedent it was only half-copying (#3198 Phase-8 review). Dev-only: Vite
// strips `import.meta.hot` from the production build.
// Exported and parameterised so the registration is testable. Inlining
// `if (import.meta.hot) { … }` at module scope leaves the true branch permanently
// uncovered — `import.meta.hot` is undefined under Vitest — which is an untested guard, the
// exact thing this PR's review was about. Reaching for a coverage-ignore comment instead
// would dodge a mandatory gate rather than answer it.
export function registerHmrDispose(hot?: { dispose: (cb: () => void) => void }): void {
  hot?.dispose(() => {
    stopMachineAudioCapabilitySubscription();
  });
}

registerHmrDispose(import.meta.hot);
