/**
 * The `audiocap:start` invoke — the ONE handler the #3198 sender-frame criterion
 * binds (A4, R5). PR 1's `audiocap:capability` is a `webContents.send` PUSH and
 * has no sender to validate; reading the absence of a check there as an oversight
 * is the mistake this module's existence is meant to prevent.
 *
 * FOUR FENCES, in this order, and the order is the design:
 *   1. sender frame   -- read on the FIRST statement (A4)
 *   2. payload shape  -- one known field; extras are dropped, never honoured
 *   3. prefix + shape -- re-parsed HERE, never trusted from the renderer (D4a)
 *   4. liveness       -- the id must still be in desktopCapturer's enumeration
 *
 * NO PID APPEARS IN THIS FILE (I-PID, A7). Main resolves a HANDLE and sends that;
 * the capture child is the only process that ever learns a PID, and it never
 * sends one back.
 */
import { ipcMain, desktopCapturer, type IpcMainInvokeEvent } from 'electron';

import { startAudiocapHost, stopAudiocapHost, type AudiocapStartResult } from '../audiocapHost';
import { parseWindowSourceId } from '../../shared/parseWindowSourceId';

import { requireTrustedSender } from './frameValidation';

/** Matches every sibling registrar (`attestation.ts`, `openExternal.ts`, `saveImage.ts`). */
type RemoteSpaOriginProvider = () => string | null;

/**
 * The share path's OWN generation mint, monotonic and module-local.
 *
 * NOT `currentAudiocapGeneration()`, which returns `session?.generation ?? 0` — the
 * LIVE child's number, or 0 when nothing is live. A share starts when nothing is
 * live, so that helper would hand `0` to every share forever and the generation the
 * result carries back would carry no information at all. `audiocapHost.ts` says it
 * outright: "#3198's share path mints its own generations, and a collision with
 * [the probe's] is harmless because I2 fences on session IDENTITY rather than on
 * the number." So this number is not a fence — it is how the renderer tells a
 * result for THIS share from a result for a superseded one.
 */
let shareGeneration = 0;
let shareOperationEpoch = 0;
function nextShareGeneration(): number {
  shareGeneration += 1;
  return shareGeneration;
}

/**
 * Exported for test. `ipcMain.handle` is registered against it below, so the
 * tests exercise the same function production does rather than a copy.
 *
 * `request: unknown` IS THE POINT, and it is a deliberate divergence from the plan's
 * draft, which declared it `AudiocapStartRequest` and then immediately cast back to
 * `unknown` to read the field. The value arrives from the renderer across a trust
 * boundary, so a declared shape is a claim TypeScript will believe and never check —
 * and the cast the draft needed is the tell. Typed `unknown`, fence 2 is the only way
 * to reach the field at all, so it cannot be deleted without the compiler objecting.
 */
export async function handleAudiocapStart(
  event: IpcMainInvokeEvent,
  request: unknown,
  getRemoteSpaOrigin: RemoteSpaOriginProvider
): Promise<AudiocapStartResult> {
  // FENCE 1 -- first statement. A4 is specific about this: a validation that
  // runs after any other work is a validation a future edit can reorder past.
  if (!requireTrustedSender(event, getRemoteSpaOrigin())) {
    return { ok: false, reason: 'protocol-fault' };
  }

  // FENCE 2. Read exactly the one field. An extra `systemAudio: true` is not
  // rejected, because rejecting it would tell a hostile renderer which fields
  // exist; it is simply never read, so it cannot influence anything.
  // `protocol-fault`, NOT `target-unresolved`: a non-record payload or a non-string
  // sourceId is a CONTRACT violation, not a target problem, and protocol-fault is
  // the member the child's own off-protocol messages already use (spec §4.2).
  const sourceId: unknown = (request as { sourceId?: unknown } | null | undefined)?.sourceId;
  if (typeof sourceId !== 'string') {
    return { ok: false, reason: 'protocol-fault' };
  }

  // FENCE 3. `parseWindowSourceId` is "a handle or nothing" and admits ONLY
  // `window:` ids, so a `screen:` id is refused here -- which is also the D6
  // main-side fence: a whole-desktop id can never reach a per-process start.
  //
  // `target-unresolved`, DELIBERATELY DIVERGING from spec §4.2's table, which gives
  // `no-backend` for an id failing the window: regex. The degrade reason's own member
  // comment states the collapse covers "a malformed id, a `screen:` id, a window that
  // closed between pick and start, and an OS call that refused" under A7 /
  // observability principle 7 -- and A7 is a Global Constraint, which outranks that
  // one spec row. Spec §4.3 independently argues `no-backend` would be "a mechanism
  // string naming the wrong mechanism". RECORDED IN THE PR BODY.
  const handle = parseWindowSourceId(sourceId);
  if (handle === null) {
    return { ok: false, reason: 'target-unresolved' };
  }

  // A trusted stop or a newer valid start wins while source enumeration is pending.
  // There is no await between the epoch check below and startAudiocapHost, so a stop
  // cannot land after the check but before the native host is registered.
  shareOperationEpoch += 1;
  const operationEpoch = shareOperationEpoch;

  // FENCE 4. A well-formed id for a window that has since closed is STALE, and
  // "a stale id must never silently widen a capture" (D4a). Thumbnail-free, so
  // this is a cheap enumeration rather than a screenshot of every window.
  const live = await desktopCapturer.getSources({
    types: ['window'],
    thumbnailSize: { width: 0, height: 0 },
    fetchWindowIcons: false,
  });
  if (operationEpoch !== shareOperationEpoch) {
    return { ok: false, reason: 'target-unresolved' };
  }
  if (!live.some((s) => s.id === sourceId)) {
    return { ok: false, reason: 'target-unresolved' };
  }

  return startAudiocapHost(nextShareGeneration(), handle);
}

/**
 * End the live capture (#3198 PR 3).
 *
 * ZERO-ARGUMENT BY CONSTRUCTION, matching `spa:reloadLatest`. The renderer TRIGGERS a
 * stop; it names nothing and chooses nothing. Main resolves which child to reap from its
 * own `session`, so a hostile renderer's only reachable effect is ending a share it can
 * already end by closing its own track.
 *
 * WHY THIS CHANNEL HAS TO EXIST. Every `killAudiocapHost` caller is a quit or crash hook,
 * and `audiocapChild.ts`'s `handleStop` "deliberately does NOT exit" -- so without this, a
 * share the user ENDED left the capture child holding its OS tap until the app quit or the
 * next share superseded it. That is a live tap outliving the share it belonged to, which
 * is the outcome ADR-0043 exists to prevent.
 *
 * `stopAudiocapHost`, never `killAudiocapHost`: the graceful path posts `{kind:'stop'}`
 * first, and the child's `handleStop` is the only caller of the addon's `status()` -- the
 * R9 silence detector, `quiesceProved` and `destroyFailures`. Killing outright destroys
 * the tap just as well and reads none of them.
 *
 * Returns nothing. There is no outcome to report: the stop is unconditional, and a
 * renderer that calls it when nothing is live simply gets a no-op.
 */
export function handleAudiocapStop(
  event: IpcMainInvokeEvent,
  getRemoteSpaOrigin: RemoteSpaOriginProvider
): void {
  // FENCE 1, first statement, as on the start handler. This reaches a process kill, so a
  // frame that is not ours must not be able to end another window's share.
  if (!requireTrustedSender(event, getRemoteSpaOrigin())) return;
  shareOperationEpoch += 1;
  stopAudiocapHost();
}

export function registerAudiocapIpc(getRemoteSpaOrigin: RemoteSpaOriginProvider): void {
  // DI, matching every sibling registrar (registerAttestationIpc, registerOpenExternalHandler,
  // registerSaveImageHandler). A direct module import would make this the one registrar that
  // cannot be tested without stubbing a module.
  ipcMain.handle('audiocap:start', (event, request: unknown) =>
    handleAudiocapStart(event, request, getRemoteSpaOrigin)
  );
  ipcMain.handle('audiocap:stop', (event) => handleAudiocapStop(event, getRemoteSpaOrigin));
}
