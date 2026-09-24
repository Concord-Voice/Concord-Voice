import type { ScreenAudioDegradeReason } from '../../../main/audiocapHost';

/**
 * Why a share lost its audio, in words fit to show a user.
 *
 * THE ONLY CONSUMER OF `ScreenAudioDegradeReason` THAT A USER EVER SEES. Before
 * this existed the reason was computed on every degrade and rendered nowhere, so
 * a share whose audio died presented as unexplained silence -- the shipped-but-
 * unread shape this epic has now produced four times (#3194's smoke harness,
 * `capture-starved`, `screenAudioBridge.ts`, and this).
 *
 * A `Record`, NOT A SWITCH. A new union member becomes a COMPILE ERROR here
 * rather than falling through a `default` into "something went wrong", which would
 * degrade a real mechanism into no answer at the moment a user needs one. Same
 * shape as `SCREEN_AUDIO_DEGRADE_REASONS` and `START_FAILURE_REASONS`, and for the
 * same reason: `tsconfig.json` includes only `src/**`, so a union enumerated
 * inside a test file is never type-checked and reads like a gate it cannot be.
 *
 * EVERY MESSAGE BUT ONE CARRIES THE WHOLE-SCREEN REMEDY, and the one is the
 * reason this paragraph is not a blanket rule. The others describe a PER-PROCESS
 * start that failed before the child reported `started`. The renderer attempts
 * one only on the `'per-process'` rung, which Linux never reaches (plan OQ1), and
 * on every other platform a whole-screen share takes the `'system-loopback'` rung
 * -- so the remedy is only ever computed where following it yields sound.
 *
 * SHOWN AS A TOAST, since #3394 PR 2. `voiceService.degradeScreenAudio` is the one
 * production caller: it writes `{ mode: 'degraded', reason }` into
 * `voiceStore.screenAudio` and puts this sentence in the voice bar's slot-error toast,
 * for every reason the capture seam produces. Until then only `'produce-rejected'`
 * reached the toast; the other reasons were stored, rendered by nothing, and a
 * refused share (a Safari window, found by that PR's M3 hardware check) went live
 * with no sound and no explanation.
 *
 * `'produce-rejected'` IS THE EXCEPTION, and an earlier version of this comment
 * asserted the rule over it without checking. Its only caller is the catch in
 * `voiceService.produceScreen`'s screen-audio production, which runs on the
 * `chromeMediaSource: 'desktop'` loopback path -- so the only user who can ever
 * read it is ALREADY sharing a whole screen, and the remedy told them to do the
 * one thing they were doing. A justification that covers most arms is not a
 * platform fact; check the call site before appending REMEDY to a new member.
 *
 * A LIVE CAPTURE THAT ENDS IS NOT A DEGRADE (#3394 PR 2). Its reason is a
 * `ScreenAudioInterruptReason`, not one of these, and its copy must never carry
 * REMEDY: telling a user mid-share to share a whole screen would widen what they
 * send (#2161).
 *
 * NOTHING HERE IS LOGGED OR COUNTED PER CAUSE (`observability.md` principle 7).
 * The principle governs telemetry, not telling a user about their own share.
 * `target-unresolved` is the one privacy-adjacent member and has ALREADY collapsed
 * four causes into itself; its single string IS that collapse. Do not split it.
 */
const REMEDY = ' Share a whole screen to include sound.';

const DEGRADE_COPY: Readonly<Record<ScreenAudioDegradeReason, string>> = {
  // Availability: no producer compiled in, or an OS below the capture floor.
  // Deliberately the SAME sentence for both -- from the user's side the outcome
  // is identical, and the difference is an operator concern.
  'no-backend': 'App sound isn’t available on this computer.' + REMEDY,
  'unsupported-os': 'App sound isn’t available on this computer.' + REMEDY,

  // Startup: the helper never got far enough to capture anything.
  'handshake-timeout': 'App sound didn’t start in time.' + REMEDY,
  'load-fault': 'App sound couldn’t start on this computer.' + REMEDY,
  'capability-fault': 'App sound couldn’t start on this computer.' + REMEDY,

  // Before `started`: the start was pending when the helper died, or when the
  // exchange with it broke down. The same two mechanisms AFTER `started` are
  // interrupt reasons, not these (#3394 PR 2).
  'child-crash': 'App sound stopped unexpectedly.' + REMEDY,
  'protocol-fault': 'App sound stopped unexpectedly.' + REMEDY,

  // The capture worked and the call refused to publish it --
  // PARTICIPANT_PRODUCER_LIMITS['screen-audio'] === 1 is the only thing that can
  // do this. A mechanism, not a caller bug, which is why it is a member at all.
  //
  // NO REMEDY, deliberately -- see the exception in the module doc. This is the
  // one arm whose reader is already sharing a whole screen, so it says what
  // happened to THIS share instead of prescribing what they have already done.
  'produce-rejected':
    'This call can’t carry another audio track, so your screen is being shared without sound.',

  // The collapsed member. FOUR causes live behind this one string: below floor,
  // the machine snapshot had not arrived, the handle did not resolve to an owner,
  // and the app closed between picking and starting. Naming any of them would
  // reintroduce the discriminator Task 11 collapsed -- so this string names NONE
  // of them, deliberately, and a test pins that it never leaks "closed",
  // "version", "snapshot", a platform name, or "pid"/"process id".
  'target-unresolved': 'We couldn’t capture that app’s sound.' + REMEDY,
};

/**
 * `Object.hasOwn`, NOT a bare `DEGRADE_COPY[reason]` — the same guard
 * `startFailureMessage` already carries, for the same reason. The map is an
 * object literal and inherits `constructor`, `toString`, `valueOf` and the rest
 * of `Object.prototype`, so a `reason` of `'constructor'` makes a bare lookup
 * return a FUNCTION: truthy, defined, and rendered straight into a video-slot
 * error. The closed set failing open through the one door a closed set shuts.
 *
 * The PARAMETER stays typed rather than widening to `unknown`, because the
 * compile-time contract is what keeps the `Record` exhaustive and a new union
 * member a build error. The guard is for the boundary the type cannot see, and
 * since #3198 PR 3 that boundary exists: `AudiocapStartResult.reason` crosses
 * from main over `audiocap:start`, and the preload bridge and the capture seam
 * store it in `voiceStore.screenAudio` as it arrived, checked against no anchor.
 * A remote SPA runs on shells older and newer than itself, so a reason this build
 * does not know is a version-skew fact rather than a hypothesis. No such value
 * reaches this function YET -- its one production call site passes a literal --
 * so the guard is for any surface that renders the stored reason. It also
 * matches `startFailureMessage`, and two sibling closed-set lookups should not
 * disagree about whether their source is trusted.
 */
export function screenAudioDegradeMessage(reason: ScreenAudioDegradeReason): string {
  if (typeof reason === 'string' && Object.hasOwn(DEGRADE_COPY, reason)) {
    return DEGRADE_COPY[reason];
  }
  return 'Your screen is being shared without sound.';
}
