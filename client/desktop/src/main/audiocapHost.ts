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
// I4. THE PROMISE SETTLES EXACTLY ONCE, AND THE FIRST OUTCOME WINS. A child that
//     says `hello` and then exits reported a capability; collapsing that into
//     "child died" would turn a real answer into a spurious packaging defect.
//     Inherited from `audiocapSmoke.ts`'s `settled` latch and still locked by
//     its migrated vacuity control.
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

import path from 'node:path';
import { app, utilityProcess, type UtilityProcess } from 'electron';
import {
  FAULT_MESSAGE_MAX_CHARS,
  HANDSHAKE_TIMEOUT_MS,
  isAudiocapFault,
  isAudiocapHello,
  sanitizeDiagnostic,
  type AudiocapCapability,
  type AudiocapFaultStage,
} from '../shared/audiocapProtocol';
import { NATIVE_ADDON_ENV, resolveNativeAddonPath } from './nativeAddonPath';

/**
 * Why the local share has no audio. A closed enum of MECHANISM strings — never a
 * cause dimension on a counter, and never a privacy-decision discriminator (C8
 * principle 7).
 *
 * Six of the seven describe the capture child. `produce-rejected` is the one that
 * does not: the capture succeeded and the SFU refused to publish it — today only
 * `PARTICIPANT_PRODUCER_LIMITS['screen-audio'] === 1` can do that. It is still a
 * mechanism rather than a caller bug, which is the test spec §5 Q4 sets for
 * membership, and the share it describes is video-only exactly like the others.
 */
export type ScreenAudioDegradeReason =
  | 'no-backend'
  | 'handshake-timeout'
  | 'child-crash'
  | 'load-fault'
  | 'capability-fault'
  | 'protocol-fault'
  | 'produce-rejected'
  // The tap is alive and delivering, and nothing has ever been audible.
  //
  // ADVISORY, AND DELIBERATELY NOT `consent-denied`. R9 measured that a
  // TCC-denied Core Audio tap returns noErr from every call and delivers ~94
  // correctly-shaped callbacks a second carrying only zeros -- byte-identical
  // to a granted tap on a paused app. Nothing at the capture seam distinguishes
  // them, so a `consent-denied` member would be a mechanism string the
  // mechanism cannot detect, which is the same defect as inventing a reason to
  // describe a caller bug. See the #3197 PR 2 spec amendment, section 9.0.
  //
  // DECLARED AND UNREACHABLE IN THIS PR, stated here for the same reason
  // `kPermissionDenied` and `ThreadStartFailed` state it at their own
  // declarations: nothing produces it. The route the design names is
  // `status().faulted` -> `fault{stage:'run'}` -> here, and neither leg exists
  // yet -- `QuantumPump::fault()` has no production caller and
  // `AudiocapFaultStage` has no `'run'` member. #3198 owns both, alongside the
  // watchdog that reads the counters this reason would be derived from. A
  // reader who assumed a live path would go looking for a producer that is not
  // there.
  | 'capture-starved'
  | 'unsupported-os';

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
 * A MEMBERSHIP SET, NOT A PHRASE MAP. #3197 PR 2 ships no user-facing copy for
 * these — the renderer has no consumer for the union yet, and adding one would
 * be new UI in a PR that ships dark. #3198 owns the copy alongside the ladder
 * rung that makes these states reachable.
 */
export const SCREEN_AUDIO_DEGRADE_REASONS: Readonly<Record<ScreenAudioDegradeReason, true>> = {
  'no-backend': true,
  'handshake-timeout': true,
  'child-crash': true,
  'load-fault': true,
  'capability-fault': true,
  'protocol-fault': true,
  'produce-rejected': true,
  'capture-starved': true,
  'unsupported-os': true,
};

export type AudiocapStartResult =
  | {
      ok: true;
      generation: number;
      /**
       * The child's CLAIM, already narrowed by `isAudiocapHello` — a truthy `1`
       * was refused before it got here. I5: a necessary input main ANDs with
       * facts it owns, never a grant. This PR's ladder has no `'per-process'`
       * rung for it to unlock; #3198 is where it becomes one input among
       * several.
       */
      perProcessAudio: boolean;
    }
  | { ok: false; reason: ScreenAudioDegradeReason };

/** Spec §6b. `capturing` is entered by the port handoff, which lands in Task 5/7. */
type HostState =
  'idle' | 'spawning' | 'handshaking' | 'ready' | 'capturing' | 'stopping' | 'faulted';

type ChildProcess = ReturnType<typeof utilityProcess.fork>;

interface HostSession {
  readonly generation: number;
  readonly child: ChildProcess;
  /** Resolves `startAudiocapHost`. Cleared by the first outcome (I4). */
  settle: ((result: AudiocapStartResult) => void) | null;
  handshakeTimer: NodeJS.Timeout | null;
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
 * CAPTURING child. This PR has none: the only child it forks is the app-start
 * probe below, which main kills on the same turn the handshake settles. A timer
 * guarding nothing is the same "shipped code with no caller" defect the probe
 * exists to close, wearing a different costume — and §6c is explicit that an
 * unwired watchdog is UNFINISHED, not defence. #3198 reintroduces
 * `armWatchdog` / `noteAudiocapCreditAck` / `setAudiocapShareLive` alongside the
 * capturing child that gives them meaning. Do not re-add it before then.
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

/** A `fault` names the stage that failed; main maps it to a degrade mechanism. */
function reasonForFaultStage(stage: AudiocapFaultStage): ScreenAudioDegradeReason {
  switch (stage) {
    case 'guard':
    case 'load':
      return 'load-fault';
    case 'capability':
      return 'capability-fault';
    case 'start':
      return 'no-backend';
    case 'protocol':
      return 'protocol-fault';
  }
}

function clearTimers(live: HostSession): void {
  if (live.handshakeTimer) {
    clearTimeout(live.handshakeTimer);
    live.handshakeTimer = null;
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
 * Terminal for THIS session: stop its timers, forget it, kill its child.
 *
 * Killing after forgetting is what makes the kill safe to issue from anywhere —
 * the `exit` that follows finds `session !== live` and does nothing.
 */
function retire(live: HostSession, result: AudiocapStartResult, nextState: HostState): void {
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
 * NOT REACHABLE IN #3195 -- `startAudiocapHost` has exactly one caller and it is
 * memoized to run once. It arms with #3198's share path, which is why it is fixed
 * here rather than left for that PR to inherit.
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

function handleChildMessage(live: HostSession, message: unknown): void {
  if (isAudiocapHello(message)) {
    // A hello is only meaningful while handshaking. A SECOND one would be a
    // child re-announcing a capability after main already decided the rung —
    // I5's failure mode arriving late rather than early. Refuse it.
    if (hostState !== 'handshaking') {
      retire(live, { ok: false, reason: 'protocol-fault' }, 'faulted');
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
    settleOnce(live, {
      ok: true,
      generation: live.generation,
      perProcessAudio: message.capability.perProcessAudio,
    });
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
      onMachineCapabilityChange?.(message.capability.perProcessAudio);
    } catch {
      // Fixed string only -- never the caught value. See above.
      console.warn('[audiocap] capability push dropped');
    }
    return;
  }

  if (isAudiocapFault(message)) {
    // C8: the loader sets an `Error.cause`, so only the sanitised message may
    // reach a sink — and `sanitizeDiagnostic` takes a string, never an Error,
    // which is the mechanism by which a cause has no path here.
    const detail = sanitizeDiagnostic(message.message, FAULT_MESSAGE_MAX_CHARS);
    console.warn('[audiocap] child fault', { stage: message.stage, detail });
    retire(live, { ok: false, reason: reasonForFaultStage(message.stage) }, 'faulted');
    return;
  }

  // There is no "unknown message ignored" branch on parentPort (spec §4a): a
  // message outside the closed control set means kill the child.
  retire(live, { ok: false, reason: 'protocol-fault' }, 'faulted');
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
 */
export function startAudiocapHost(generation: number): Promise<AudiocapStartResult> {
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
      settle: resolve,
      handshakeTimer: null,
      capability: null,
    };
    session = live;
    hostState = 'handshaking';

    const handshakeTimer = setTimeout(() => {
      if (session !== live) return;
      retire(live, { ok: false, reason: 'handshake-timeout' }, 'faulted');
    }, HANDSHAKE_TIMEOUT_MS);
    handshakeTimer.unref();
    live.handshakeTimer = handshakeTimer;

    child.on('message', (message: unknown) => {
      if (session !== live) return;
      handleChildMessage(live, message);
    });

    // A child that died without a word is a PACKAGING DEFECT surfacing, and it
    // must read as one rather than as an absent capability. I6: no respawn.
    child.on('exit', () => {
      if (session !== live) return;
      retire(live, { ok: false, reason: 'child-crash' }, 'faulted');
    });

    // 'error' carries a Node diagnostic report; none of its arguments is read,
    // let alone logged (C8).
    child.on('error', () => {
      if (session !== live) return;
      retire(live, { ok: false, reason: 'child-crash' }, 'faulted');
    });
  });
}

// ---------------------------------------------------------------------------
// THE APP-START CAPABILITY PROBE
// ---------------------------------------------------------------------------

/**
 * The generation the probe forks under.
 *
 * It only has to be a value no live session is using, and at app start every
 * value qualifies: the probe runs before any share exists and kills its child on
 * the same turn the handshake settles. #3198's share path mints its own
 * generations, and a collision with this one is harmless because I2 fences on
 * session IDENTITY rather than on the number.
 */
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

/** The recorded answer, or `null` while the probe is still in flight. */
export function audiocapProbeResult(): AudiocapProbeResult | null {
  return probeResult;
}

async function runCapabilityProbe(): Promise<AudiocapProbeResult> {
  let result: AudiocapProbeResult;
  try {
    const started = await startAudiocapHost(PROBE_GENERATION);
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
    // P3, unconditional. A validated `hello` deliberately does NOT kill the
    // child — that is the share path's contract — and this PR has no share path
    // to hand it to, so the probe reaps its own.
    killAudiocapHost();
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
