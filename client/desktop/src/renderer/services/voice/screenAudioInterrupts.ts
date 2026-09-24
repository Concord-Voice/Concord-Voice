import {
  isAudiocapInterrupted,
  type ScreenAudioInterruptReason,
} from '../../../shared/audiocapProtocol';

type InterruptHandler = (reason: ScreenAudioInterruptReason) => void;

/**
 * One claim and ONE pending slot (#3394 PR 2 §4.4). Nothing orders a
 * `webContents.send` push against an `invoke` reply, so an interrupt for a child
 * that crashed right after `started` can arrive before the service has claimed
 * its generation. Generations are monotonic, so one slot is enough: a newer claim
 * always supersedes an older pending entry, and a push older than the live claim
 * is stale by construction.
 */
let claim: { generation: number; handler: InterruptHandler } | null = null;
let pending: { generation: number; reason: ScreenAudioInterruptReason } | null = null;
let unsubscribe: (() => void) | null = null;

/**
 * Validate and route a push. Deliberately `unknown` -- this is the boundary that
 * narrows whatever main sent, whether it arrives live over `onInterrupted` or is
 * replayed in a test.
 */
export function deliverInterrupt(payload: unknown): void {
  if (!isAudiocapInterrupted(payload)) return;
  const { generation, reason } = payload;
  if (claim !== null && claim.generation === generation) {
    claim.handler(reason);
    return;
  }
  if (claim !== null && generation < claim.generation) return;
  pending = { generation, reason };
}

/**
 * Claim the one live slot for `generation`, replaying an earlier push that is
 * for this generation and dropping one that is now stale.
 */
export function claimInterrupts(generation: number, handler: InterruptHandler): void {
  claim = { generation, handler };
  const early = pending;
  if (early === null) return;
  if (early.generation < generation) {
    pending = null;
    return;
  }
  if (early.generation === generation) {
    pending = null;
    handler(early.reason);
  }
}

/** Release the claim for `generation`. A claim for a different (newer) generation is untouched. */
export function releaseInterrupts(generation: number): void {
  if (claim !== null && claim.generation === generation) claim = null;
}

// MODULE SCOPE with a feature-detect guard, on the `machineAudioCapability.ts` precedent: the
// push can arrive before any bridge exists to claim it, and buffering it in `pending` above is
// the whole reason this module -- not the bridge, not the service -- owns the subscription.
if (typeof globalThis.electron?.audiocap?.onInterrupted === 'function') {
  unsubscribe = globalThis.electron.audiocap.onInterrupted(deliverInterrupt);
}

/** Test-only. */
export function resetScreenAudioInterruptsForTest(): void {
  claim = null;
  pending = null;
}

/** Test-only teardown. Not called in production: the subscription lives as long as the renderer. */
export function stopScreenAudioInterruptSubscription(): void {
  unsubscribe?.();
  unsubscribe = null;
}

// The other half of the `machineAudioCapability.ts` precedent this module cites. Without it,
// every Vite hot reload of this module registers a second `ipcRenderer.on('audiocap:interrupted',
// …)` without removing the first. Dev-only: Vite strips `import.meta.hot` from the production
// build. Exported and parameterised so the registration is testable, rather than an inline
// `if (import.meta.hot) { … }` whose true branch is permanently uncovered under Vitest.
export function registerHmrDispose(hot?: { dispose: (cb: () => void) => void }): void {
  hot?.dispose(() => {
    stopScreenAudioInterruptSubscription();
  });
}

registerHmrDispose(import.meta.hot);
