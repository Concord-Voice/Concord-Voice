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
 * A `Record`, NOT A SWITCH. An eleventh union member becomes a COMPILE ERROR here
 * rather than falling through a `default` into "something went wrong", which would
 * degrade a real mechanism into no answer at the moment a user needs one. Same
 * shape as `SCREEN_AUDIO_DEGRADE_REASONS` and `START_FAILURE_REASONS`, and for the
 * same reason: `tsconfig.json` includes only `src/**`, so a union enumerated
 * inside a test file is never type-checked and reads like a gate it cannot be.
 *
 * NINE OF TEN MESSAGES CARRY THE WHOLE-SCREEN REMEDY, and the tenth is the
 * reason this paragraph is not a blanket rule. The nine are reachable only AFTER
 * a per-process start is attempted -- and THROUGH PR 2 NOTHING ATTEMPTS ONE, on
 * any platform. `'produce-rejected'` is the only arm with a production caller;
 * the other nine are written, exhaustive and unreached, exactly as
 * `'capture-starved'` already discloses of itself below. An earlier version of
 * this paragraph named a PLATFORM precondition (Windows and macOS only, Linux
 * never leaving the `'none'` rung), which is true but is not the operative one
 * and reads as though the other nine reach users on two of three platforms
 * today. They reach nobody. The whole-screen remedy is therefore correct-by-
 * vacuity for now and correct-by-construction once PR 3 wires the seam, since a
 * per-process start is refused outright on Linux (plan OQ1).
 *
 * `'produce-rejected'` IS THE EXCEPTION, and an earlier version of this comment
 * asserted the rule over it without checking. Its only caller is the catch in
 * `voiceService.produceScreen`'s screen-audio production, which runs on the
 * `chromeMediaSource: 'desktop'` loopback path -- so the only user who can ever
 * read it is ALREADY sharing a whole screen, and the remedy told them to do the
 * one thing they were doing. A justification that covers nine of ten arms is not
 * a platform fact; check the call site before appending REMEDY to a new member.
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

  // Mid-share: it was running and stopped.
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

  // ADVISORY AND CURRENTLY UNREACHABLE. `capture-starved` has no producer: the
  // route the design names is status().faulted -> fault{stage:'run'} -> here, and
  // neither leg exists. Copy is supplied because the Record demands exhaustiveness,
  // NOT because a user can see it today. It must never read as an accusation --
  // a granted tap on a paused app is byte-identical to a denied one (#3197 R9), so
  // this says what was observed and nothing about why.
  'capture-starved': 'No sound has come through from that app.' + REMEDY,
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
 * compile-time contract is what keeps the `Record` exhaustive and an eleventh
 * union member a build error. The guard is for the boundary the type cannot
 * see. The guard is DEFENCE-IN-DEPTH FOR A BOUNDARY PR 3 OPENS, not for one
 * that exists: no reason reaches this function over IPC today. `reasonForFaultStage`
 * feeds `AudiocapStartResult.reason`, whose only production consumer is
 * `runCapabilityProbe`, which stores it and `console.debug`s it -- nothing forwards
 * a `ScreenAudioDegradeReason` to the renderer. The guard stays (it matches
 * `startFailureMessage`, and two sibling closed-set lookups should not disagree
 * about whether their source is trusted); what changed is that this paragraph no
 * longer justifies it by a sender that is not yet there. The IPC path it
 * anticipates lands with the `audiocap:start` handler in PR 3.
 */
export function screenAudioDegradeMessage(reason: ScreenAudioDegradeReason): string {
  if (typeof reason === 'string' && Object.hasOwn(DEGRADE_COPY, reason)) {
    return DEGRADE_COPY[reason];
  }
  return 'Your screen is being shared without sound.';
}
