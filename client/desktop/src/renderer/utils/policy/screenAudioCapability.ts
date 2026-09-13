import { parseWindowSourceId } from '../../../shared/parseWindowSourceId';

/**
 * WHICH audio mechanism a capture target admits — deliberately not a boolean.
 *
 * A boolean says only that *some* audio is authorised, never which capture shape, so
 * every caller had to re-derive the mechanism from the id and platform it had just
 * handed in. `'system-loopback'` names the one shape that exists today: the
 * `chromeMediaSource: 'desktop'` whole-desktop tap, which no caller may request off any
 * other verdict — #2161 lived on exactly that line.
 *
 * The union is also what makes a future rung (per-process capture, #3198) arrive as a
 * decision at every call site rather than as a silently widened capture path: a
 * non-empty string is truthy, so a caller still testing this value for truthiness would
 * wave a new mechanism straight into the loopback request. Consumers therefore compare
 * against an exact verdict, and the capture seam switches exhaustively.
 */
export type ScreenAudioVerdict = 'none' | 'system-loopback' | 'per-process';

/**
 * Can a given capture target carry audio on this platform, and by what mechanism?
 *
 * ONE authority, deliberately. The picker and the capture path both need this answer,
 * and when they each computed it inline they drifted: the picker tested only the
 * `screen:` prefix and so offered an enabled, default-on Stream Audio control on Linux,
 * where this capture path has no loopback at all and silently falls back to video.
 *
 * Two independent reasons a target cannot carry audio:
 *
 *   1. It is a window or application. Electron's desktop audio capture is a
 *      whole-system loopback that ignores `chromeMediaSourceId`, so a window share
 *      asking for audio sends every application's sound to the channel (#2161).
 *      Per-application audio needs a native addon — see ADR-0043.
 *   2. The platform has no loopback. Linux is the known case.
 *
 * An UNKNOWN platform resolves to allowed rather than refused, and that is not a
 * fail-open: it is the dev/web path, which reaches `getDisplayMedia` — OS-mediated
 * consent for one user-chosen surface — not the whole-desktop loopback #2161 is about.
 * The capture path still gates on the prefix regardless of what this returns.
 *
 * THE THIRD RUNG (#3198). `machineCapability` is the machine's per-process claim,
 * pushed from main over IPC contract 28 and held in
 * `voiceStore.machineScreenAudioCapable`. It is an AFFORDANCE input: it decides what to
 * OFFER and what to SAY. Every enforcement decision is re-derived in main from inputs
 * main re-validates, so a renderer that lies to itself about this value gains nothing.
 *
 * THE SINGLE-AUTHORITY RULE IS WHY THIS FUNCTION EXISTS, and it is why the rung was
 * added here rather than inline at a call site: the picker and the capture path each
 * computed this themselves once, and they drifted. Extend this function; never add a
 * second copy of the policy.
 */
export function canCarryScreenAudio(
  sourceId: string | null | undefined,
  platform: string | null | undefined,
  machineCapability?: boolean | null
): ScreenAudioVerdict {
  // TYPE-GUARD FIRST, because `.startsWith()` below is a METHOD CALL on this value and
  // runs BEFORE the type-guarded parse. Found by #3198's pre-PR adversarial pass: a
  // duck-typed non-string (`{ startsWith: () => true }`) short-circuited straight to
  // 'system-loopback' -- the one verdict that grants the whole-system mix. Unreachable
  // today (every caller passes a string from main-authoritative IPC), but the helper
  // this function delegates to guards its own input and the function that GRANTS the
  // mix did not, and that asymmetry is the wrong way round.
  if (typeof sourceId !== 'string' || sourceId.length === 0) return 'none';
  // Linux first. It has NEITHER a loopback NOR a per-process backend, so it
  // short-circuits both remaining rungs rather than being re-tested inside each.
  //
  // TYPE-GUARDED TOO, for the symmetry the #3198 Phase-8 review asked for. `=== 'linux'`
  // is a strict primitive compare, so a duck-typed or boxed value (`new String('linux')`)
  // slips past the Linux rung. The damage is downgrade-only — a Linux SCREEN share then
  // attempts a loopback that does not exist, `getUserMedia` throws, and the capture falls
  // back to video-only — and it can never move a `window:` target, which does not reach
  // the grant below. Guarded anyway: the argument for guarding `sourceId` was that the
  // helper it delegates to guards its own input while the function that GRANTS the mix did
  // not, and leaving the sibling parameter unguarded reproduces that asymmetry.
  //
  // `null`/`undefined` STAY PERMISSIVE and are not folded into this guard. They mean "not
  // resolved yet" — the dev/web path and the picker's pre-probe render both pass `null`
  // (`ScreenSharePicker.tsx:303`) — and refusing them would turn an unresolved platform
  // into a silent audio refusal on every non-Electron share. Only a value that is neither
  // a string nor absent is untrusted.
  if (platform !== null && platform !== undefined && typeof platform !== 'string') return 'none';
  if (platform === 'linux') return 'none';
  if (sourceId.startsWith('screen:')) return 'system-loopback';
  // The per-process rung (#3198). `=== true` is deliberate: `null` (the snapshot has
  // not arrived), `undefined` (a shell below IPC contract 28, or a caller not yet
  // threaded) and `false` all resolve to the pre-addon answer. Absence IS the
  // fail-closed state, never "audio is fine".
  if (parseWindowSourceId(sourceId) !== null) {
    return machineCapability === true ? 'per-process' : 'none';
  }
  // Prefix allowlist, not a window denylist: an id shape we do not recognise must not
  // fall through to "audio is fine". Fail closed on the unknown.
  return 'none';
}

/**
 * Does this verdict mean the UI may OFFER a Stream Audio control for the target?
 *
 * EXISTS BECAUSE THE UNION'S OWN COMPILE-TIME PROMISE WAS NOT KEPT. The docblock above
 * says a new rung "arrives as a decision at every call site", and three call sites were
 * converted to exhaustive switches — but the fourth, `ScreenSharePicker`, tested
 * `=== 'system-loopback'`, and an equality test against a widened union compiles
 * silently. Widening the union from two members to three produced zero errors at the one
 * consumer that decides whether the control appears at all (#3198 Phase-8 review).
 *
 * The picker's own comment reasoned carefully about drift and defended exactly one
 * direction: an equality test does stop a future verdict from lighting the control up
 * wrongly. It guarantees the inverse instead — the control stays DARK on a machine that
 * can carry audio, which is silent-feature-death rather than a privacy breach, and is the
 * failure class ADR-0043's epic exists to close.
 *
 * `'per-process'` IS FALSE HERE, AND THAT IS THE FAIL-CLOSED CHOICE FOR PR 1 OF 2.
 * The rung is unreachable in this PR (no production call site passes the third argument
 * to `canCarryScreenAudio`), so no user meets either answer. `false` is nonetheless the
 * one to ship: `voiceService.setScreenAudioEnabled` REFUSES `'per-process'` until PR 2
 * wires `start({ targetPids })`, so `true` here would mean an enabled control that errors
 * on every click — which is precisely the outcome this PR's own description argues against
 * making reachable. PR 2 flips this to `true` in the same change that lands the capture
 * wiring, and the agreement test in `voiceService.captureSeam.test.ts` fails if it does
 * not.
 */
export function verdictOffersAudio(verdict: ScreenAudioVerdict): boolean {
  switch (verdict) {
    case 'system-loopback':
      return true;
    case 'per-process':
      return false;
    case 'none':
      return false;
    default: {
      const unhandled: never = verdict;
      console.debug('verdictOffersAudio: unhandled verdict', unhandled);
      return false;
    }
  }
}
