// concord-audiocap utilityProcess HOST — runs in main (#3195, ADR-0043).
//
// Owns the whole life of the capture child: forks it with a fully-specified
// environment, validates its handshake, decides the capability rung itself,
// fences every asynchronous continuation behind a generation token, kills it
// from four quit hooks, and probes the machine's capability once at app start.
//
// It ABSORBS `audiocapSmoke.ts` rather than calling it (plan Task 3): a caller
// would leave two lifecycle owners for one OS process and keep a diagnostic
// entry point inside app.asar forever. Everything that file proved — the env
// allowlist, the fork/handshake/timeout skeleton, the settle-once latch — is
// here, and its tests came with it.
//
// ---------------------------------------------------------------------------
// THE INVARIANTS, STATED BEFORE THE CODE
// ---------------------------------------------------------------------------
//
// I1. AT MOST ONE CHILD. `session` is the single handle. Everything that can
//     terminate a child nulls it, and nulling it is what makes `killAudiocapHost`
//     idempotent — not a `killed` flag, which would need its own reset rule.
//
// I2. EVERY CONTINUATION IS FENCED ON SESSION IDENTITY, not on the generation
//     number. A `message`, `exit`, `error` or handshake timeout acts only while
//     `session === ` the session it was registered for. Identity is exact where
//     an ordering comparison is not: it survives a caller that reuses or repeats
//     a generation, and it cannot be satisfied by a later
//     session that happens to carry the same number. `currentAudiocapGeneration()`
//     projects that identity outward for callers that hold only a number.
//
//     WHAT BREAKS IT: a continuation registered on the module (rather than
//     closed over its session) — e.g. hoisting a handler out of `startAudiocapHost`
//     "to avoid re-creating closures". Then a dying child's exit settles a
//     successor's promise, which is the exact shape #3195 exists to avoid on the
//     `switchScreenSourceQueued` path.
//
// I3. A START ALWAYS SUPERSEDES. `startAudiocapHost` kills any live child before
//     forking, synchronously and with no `await` in between. The caller's
//     generation is therefore expected to be monotonically non-decreasing;
//     `voiceService`'s per-source screen serialization tail (frontend.md
//     § "Every camera/screen re-produce path is serialized per source") is what
//     guarantees it. WHAT BREAKS IT: calling this outside that tail, where a
//     stale lower-generation start could kill a newer child. There is
//     deliberately no entry-side ordering guard, because refusing a start needs
//     a degrade reason and `ScreenAudioDegradeReason` is a closed enum of
//     MECHANISMS (spec §5 Q4) — inventing a "superseded" member to describe a
//     caller bug would put a non-mechanism into user-facing copy.
//
// I4. THE PROMISE SETTLES EXACTLY ONCE, AND THE FIRST OUTCOME WINS. On the PROBE
//     path a child that says `hello` and then exits reported a capability;
//     collapsing that into "child died" would turn a real answer into a spurious
//     packaging defect. Inherited from `audiocapSmoke.ts`'s `settled` latch and
//     still locked by its migrated vacuity control.
//
//     A CAPTURE differs by design (#3394): its `hello` does not settle the
//     promise -- it enters `starting`, and only the child's `started` ack does.
//     A capture child that exits during `starting` has therefore reported nothing
//     yet, and settling `child-crash` is the truth rather than a collapse: it died
//     before it could say whether the tap exists.
//
// I5. MAIN DECIDES THE RUNG. `capability.perProcessAudio` arriving over
//     `parentPort` is a NECESSARY INPUT that main ANDs with facts it owns; it is
//     never a grant (spec §7). `isAudiocapHello` is strict — a truthy `1` is
//     REJECTED rather than coerced — so a hostile or buggy child claiming
//     capability is refused here, not one layer down. This is #2161's defect
//     arriving on a different channel.
//
// I6. ZERO RESPAWN (spec §5 Q3). A crash is ADR-0043 D4b risk 4 materializing.
//     There is no retry, and deliberately no "one retry if the crash preceded
//     the first quantum" refinement: that is the packaging-defect signal, and it
//     must be the loudest case rather than the quietest. The next share forks
//     fresh.
//
// I7. PUBLISH ONLY THE INVOLUNTARY END OF A LIVE CAPTURE (#3394 PR 2; spec §2 I5,
//     which numbers it differently). `retire()` is the one publisher. It
//     publishes iff three facts held WHEN THE CAUSE ARRIVED: the caller named an
//     interrupt reason (non-null), `session === live` (I2), and `hostState ===
//     'capturing'` (the start promise already settled `ok:true`, so the renderer
//     holds a bridge that nothing else will tell it is dead). The decision is
//     taken at entry, before `retire` forgets the session; the listener runs
//     last, after the child is reaped and the promise settled, inside a
//     try/catch. At most once per session: the first retire forgets it, and the
//     `session !== live` fence drops everything that child says afterwards.
//
//     The child is information, never authorization (spec §2 I1). What it can
//     steer is WHICH of three main-owned reasons describes its own capture
//     ending -- which it could already end by exiting. The generation is main's,
//     and the reason is main's mapping of the cause, never the child's text.
//
//     WHAT BREAKS IT: deciding after `session = null` (never publishes, and the
//     renderer keeps a bridge around a dead child -- the silent per-process
//     defect); dropping the `'capturing'` conjunct (a start-phase refusal is
//     both settled AND published, so the renderer is told twice about a share
//     it never had); and any voluntary path -- stop, kill, supersede,
//     `killAudiocapCapture`, the probe -- passing a reason. Those paths do not
//     call `retire` at all today; a future one that does passes `null`.

import path from 'node:path';
import { app, utilityProcess, MessageChannelMain, type UtilityProcess } from 'electron';
import {
  CHANNELS,
  CREDIT_BOUND,
  FAULT_MESSAGE_MAX_CHARS,
  FRAME_COUNT,
  HANDSHAKE_TIMEOUT_MS,
  QUANTUM_MS,
  RING_SLOTS,
  SAMPLE_RATE,
  START_ACK_TIMEOUT_MS,
  isAudiocapFault,
  isAudiocapHello,
  isAudiocapStarted,
  sanitizeDiagnostic,
  type AudiocapCapability,
  type AudiocapFaultStage,
  type AudiocapHello,
  type AudiocapInterrupted,
  type AudiocapStart,
  type ScreenAudioInterruptReason,
} from '../shared/audiocapProtocol';
import { NATIVE_ADDON_ENV, resolveNativeAddonPath } from './nativeAddonPath';

/**
 * Why the local share has no audio. A closed enum of MECHANISM strings — never a
 * cause dimension on a counter, and never a privacy-decision discriminator (C8
 * principle 7).
 *
 * Every member but one describes the CAPTURE path: the child, the addon it hosts,
 * or main's own refusal before any child is asked (`unsupported-os`, and the
 * main-side legs of `target-unresolved` and `protocol-fault`). `produce-rejected`
 * is the one that does not: the capture succeeded and the SFU refused to publish
 * it — today only `PARTICIPANT_PRODUCER_LIMITS['screen-audio'] === 1` can do that.
 * It is still a mechanism rather than a caller bug, which is the test spec §5 Q4
 * sets for membership, and the share it describes is video-only exactly like the
 * others.
 *
 * Deliberately no count. This said "Six of the seven" while the union held ten;
 * `SCREEN_AUDIO_DEGRADE_REASONS` below is the membership list the compiler checks.
 *
 * A START PRODUCES THESE; A LIVE CAPTURE THAT ENDS DOES NOT (#3394 PR 2). Once
 * `started` has settled the promise, an involuntary end is an `AudiocapInterrupted`
 * whose reason is a `ScreenAudioInterruptReason` (`src/shared/audiocapProtocol.ts`)
 * -- see I7 above. `capture-starved` was a member until then: an advisory for a
 * silent tap with no producer on any path, removed together with the silence
 * advisory it belonged to. Do not re-add a mid-share reason here.
 */
export type ScreenAudioDegradeReason =
  | 'no-backend'
  | 'handshake-timeout'
  | 'child-crash'
  | 'load-fault'
  | 'capability-fault'
  | 'protocol-fault'
  | 'produce-rejected'
  | 'unsupported-os'
  // Main could not turn the renderer's source id into a live window handle, or
  // the child could not turn that handle into an owning PID. ONE member for both
  // legs, and for all of: a malformed id, a `screen:` id, a window that closed
  // between pick and start, and an OS call that refused.
  //
  // COLLAPSED DELIBERATELY (#3198 A7, observability.md principle 7). Splitting it
  // by cause would be a privacy-decision discriminator -- the same ruling that
  // forbids a reason dimension on `presence_audience_suppressed_total`.
  //
  // BOTH LEGS ARE NOW LIVE. The CHILD-side one always was: `audiocapChild.ts`
  // resolves the handle before it asks the addon for anything, and a refusal arrives
  // here as `fault{stage:'target'}`. The MAIN-side legs -- a malformed id, a
  // `screen:` id, a window absent from a live enumeration -- arrive from fences 2-4
  // of `handleAudiocapStart` in `src/main/ipc/audiocap.ts`, which #3198 PR 3 added.
  // This comment said that file "does not exist yet" until PR 3 created it.
  | 'target-unresolved';

/**
 * THE EXHAUSTIVENESS ANCHOR for `ScreenAudioDegradeReason`, and it exists
 * because a test cannot supply one.
 *
 * `tsconfig.json` includes only `src/**` (verified: zero files under `tests/`
 * are in the type program), so a union enumerated as a typed array inside a
 * test file is NEVER type-checked — it reads like a gate and can never fail.
 * Declaring the set HERE makes a missing member a compile error and gives the
 * test something real to assert against at runtime. Same reasoning, and the
 * same shape, as `START_FAILURE_REASONS` in audiocapChild.ts.
 *
 * A MEMBERSHIP SET, NOT A PHRASE MAP. #3197 PR 2 shipped no user-facing copy for
 * these — the renderer had no consumer for the union, and adding one would have
 * been new UI in a PR that shipped dark. #3198 Task 13b owns the copy: see
 * `renderer/utils/policy/screenAudioDegradeCopy.ts` for the phrase map.
 */
export const SCREEN_AUDIO_DEGRADE_REASONS: Readonly<Record<ScreenAudioDegradeReason, true>> = {
  'no-backend': true,
  'handshake-timeout': true,
  'child-crash': true,
  'load-fault': true,
  'capability-fault': true,
  'protocol-fault': true,
  'produce-rejected': true,
  'unsupported-os': true,
  'target-unresolved': true,
};

export type AudiocapStartResult =
  | {
      ok: true;
      generation: number;
      /**
       * The child's CLAIM, already narrowed by `isAudiocapHello` — a truthy `1`
       * was refused before it got here. I5: a necessary input main ANDs with
       * facts it owns, never a grant.
       *
       * On the PROBE path this is the claim itself and may be `false`. On the
       * CAPTURE path it is `true` by construction: `handleHello` refuses a
       * capture whose claim is false (`no-backend`), and only `handleStarted`
       * settles `ok: true` there, after the child reports the tap exists.
       */
      perProcessAudio: boolean;
    }
  | { ok: false; reason: ScreenAudioDegradeReason };

/**
 * Spec §6b, extended by #3394. A capture passes `handshaking → ready → starting →
 * capturing`: `starting` is the window between posting `start` and hearing `started`,
 * during which a fault still reaches the start promise. The probe stops at `ready`.
 */
type HostState =
  'idle' | 'spawning' | 'handshaking' | 'ready' | 'starting' | 'capturing' | 'stopping' | 'faulted';

type ChildProcess = ReturnType<typeof utilityProcess.fork>;

interface HostSession {
  readonly generation: number;
  readonly child: ChildProcess;
  /**
   * The `HWND` / `CGWindowID` this session captures, or `null` for the app-start
   * capability probe, which has no window and must never post a `start`.
   *
   * REINTRODUCED BY #3198 PR 3, alongside the leg that reads it. PR 2's review
   * DELETED this field because nothing consumed it (m2, on #3195's precedent of
   * deleting an unwired watchdog rather than shipping it inert). The field is back
   * because `handleChildMessage`'s hello arm now posts `{kind:'start', …}` with it.
   */
  readonly windowHandle: number | null;
  /** Resolves `startAudiocapHost`. Cleared by the first outcome (I4). */
  settle: ((result: AudiocapStartResult) => void) | null;
  handshakeTimer: NodeJS.Timeout | null;
  /** Armed on entering `starting`; cleared by `started`, and by `clearTimers` on every exit. */
  startAckTimer: NodeJS.Timeout | null;
  capability: AudiocapCapability | null;
}

/**
 * BUILD THE CHILD ENVIRONMENT FROM AN ALLOWLIST (#3194 review: CWE-497 + CWE-178).
 *
 * This was `{ ...process.env }` followed by `delete childEnv[NATIVE_ADDON_ENV]`,
 * and that shape had two defects that an allowlist closes at once.
 *
 * 1. CWE-497, handing the addon main's whole environment. ADR-0043 D5 puts the
 *    addon in a utilityProcess precisely so a memory-safety bug in rt/ cannot own
 *    the process holding SSO tokens and the update path. Copying every variable
 *    into that process gives back part of what D5 buys —
 *    GOOGLE_OAUTH_CLIENT_SECRET_DESKTOP is a documented main-process fallback, so
 *    a real secret is in scope. `utilityProcess.fork({env})` REPLACES the parent
 *    environment rather than merging it (Electron documents the `env` option's
 *    default as `process.env`, and sets `clear_environment=true` whenever any env
 *    object is supplied — `electron_api_utility_process.cc`:
 *    `.WithEnvironment(env_map, env_map.empty() ? false : true)`), so an allowlist
 *    genuinely is the whole environment the child sees — this is not defence that
 *    something else quietly undoes.
 *
 * 2. CWE-178, the case-sensitive delete. `{ ...process.env }` produces a PLAIN
 *    object whose keys are case-sensitive, while Windows env lookup is not. An
 *    attacker running `setx concord_audiocap_path ...` left a key the delete did
 *    not match and the child's own lookup still resolved — defeating exactly the
 *    denial-of-service protection the previous comment here claimed to provide.
 *    Under an allowlist a key that is not on the list never reaches the child at
 *    all, whatever its casing, so the collision is unrepresentable rather than
 *    filtered. That is why this is an allowlist and not a case-folding compare.
 *
 * The allowlist is deliberately minimal. PATH is required on Windows for the
 * dependent-DLL search LoadLibrary performs; SystemRoot and windir are required by
 * Windows itself for a process to start at all; TMPDIR/TEMP/TMP keep temp-file
 * resolution sane. Nothing here carries credentials.
 *
 * Do not add a `NODE_OPTIONS` passthrough, and do not spread `process.env`
 * "temporarily for debugging" (spec §7).
 */
export const ENV_ALLOWLIST: readonly string[] = [
  'PATH',
  'SystemRoot',
  'windir',
  'TMPDIR',
  'TEMP',
  'TMP',
];

/**
 * NO WATCHDOG RAIL HERE, DELIBERATELY (spec §6c rail 3, removed 2026-09-10).
 *
 * The rail's condition is "no credit ack AND no live share", which describes a
 * CAPTURING child. It was removed while no capturing child existed, because a
 * timer guarding nothing is the "shipped code with no caller" defect §6c calls
 * UNFINISHED rather than defence. That premise ended with #3198 PR 3, which
 * posts `{kind:'start'}` (`beginCapture`, from `handleHello`); the rail was not
 * reintroduced then.
 * #3394 PR 1 closes the renderer-reload and renderer-crash halves of its case
 * with `wireAudiocapRendererLoss`. What stays uncovered is a renderer that is
 * alive but has stopped acking credits: the child drops quanta at its ring and
 * keeps the OS tap until stop, supersede or quit. Reintroducing the rail for that
 * case is an open decision, not one this file has taken.
 */

let session: HostSession | null = null;
let hostState: HostState = 'idle';

/**
 * The generation of the LIVE child, or 0 when there is none.
 *
 * Deliberately liveness-scoped rather than "highest generation ever seen": the
 * sixth currentness term added at `voiceService.ts:3977-3987` wants to reject a
 * continuation both when a newer share superseded it AND when the host was torn
 * down under it. One comparison covers both.
 */
export function currentAudiocapGeneration(): number {
  return session?.generation ?? 0;
}

/**
 * The environment the child gets, in full. Every allowlisted key is present —
 * absent-in-the-parent becomes empty rather than missing — so the child's
 * environment has one shape on every machine and cannot be read as a signal of
 * which variables main happens to hold. `os.tmpdir()` treats an empty
 * TMPDIR/TEMP/TMP as absent (they are falsy), and the Windows-only keys are
 * always populated on Windows, so the empty form is never load-bearing.
 */
function buildChildEnv(addonPath: string): NodeJS.ProcessEnv {
  const childEnv: NodeJS.ProcessEnv = {};
  for (const key of ENV_ALLOWLIST) {
    childEnv[key] = process.env[key] ?? '';
  }
  // Set the variable ONLY when packaged. The loader treats CONCORD_AUDIOCAP_PATH as
  // a value that must EQUAL the one path it derives from process.resourcesPath — so
  // in dev, where the addon lives in the node-gyp output tree instead, any value at
  // all is refused. It is a cross-check, never a way to choose what gets loaded.
  if (app.isPackaged) {
    childEnv[NATIVE_ADDON_ENV] = addonPath;
  }
  return childEnv;
}

/**
 * A `fault` names the stage that failed; main maps it to a degrade mechanism.
 *
 * `'run'` IS EXCLUDED BY TYPE. It is the one stage that describes a capture that was
 * live, and a live capture's end is an interrupt, never a degrade (I7). The fault arm
 * of `handleChildMessage` handles it before calling this, so a new caller cannot hand
 * a mid-share fault to the start-result union without a compile error.
 */
function reasonForFaultStage(stage: Exclude<AudiocapFaultStage, 'run'>): ScreenAudioDegradeReason {
  switch (stage) {
    case 'guard':
    case 'load':
      return 'load-fault';
    case 'capability':
      return 'capability-fault';
    case 'start':
      return 'no-backend';
    // NOT 'no-backend'. The child refused before the addon was asked to capture,
    // so nothing was ever tapped and the machine's backend is not in question
    // (#3198 spec §4.3).
    case 'target':
      return 'target-unresolved';
    case 'protocol':
      return 'protocol-fault';
  }
}

function clearTimers(live: HostSession): void {
  if (live.handshakeTimer) {
    clearTimeout(live.handshakeTimer);
    live.handshakeTimer = null;
  }
  if (live.startAckTimer) {
    clearTimeout(live.startAckTimer);
    live.startAckTimer = null;
  }
}

/** I4: the first outcome wins; later ones are dropped rather than overwritten. */
function settleOnce(live: HostSession, result: AudiocapStartResult): void {
  const settle = live.settle;
  if (!settle) return;
  live.settle = null;
  settle(result);
}

/**
 * Terminal for THIS session: stop its timers, forget it, kill its child, and --
 * only for the involuntary end of a live capture -- tell main (I7).
 *
 * Killing after forgetting is what makes the kill safe to issue from anywhere —
 * the `exit` that follows finds `session !== live` and does nothing.
 *
 * `interrupt` IS REQUIRED, WITH NO DEFAULT. Every caller names its cause, and a
 * cause that is not an involuntary mid-share end names `null`. A defaulted
 * parameter is the `screenAudioRefusalMessage(platform = null)` defect shape: the
 * compiler stops naming the call sites that have to decide.
 */
function retire(
  live: HostSession,
  result: AudiocapStartResult,
  nextState: HostState,
  interrupt: ScreenAudioInterruptReason | null
): void {
  // I7: decided at ENTRY, before any mutation, from the state the cause arrived in.
  const publish = interrupt !== null && session === live && hostState === 'capturing';
  if (session === live) {
    session = null;
    hostState = nextState;
  }
  clearTimers(live);
  // `reapChild`, not a bare `kill()`: a handshake timeout can fire while the
  // child is still spawning, and a bare kill there returns false and leaves it
  // to come up unreachable. See `reapChild`.
  reapChild(live.child);
  settleOnce(live, result);
  // LAST, so a listener that throws cannot skip the reap or the settle, and one
  // that re-enters `startAudiocapHost` finds this session already forgotten.
  // `interrupt !== null` is repeated so TypeScript narrows it; `publish` alone does not.
  if (publish && interrupt !== null) {
    try {
      onInterrupted?.({ generation: live.generation, reason: interrupt });
    } catch {
      // Fixed string only -- never the caught value, which may carry an
      // `Error.cause` (observability.md principle 3). No reason either: a line
      // per cause is the reason dimension principle 7 rules out.
      console.warn('[audiocap] interrupt listener threw');
    }
  }
}

/**
 * Reap the child. Synchronous, idempotent, fire-and-forget.
 *
 * C3 / #1383: `utilityProcess.kill()` is synchronous and there is deliberately
 * no graceful-shutdown handshake to await. An `await` on this path re-enters the
 * quit-deadlock veto window, and no quit hook may take one.
 *
 * A start still in flight settles `child-crash` — the child is gone, which is
 * what that mechanism means. Callers that stop a share deliberately discard the
 * result through their own currentness fence, so this value never reaches copy.
 */
export function killAudiocapHost(): void {
  // A child quiescing under `stopAudiocapHost` has no `session`, so it would survive
  // this call and the fork that follows it. DRAIN FIRST, and note WHY that ordering
  // is not merely tidy: `startAudiocapHost` supersedes by calling this and then
  // forking, so a lingering child would hold a live OS tap while the next share's
  // child opens its own — two taps where the design says one, on the privacy surface
  // this whole epic exists to narrow.
  drainStoppingChild();

  const live = session;
  if (!live) return;
  session = null;
  hostState = 'stopping';
  clearTimers(live);
  reapChild(live.child);
  settleOnce(live, { ok: false, reason: 'child-crash' });
  hostState = 'idle';
}

/**
 * Retire the live CAPTURE session because the renderer that owned it is gone (#3394, spec C15).
 *
 * Before this, a reload or renderer crash left the capture child holding its OS tap until
 * the next share or quit: `render-process-gone` only logged, and nothing watched a
 * main-frame navigation. That is the privacy invariant #3197 exists for, reachable by an
 * ordinary Cmd+R.
 *
 * SPARES THE PROBE. The app-start probe has no window and overlaps the first page load by
 * design; killing it there would reap it before its `hello` on every cold start, memoizing
 * `child-crash` as the probe result and leaving the machine-capability snapshot `null` until
 * the first share's own `hello`. A start still pending in `'starting'` settles `child-crash`
 * like any other kill; the renderer that would read it is the one that just went away.
 *
 * THE IMMEDIATE KILL, DELIBERATELY NOT THE GRACEFUL `stopAudiocapHost`. Process exit destroys
 * the OS tap at once, and the document that owned it is already gone; the graceful stop would
 * leave the tap alive for up to STOP_QUIESCE_MS while the child acted on `stop`, for a share
 * nobody can see any more. The cost is stated rather than hidden: the child's `handleStop` is
 * the only reader of the addon's TEARDOWN evidence in `status()` -- `quiesceProved`,
 * `destroyFailures` and the R9 counters `signalTotal` / `silentSinceStart` -- so on this path
 * those go unread. The claim is about those fields, not about `status()` itself: a mid-capture
 * reader that looks only at `faulted` (spec §4.1's fault watch) forfeits nothing here.
 *
 * PUBLISHES NOTHING (I7): this is a voluntary end, and `killAudiocapHost` never calls `retire`.
 */
export function killAudiocapCapture(): void {
  const live = session;
  if (live?.windowHandle == null) return;
  killAudiocapHost();
}

/**
 * Counts renderer losses, so an `audiocap:start` still awaiting its window enumeration can
 * see one that landed during the await. Without it the loss finds no session to kill, the
 * start forks anyway, and the new document inherits a tap nobody reaps (red-team VULN-A).
 */
let rendererLossEpoch = 0;

export function audiocapRendererLossEpoch(): number {
  return rendererLossEpoch;
}

function noteRendererLoss(): void {
  rendererLossEpoch += 1;
  killAudiocapCapture();
}

/**
 * Wire the renderer-loss signals. Extracted from `main.ts` so the predicate is tested
 * here — `src/main/**` is outside coverage, and a wire nobody reaches is the failure mode
 * this epic keeps meeting.
 */
export function wireAudiocapRendererLoss(contents: Pick<Electron.WebContents, 'on'>): void {
  contents.on('render-process-gone', noteRendererLoss);
  // `did-navigate`, NOT `did-start-navigation`. Electron fires `did-start-navigation`
  // BEFORE `will-navigate`, so keying on it would end the capture for a link click that
  // main.ts's `will-navigate` gate then cancels, silently dropping app audio from a live
  // share. `did-navigate` fires only for the main frame and only once a cross-document
  // navigation COMMITS; in-page navigations fire `did-navigate-in-page` instead. The
  // cost is that on a real reload the tap outlives the old document until commit.
  contents.on('did-navigate', noteRendererLoss);
  // A main-frame navigation that FAILS commits an error page and fires no `did-navigate`,
  // yet the document that owned the capture is gone (measured on Electron 44, red-team
  // VULN-B). ERR_ABORTED (-3) is excluded, as in the self-heal filter: a cancelled
  // navigation leaves the document in place.
  contents.on('did-fail-load', (_event, errorCode, _description, _url, isMainFrame) => {
    if (isMainFrame && errorCode !== -3) noteRendererLoss();
  });
}

/**
 * How long a child gets to act on `stop` before it is reaped anyway.
 *
 * Short on purpose. The child's own teardown budget is #3197's 250 ms quiesce, and
 * this is not a second copy of it — it is only the window in which the child must
 * READ the message and begin. Overshooting costs a real overlap on the supersede path;
 * undershooting costs a liveness line in a dev console.
 */
const STOP_QUIESCE_MS = 150;

/** A child that was told to stop and has not exited yet. At most one. */
let stopping: { child: ChildProcess; timer: NodeJS.Timeout } | null = null;

function drainStoppingChild(): void {
  const pending = stopping;
  if (!pending) return;
  stopping = null;
  clearTimeout(pending.timer);
  reapChild(pending.child);
}

/**
 * End a capture the way the child can observe (#3198 PR 3).
 *
 * SEPARATE FROM `killAudiocapHost`, NOT A REPLACEMENT FOR IT, because the two have
 * incompatible contracts. `killAudiocapHost` is synchronous and must stay so: it is
 * called from `startAudiocapHost`'s supersede (I3, nothing awaited in between) and from
 * the quit hooks, where an await re-enters the #1383 quit-deadlock veto window. A
 * graceful stop necessarily lets the child outlive the call, so folding one into the
 * other would either break the supersede or make the quit path await.
 *
 * WHAT POSTING `stop` BUYS, since killing alone already destroys the tap (process exit
 * reaps it — #3197). The child's `handleStop` is the ONLY path on which the addon's
 * TEARDOWN evidence in `status()` is read: `signalTotal` / `silentSinceStart` (the R9
 * counters), `quiesceProved` and `destroyFailures`. It is not necessarily the only caller
 * of `status()` -- a mid-capture reader that looks only at `faulted` (spec §4.1's fault
 * watch) reads none of those. Kill-only computes all of them on every callback and shows
 * them to nobody — which is the shipped-but-unread failure this epic has now produced
 * five times, reproduced one more layer up. The child writes that line to stderr and
 * `startAudiocapHost` echoes it in unpackaged builds.
 *
 * PUBLISHES NOTHING (I7): the user ended this share, and this function never calls
 * `retire`. A late `fault` or `exit` from the detached child is dropped by the
 * `session !== live` fence in its listeners.
 *
 * A non-zero `destroyFailures` means an OS tap may have outlived the share, and the
 * correct response is exactly what the timer already does: kill the process.
 *
 * ITS ONLY CALLER IS THE RENDERER'S `audiocap:stop` (`handleAudiocapStop`). The supersede
 * and quit paths use `killAudiocapHost`, which is why the probe guard below lives here and
 * not there.
 */
export function stopAudiocapHost(): void {
  const live = session;
  // SPARES THE PROBE (#3394 PR 1, R4), mirroring `killAudiocapCapture`: the windowless
  // app-start probe is no renderer's capture, so a renderer's stop has nothing to end. This
  // was worse than the supersede case, not equivalent to it. A share that reaps the probe
  // heals the snapshot from its OWN `hello`; a stop forks nothing, so `child-crash` was
  // memoized as the probe result, the machine-capability snapshot stayed `null`, and
  // per-process audio was gone for the life of the process.
  //
  // `?.` against a STRICT `null` is the discriminator: no session reads `undefined` and falls
  // through to the ordinary path below; only a live session with no window returns here.
  // (`killAudiocapCapture` compares loosely because it has nothing to do in either case.)
  //
  // RETURNS BEFORE `drainStoppingChild()`, deliberately. By construction nothing is draining
  // while the probe is live: the probe's own `startAudiocapHost` ran its I3 kill -- which
  // drains -- before the probe's session existed, and `stopping` is written only below, after
  // detaching a live CAPTURE session, which I1 says cannot coexist with the probe. And were a
  // child quiescing anyway, a stop that found no capture to end has no business cutting that
  // quiesce short: its reap is already owned by its own timer, and draining it early would
  // forfeit exactly the `status()` read the graceful path exists to obtain.
  if (live?.windowHandle === null) return;

  // THE SAME RULE, FOR THE SAME REASON, when there is no session at all. It used to drain
  // first and return second, so a REDUNDANT stop killed the child the previous stop had just
  // started quiescing -- and a single Stop-sharing click sends two, because the renderer's
  // `stopScreenAudioHost` invokes `audiocap:stop` from every teardown it sits on. Measured
  // (#3394 T0): stop at t, second stop and `reapChild` at t+16 ms, inside `addon.stop()`, so
  // the liveness line was never written on ANY graceful stop. Draining belongs only to the
  // case below, where a new live session needs the single `stopping` slot the old child holds.
  if (!live) return;
  drainStoppingChild();

  // Detach first, for the same reason `retire` forgets before it kills: once `session`
  // is null a late `exit` finds `session !== live` and does nothing.
  session = null;
  hostState = 'stopping';
  clearTimers(live);
  // A no-op once the start has settled, which is the ordinary case — a share cannot end
  // before it began. It matters only for a stop that lands during the handshake, or
  // while `starting` -- after `start` was posted and before `started` (#3394).
  settleOnce(live, { ok: false, reason: 'child-crash' });

  const child = live.child;
  try {
    child.postMessage({ kind: 'stop' });
  } catch {
    // The child is already gone. Reap on the spot rather than waiting out a window for
    // a message nothing will read.
    reapChild(child);
    hostState = 'idle';
    return;
  }

  const timer = setTimeout(() => {
    if (stopping?.child !== child) return;
    stopping = null;
    reapChild(child);
  }, STOP_QUIESCE_MS);
  // `unref` for the same reason the handshake timer does it: a pending reap must never
  // be the thing holding the event loop open at quit.
  timer.unref();
  stopping = { child, timer };

  // The child exiting on its own is the good path and makes the timer redundant.
  child.once('exit', () => {
    if (stopping?.child !== child) return;
    clearTimeout(stopping.timer);
    stopping = null;
  });

  hostState = 'idle';
}

/**
 * Kill a child, INCLUDING one that has not finished spawning.
 *
 * `UtilityProcess.kill()` returns `false` when the OS process does not exist yet
 * -- and it does NOT cancel the pending spawn. The child comes up anyway, with
 * nothing holding a reference to it. Measured on Electron 44.1.1 / darwin, forking
 * and killing in the same tick:
 *
 *     A same-tick kill() returned: false
 *     A after 2.5s: spawned=true exited=false pid=7659
 *     B post-spawn kill() returned: true      <- control
 *     B after 1.5s: exited=true
 *
 * This is exactly the window `startAudiocapHost` opens on itself: I3 supersedes
 * by calling `killAudiocapHost()` and then forking, so two starts in quick
 * succession can leave the FIRST child spawning while `session` already points at
 * the second. The first is then live, unreachable and unreaped -- two processes
 * holding the native addon where the design says one.
 *
 * Bounded, and the bound is worth stating so this is not over-sold: Chromium
 * reaps utilityProcess children when the app exits, verified in the same probe
 * (the surviving pid was gone once the parent died). So this strands a process
 * for the remaining life of the app, never beyond it.
 *
 * REACHABLE SINCE #3198 PR 3. When #3195 wrote this, `startAudiocapHost` had exactly
 * one caller, memoized to run once, so the window could not open. `audiocap:start`
 * (`handleAudiocapStart`) is now a second caller a renderer can invoke repeatedly, and
 * two starts in quick succession open it exactly as described above -- which is why it
 * was fixed ahead of the share path rather than left for that path to inherit.
 *
 * `once('spawn')` rather than a timer: the event is the precise moment the OS
 * process exists, and an unreaped listener on a child that never spawns costs
 * nothing because the child is unreachable either way.
 */
function reapChild(child: UtilityProcess): void {
  if (child.kill()) return;
  child.once('spawn', () => {
    child.kill();
  });
}

/**
 * Build the capture channel, tell the child to start, and hand the renderer its end.
 *
 * Returns `false` rather than throwing, so the hello arm stays a single straight-line
 * decision and every failure lands on one retire.
 *
 * ORDER IS THE DESIGN: the child is told first, the renderer second. A renderer holding
 * a port whose peer was never given to a child waits forever with no signal; a child
 * capturing into a port the renderer has not yet adopted merely queues quanta, which the
 * `MessagePort` buffers until the far end is started. One failure mode is silent and
 * permanent, the other self-corrects in a tick.
 *
 * `port1` goes to the child by TRANSFER, so it is neutered in main the moment
 * `postMessage` returns. The `close()` calls on the failure arm are therefore only
 * meaningful for the port that did NOT transfer — closing a neutered port is a no-op,
 * which is why both are closed unconditionally rather than guarded by which leg failed.
 */
function beginCapture(live: HostSession, windowHandle: number): boolean {
  const sink = onCapturePort;
  // No sink means `main.ts` never registered one — a wiring defect, not a machine
  // fact. Fail closed: a capture whose audio can reach nobody is worse than none.
  if (!sink) return false;

  const channel = new MessageChannelMain();
  try {
    // Every geometry field mirrors a compiled constant, so the child validates them
    // against its own copies and a mismatch is a protocol fault rather than a resample.
    // `windowHandle` is the one REQUEST in the message — a HANDLE, never a PID (I-PID).
    const start: AudiocapStart = {
      kind: 'start',
      quantumMs: QUANTUM_MS,
      sampleRate: SAMPLE_RATE,
      channels: CHANNELS,
      frameCount: FRAME_COUNT,
      creditBound: CREDIT_BOUND,
      ringSlots: RING_SLOTS,
      windowHandle,
    };
    live.child.postMessage(start, [channel.port1]);
    sink(live.generation, channel.port2);
    return true;
  } catch {
    // Nothing caught here is logged. The throw can carry an `Error.cause` from the
    // renderer boundary (C8), and this module's own rule is that only a sanitised
    // string ever reaches a sink.
    channel.port1.close();
    channel.port2.close();
    return false;
  }
}

function armStartAck(live: HostSession): void {
  const timer = setTimeout(() => {
    if (session !== live) return;
    // Same reason as a missed hello: the child did not finish in time. The copy for
    // it already says "didn't start in time", which is exactly what happened.
    // `null`: the start never settled, so the start result IS the report (I7).
    retire(live, { ok: false, reason: 'handshake-timeout' }, 'faulted', null);
  }, START_ACK_TIMEOUT_MS);
  // `unref` for the handshake timer's reason: a pending ack must never hold the loop open at quit.
  timer.unref();
  live.startAckTimer = timer;
}

/**
 * The tap exists. ONLY legal in `starting` (§4a): a duplicate, a `started` before
 * `hello`, or one from the probe is a child saying something outside its contract.
 */
function handleStarted(live: HostSession): void {
  if (hostState !== 'starting') {
    // A duplicate `started` arrives in `capturing` and publishes (I7); one before
    // `hello` does not, because nothing had settled yet.
    retire(live, { ok: false, reason: 'protocol-fault' }, 'faulted', 'protocol-fault');
    return;
  }
  // The shared helper, not a hand-rolled clear of the one timer this arm armed: it is the
  // same call every other exit from a pending state makes, so a timer added to the session
  // later is cleared here without anyone remembering this site.
  clearTimers(live);
  hostState = 'capturing';
  // `true` by construction: `handleHello` enters `starting` only on the arm where the
  // child's validated capability was true (I5 — a necessary input, never a grant).
  settleOnce(live, { ok: true, generation: live.generation, perProcessAudio: true });
}

function notifyMachineCapability(perProcessAudio: boolean): void {
  // NOTIFY LAST, AND NEVER LET IT ABORT THE HANDSHAKE. Found by #3198's pre-PR
  // adversarial pass. The listener reaches `webContents.send`; called before
  // `settleOnce`, a throw there left the promise unsettled with `live.handshakeTimer`
  // already cleared -- the host wedged in 'handshaking' forever and never reaped its
  // child, and the throw then reached main's uncaughtException handler, which calls
  // `app.exit(1)`.
  //
  // THE MECHANISM IS NOT "A DISPOSED RENDER FRAME", WHICH IS WHAT THIS COMMENT SAID
  // UNTIL THE PHASE-8 PASS MEASURED IT. On real Electron 44.1.1, a disposed frame does
  // not throw at all: neither a crashed renderer nor a saved `webContents` handle used
  // after the window is destroyed. What throws is reading `.webContents` off a DESTROYED
  // `BrowserWindow` -- on the property access, before `.send` -- and a `?.` null check
  // does not cover that, which is why `main.ts` now guards both push sites with
  // `isDestroyed()`. The fix here was right; its stated cause was not.
  //
  // Both halves are load-bearing, and each is pinned by its own test. The ORDERING
  // guarantees the handshake completes -- proven by the re-entrant-listener case, not by
  // the throwing-listener case, which the catch alone satisfies. The CATCH guarantees a
  // send into a destroyed window cannot kill the app after it has.
  //
  // Swallowing is correct here rather than lossy: the value is monotone (a machine
  // fact, identical on every hello), and `did-finish-load` re-pushes it on the next
  // load -- so a dropped push self-heals against the very frame that could not take it.
  //
  // The drop is LOGGED, and the earlier "nothing is logged" rule over-read its own
  // justification: observability.md principle 3 constrains the ERROR OBJECT (an
  // `Error.cause` must not reach a sink), not the fact that a push was dropped. A fixed
  // string carries no cause, no PII and no privacy discriminator. Without it, a listener
  // that throws on EVERY invocation is indistinguishable from a machine with no
  // per-process backend, permanently and with no signal anywhere.
  try {
    onMachineCapabilityChange?.(perProcessAudio);
  } catch {
    // Fixed string only -- never the caught value. See above.
    console.warn('[audiocap] capability push dropped');
  }
}

/**
 * The hello arm of `handleChildMessage`, extracted for S3776 (cognitive
 * complexity 18 > 15).
 *
 * EXTRACTED, NOT FLATTENED. The arm is one ordered sequence -- clear the
 * handshake timer, publish the snapshot, decide the rung, settle (or, for a
 * capture, enter `starting` and wait for `started` -- #3394), notify -- and
 * each of its branches is a distinct refusal with its own reason. Collapsing
 * them into fewer conditionals would have traded this epic's whole product (a
 * reason string that names the mechanism that actually failed) for a metric. A
 * named function costs one call and leaves every branch intact.
 *
 * `message` arrives already narrowed by `isAudiocapHello`, which is what keeps
 * the capability read below a real boolean rather than a truthy value main would
 * then AND into facts it owns (I5).
 */
function handleHello(live: HostSession, message: AudiocapHello): void {
  // A hello is only meaningful while handshaking. A SECOND one would be a
  // child re-announcing a capability after main already decided the rung —
  // I5's failure mode arriving late rather than early. Refuse it.
  if (hostState !== 'handshaking') {
    // Publishes only when the late hello arrives in `capturing` (I7).
    retire(live, { ok: false, reason: 'protocol-fault' }, 'faulted', 'protocol-fault');
    return;
  }
  if (live.handshakeTimer) {
    clearTimeout(live.handshakeTimer);
    live.handshakeTimer = null;
  }
  live.capability = message.capability;
  // THE SNAPSHOT'S ONLY WRITER (#3198 spec §4.1). `isAudiocapHello` has already
  // refused a truthy non-boolean, so this is a real boolean — and I5 still holds:
  // a necessary input main ANDs with facts it owns, never a grant.
  machineCapability = message.capability.perProcessAudio;
  hostState = 'ready';

  // THE CAPTURE LEG (#3198 PR 3, plan Task 12a) — the one this epic never had.
  // #3195 built the host and the handshake, #3197 the backends, #3198 PR 1 the
  // ladder and PR 2 the resolver; nothing anywhere posted `{kind:'start'}`, so
  // `AudiocapStart` had no constructor and the child's `handleStart` was
  // unreachable in production. `HostState` has carried a `'capturing'` member
  // since #3195 with a comment saying the port handoff enters it. This is it --
  // though since #3394 the handoff enters `starting`, and `capturing` waits for the
  // child's `started` ack (see `handleStarted`).
  //
  // IT RUNS HERE, IN THE HELLO ARM, AND NOWHERE ELSE. This is the only point that
  // knows the child survived the handshake AND what it claims to support, which
  // are the two facts a start depends on. Starting from `startAudiocapHost` would
  // race the handshake it exists to wait for.
  //
  // THE PROBE TAKES NONE OF IT. `windowHandle === null` means "no window", so the
  // app-start capability probe settles exactly as it did before this PR — no
  // channel, no `start`, no `'capturing'`. That is what keeps P3 (the probe's
  // child is short-lived) and P4 (it runs once) untouched.
  let outcome: AudiocapStartResult = {
    ok: true,
    generation: live.generation,
    perProcessAudio: message.capability.perProcessAudio,
  };
  if (live.windowHandle !== null) {
    if (!message.capability.perProcessAudio) {
      // FAIL CLOSED, and reachable despite the renderer's ladder. `canCarryScreenAudio`
      // should have refused this share long before here, but a renderer that lies to
      // itself must not obtain a capture — I5: a necessary input main ANDs with facts
      // it owns, never a grant.
      outcome = { ok: false, reason: 'no-backend' };
    } else if (beginCapture(live, live.windowHandle)) {
      // #3394: NOT `capturing`, and NOT settled. The child has been told to start and has
      // not yet resolved its target or created its tap; every refusal from here on must
      // still reach the start promise (spec C1). `started` settles it; a fault, an exit
      // or START_ACK_TIMEOUT_MS retires it with the real reason.
      hostState = 'starting';
      armStartAck(live);
      notifyMachineCapability(message.capability.perProcessAudio);
      return;
    } else {
      // `beginCapture` returns false only when the channel or the `start` post
      // itself failed, so the child may be holding a tap nobody can hear. The
      // `retire` below is what reaps it.
      outcome = { ok: false, reason: 'protocol-fault' };
    }
  }

  // `retire` on the failure arm, not `settleOnce`: a child that was told to start
  // and whose port never reached the renderer is a child holding an OS tap nobody
  // is listening to. Reaping it is the only thing that destroys that tap.
  //
  // `null`: this arm runs in `ready`, before anything settled, so the start result
  // is the whole report (I7).
  if (outcome.ok) {
    settleOnce(live, outcome);
  } else {
    retire(live, outcome, 'faulted', null);
  }
  notifyMachineCapability(message.capability.perProcessAudio);
}

function handleChildMessage(live: HostSession, message: unknown): void {
  if (isAudiocapHello(message)) {
    handleHello(live, message);
    return;
  }

  if (isAudiocapStarted(message)) {
    handleStarted(live);
    return;
  }

  if (isAudiocapFault(message)) {
    // C8: the loader sets an `Error.cause`, so only the sanitised message may
    // reach a sink — and `sanitizeDiagnostic` takes a string, never an Error,
    // which is the mechanism by which a cause has no path here.
    const detail = sanitizeDiagnostic(message.message, FAULT_MESSAGE_MAX_CHARS);
    console.warn('[audiocap] child fault', { stage: message.stage, detail });
    if (message.stage === 'run') {
      // STAGE LEGALITY (spec §4.3). `'run'` means a capture that was live failed, so
      // it is legal only after `started`. In `handshaking` or `starting` it is the
      // child speaking outside its contract: the start settles `protocol-fault`, and
      // `retire`'s `capturing` gate keeps it unpublished.
      retire(live, { ok: false, reason: 'protocol-fault' }, 'faulted', 'capture-interrupted');
    } else {
      // A start-phase stage while `capturing` is equally out of contract, so it
      // publishes `protocol-fault`; before `started` only the start result reports it.
      retire(
        live,
        { ok: false, reason: reasonForFaultStage(message.stage) },
        'faulted',
        'protocol-fault'
      );
    }
    return;
  }

  // There is no "unknown message ignored" branch on parentPort (spec §4a): a
  // message outside the closed control set means kill the child.
  retire(live, { ok: false, reason: 'protocol-fault' }, 'faulted', 'protocol-fault');
}

/**
 * Fork the capture child and run the handshake.
 *
 * Resolves rather than rejects on every failure path: a degraded capture is a
 * product state the caller renders (spec §5 Q4), not an exception. `ok: false`
 * means video-only with a reason — NEVER a fall back to a system mix (C9).
 *
 * The fork happens in the executor's synchronous run, before any microtask, so
 * a caller may `void` this and still rely on the child existing.
 *
 * `windowHandle` IS BACK, AND IT IS REQUIRED RATHER THAN OPTIONAL. PR 2's review
 * deleted it because nothing read it; PR 3 restores it together with the hello-arm
 * leg that posts `{kind:'start', …}`, so the parameter and its consumer land in one
 * change (#3198 PR 2 review, m2).
 *
 * Required-with-explicit-`null`, NOT `windowHandle = null`. A defaulted parameter is
 * precisely how the sibling defect happened: `screenAudioRefusalMessage` gained
 * `platform: string | null = null`, the sole production call site kept passing one
 * argument, and the Linux branch was dead in production while its unit tests passed.
 * Required means the compiler names every caller that has to decide, and the probe's
 * `null` is a statement ("no window") rather than an omission.
 */
export function startAudiocapHost(
  generation: number,
  windowHandle: number | null
): Promise<AudiocapStartResult> {
  // I3. Supersede first, synchronously, with nothing awaited in between.
  killAudiocapHost();

  const addonPath = resolveNativeAddonPath(
    process.platform,
    app.isPackaged,
    process.resourcesPath,
    process.cwd()
  );

  // null is a legitimate outcome, not an error: ADR-0043 puts Linux/PipeWire out
  // of scope, so there is no addon to host there — and no child to fork.
  if (!addonPath) {
    hostState = 'faulted';
    return Promise.resolve({ ok: false, reason: 'unsupported-os' });
  }

  return new Promise<AudiocapStartResult>((resolve) => {
    hostState = 'spawning';
    const child = utilityProcess.fork(path.join(__dirname, 'audiocapChild.js'), [], {
      env: buildChildEnv(addonPath),
      // 'inherit' (the default) would put the child's stdout and stderr straight
      // onto main's, where a loader failure carrying an `Error.cause` becomes a
      // log line nobody sanitised (C8). Piping captures them; draining without a
      // 'data' listener discards them and keeps the child from blocking on a
      // full pipe. The child's diagnostics reach us as `fault` messages instead,
      // which are charset-restricted and length-capped by the protocol.
      stdio: 'pipe',
    });
    // C8, AND ITS ONE DEV-ONLY EXCEPTION.
    //
    // Piping and draining without a 'data' listener discards the child's stdout
    // and stderr, which is what stops a loader failure carrying an
    // `Error.cause` from becoming a log line nobody sanitised. That is a
    // PRODUCTION concern, and it had a cost nobody priced: it also discarded the
    // consent/liveness line design §6.2 says a developer must be able to read,
    // so `signalTotal` and `silentSinceStart` were computed on every callback
    // and reached no human at all. R9 measured state B precisely so that a
    // developer would stop inferring denial from silence; with the child's only
    // voice muted, they still had to.
    //
    // In a packaged build nothing changes. Unpackaged, the child's stderr is
    // echoed — it is the developer's own machine, the addon is their own build,
    // and a raw `Error.cause` in a dev console is the thing they are debugging.
    child.stdout?.resume();
    if (app.isPackaged) {
      child.stderr?.resume();
    } else {
      // BOUNDED, because an unbounded echo is a new failure mode rather than a
      // diagnostic: a native crash loop or verbose logging left on by mistake
      // would flood a dev console and bury the one line this exists to show.
      // Same posture as the rest of this surface — PCM_PROTOCOL_FAULT_LIMIT in
      // audiocapChild.ts caps repeats, FAULT_MESSAGE_MAX_CHARS caps length.
      //
      // Dropping the tail rather than the head is deliberate: the liveness line
      // is written at teardown, so a run that hits the cap has already gone
      // wrong in a way the first chunks describe better than the last.
      let echoed = 0;
      child.stderr?.on('data', (chunk: Buffer) => {
        if (echoed >= CHILD_STDERR_ECHO_LIMIT) return;
        const text = chunk.toString('utf8').trimEnd();
        if (text.length === 0) return;
        echoed += 1;
        console.debug('[audiocap:child]', text.slice(0, CHILD_STDERR_ECHO_MAX_CHARS));
        if (echoed === CHILD_STDERR_ECHO_LIMIT) {
          console.debug('[audiocap:child] …further output suppressed (echo cap reached)');
        }
      });
    }

    const live: HostSession = {
      generation,
      child,
      windowHandle,
      settle: resolve,
      handshakeTimer: null,
      startAckTimer: null,
      capability: null,
    };
    session = live;
    hostState = 'handshaking';

    const handshakeTimer = setTimeout(() => {
      if (session !== live) return;
      retire(live, { ok: false, reason: 'handshake-timeout' }, 'faulted', null);
    }, HANDSHAKE_TIMEOUT_MS);
    handshakeTimer.unref();
    live.handshakeTimer = handshakeTimer;

    child.on('message', (message: unknown) => {
      if (session !== live) return;
      handleChildMessage(live, message);
    });

    // A child that died without a word is a PACKAGING DEFECT surfacing, and it
    // must read as one rather than as an absent capability. I6: no respawn.
    // Mid-share, the same death is published as `child-crash` (I7).
    child.on('exit', () => {
      if (session !== live) return;
      retire(live, { ok: false, reason: 'child-crash' }, 'faulted', 'child-crash');
    });

    // 'error' carries a Node diagnostic report; none of its arguments is read,
    // let alone logged (C8).
    child.on('error', () => {
      if (session !== live) return;
      retire(live, { ok: false, reason: 'child-crash' }, 'faulted', 'child-crash');
    });
  });
}

// ---------------------------------------------------------------------------
// THE APP-START CAPABILITY PROBE
// ---------------------------------------------------------------------------

/// Bounds on the dev-only child-stderr echo. Neither exists in a packaged build,
/// where nothing listens at all.
///
/// The cap is not paranoia about volume: it is that an unbounded echo turns a
/// native crash loop into a flooded console, burying the single liveness line
/// this channel was opened to carry. Shaped after the rest of this surface --
/// `PCM_PROTOCOL_FAULT_LIMIT` (audiocapChild.ts) caps repeats and
/// `FAULT_MESSAGE_MAX_CHARS` (audiocapProtocol.ts) caps length.
const CHILD_STDERR_ECHO_LIMIT = 200;
const CHILD_STDERR_ECHO_MAX_CHARS = 4096;

/**
 * The generation the probe forks under.
 *
 * It only has to be a value no live session is using, and at app start every
 * value qualifies: the probe runs before any share exists and kills its child on
 * the same turn the handshake settles. #3198's share path mints its own
 * generations, and a collision with this one is harmless because I2 fences on
 * session IDENTITY rather than on the number.
 */
const PROBE_GENERATION = 1;

/**
 * What one launch learned about this machine.
 *
 * `null` means the probe has not settled yet, and a reader must treat that
 * exactly as it treats `ok: false` — no per-process audio. C9: neither is ever a
 * reason to widen capture to a system mix.
 */
export type AudiocapProbeResult =
  { ok: true; perProcessAudio: boolean } | { ok: false; reason: ScreenAudioDegradeReason };

let probeResult: AudiocapProbeResult | null = null;
let probeInFlight: Promise<AudiocapProbeResult> | null = null;

/**
 * THE MACHINE-CAPABILITY SNAPSHOT (#3198 spec §4.1) — one bit, pushed to the renderer.
 *
 * `null` means "no child has completed a handshake yet" and the renderer reads it as the
 * PRE-ADDON rungs. Absence IS the fail-closed state (C9); there is no failure mode to
 * model here, because there is nothing a missing answer could widen.
 *
 * WRITTEN ONLY FROM A VALIDATED `hello`, and deliberately NOT from `runCapabilityProbe`.
 * The probe records `{ok:false,'child-crash'}` when a share reaps its child mid-handshake
 * — for a machine that is fully capable — and `probeInFlight ??=` guarantees that value
 * is never recomputed. Routing the snapshot through the probe would publish that false
 * negative to the renderer and keep it for the life of the process. This way a reaped
 * probe leaves `null`, and the very share that reaped it sets the snapshot from its own
 * `hello`. Self-healing in one share, with no second child and no re-probe path, so I1
 * (one session), I3 (supersede-and-kill) and I4 are untouched.
 *
 * MONOTONE IN INFORMATION: a machine fact, identical on every `hello`, so a duplicate
 * push is idempotent and ordering-insensitive.
 */
let machineCapability: boolean | null = null;

/** The machine's per-process claim, or `null` while no handshake has completed. */
export function audiocapMachineCapability(): boolean | null {
  return machineCapability;
}

/**
 * Notified whenever the snapshot changes, so main can push it to the renderer.
 *
 * A callback rather than a direct `webContents.send` because this module owns no window
 * handle and must not acquire one: `main.ts` owns window lifecycle, and importing it here
 * would invert the dependency and make this module untestable in a `node` environment.
 */
let onMachineCapabilityChange: ((perProcessAudio: boolean) => void) | null = null;

export function setAudiocapCapabilityListener(
  listener: ((perProcessAudio: boolean) => void) | null
): void {
  onMachineCapabilityChange = listener;
}

/**
 * Notified when a LIVE capture ends involuntarily, so main can push
 * `audiocap:interrupted` (#3394 PR 2). A callback for the reason
 * `onMachineCapabilityChange` is one: this module owns no window.
 *
 * Its only caller is `retire()`, under I7. The object it receives is minted there
 * from two main-owned values; a listener that forwards it should still copy the two
 * fields rather than pass the reference on.
 */
let onInterrupted: ((interrupt: AudiocapInterrupted) => void) | null = null;

export function setAudiocapInterruptListener(
  listener: ((interrupt: AudiocapInterrupted) => void) | null
): void {
  onInterrupted = listener;
}

/**
 * Hands the renderer end of a capture channel to whoever owns the window (#3198 PR 3).
 *
 * A callback for exactly the reason `onMachineCapabilityChange` is one, and the
 * argument is stronger here: this sink must reach `webContents.postMessage` with a
 * TRANSFER LIST, so it needs a live `WebContents`. Acquiring one in this module would
 * invert the dependency `main.ts` owns and take the whole file out of the `node` test
 * environment its suite runs in.
 *
 * The sink returns nothing and is allowed to throw. A throw means the port did not
 * reach the renderer, which is NOT the monotone, self-healing case the capability push
 * is — the child is already capturing and nobody is listening — so the hello arm treats
 * it as a failed start and retires the session rather than swallowing it.
 */
let onCapturePort: ((generation: number, port: Electron.MessagePortMain) => void) | null = null;

export function setAudiocapPortSink(
  sink: ((generation: number, port: Electron.MessagePortMain) => void) | null
): void {
  onCapturePort = sink;
}

/** The recorded answer, or `null` while the probe is still in flight. */
export function audiocapProbeResult(): AudiocapProbeResult | null {
  return probeResult;
}

async function runCapabilityProbe(): Promise<AudiocapProbeResult> {
  let result: AudiocapProbeResult;
  // The probe's OWN session, read synchronously after the call (the executor assigns
  // `session` before returning). `null` means the probe created no child to reap.
  let probeOwn: HostSession | null = null;
  try {
    // `null`, and it is a statement rather than an omission: the probe asks whether
    // this MACHINE has a per-process backend, so it has no window and must never
    // reach the hello arm's `start` post. See the parameter's docblock.
    const pendingProbe = startAudiocapHost(PROBE_GENERATION, null);
    probeOwn = session;
    const started = await pendingProbe;
    result = started.ok
      ? { ok: true, perProcessAudio: started.perProcessAudio }
      : { ok: false, reason: started.reason };
  } catch {
    // `startAudiocapHost` RESOLVES on every failure it models, so arriving here
    // means main could not issue the fork at all. The child never existed, which
    // is what `child-crash` names; inventing a member for a caller-side fault
    // would put a non-mechanism into user-facing copy (I3's reasoning). The
    // error itself is neither read nor logged — C8 gives it no path to a sink.
    result = { ok: false, reason: 'child-crash' };
  } finally {
    // P3. A validated `hello` deliberately does NOT kill the child — that is the
    // share path's contract — so the probe reaps its own. ONLY its own (I2): a share
    // that superseded the probe mid-handshake is live here, and a bare kill would
    // end it as `child-crash` (red-team VULN-C).
    if (probeOwn !== null && session === probeOwn) killAudiocapHost();
  }
  probeResult = result;
  // Mechanism strings and a boolean: no key material, no PII, no raw error, and
  // no cause dimension on a counter (C8).
  console.debug('[audiocap] capability probe settled', result);
  return result;
}

/**
 * THE APP-START CAPABILITY PROBE — the production caller (spec §0).
 *
 * Fork the child once, take its handshake, record what it claims, kill it.
 *
 * WHY IT EXISTS, STATED BEFORE THE CODE. Everything above is mechanism, and
 * mechanism with no caller on a path a user actually takes is unreachable code
 * that only a test drives — the condition three reviewers flagged on #3194 and
 * the one §0 promises this PR does not ship. This is that caller. It also
 * answers, once per launch and off the critical path, the question #3198's rung
 * will ask: does this machine's addon claim a per-process backend at all?
 *
 * FOUR PROPERTIES IT MUST HOLD, and what breaks each:
 *
 * P1. IT NEVER DELAYS APP START. Nothing awaits it; `main.ts` schedules it off
 *     the ready path. WHAT BREAKS IT: awaiting this inside `whenReady`, where a
 *     child that never says `hello` costs HANDSHAKE_TIMEOUT_MS of startup and a
 *     child that hangs costs all of it.
 *
 * P2. IT FAILS CLOSED AND SILENT. Every outcome — unsupported OS, dead child,
 *     hostile capability payload, a fork that could not be issued — becomes a
 *     recorded mechanism. It never rejects, never opens a dialog, and never
 *     widens anything: a `false` or absent answer is video-only (C9).
 *
 * P3. THE CHILD IS SHORT-LIVED BY CONSTRUCTION. Killed on the same turn the
 *     handshake settles, whatever the outcome.
 *
 * P4. IT RUNS ONCE. `startAudiocapHost` supersedes — it kills any live child
 *     first (I3) — so a second probe, once #3198's shares exist, would reap a
 *     capture child mid-share. The memo makes "once" structural rather than a
 *     convention `main.ts` is trusted to keep.
 *
 * The answer deliberately stays in main. #3198 adds the renderer-facing rung and
 * the channel that carries it; this PR has no renderer→main audiocap control
 * channel to put it on, and adding one for a value nothing reads yet would be
 * the same defect in the other direction.
 */
export function probeAudiocapCapability(): Promise<AudiocapProbeResult> {
  probeInFlight ??= runCapabilityProbe();
  return probeInFlight;
}
