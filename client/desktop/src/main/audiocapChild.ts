// concord-audiocap utilityProcess CHILD — runs INSIDE a utilityProcess (#3195, ADR-0043).
//
// Loads the native addon, probes `capability()`, announces itself to main, and
// then — once main says `start` and hands over one end of a `MessageChannelMain`
// — pumps 3872-byte PCM quanta onto that port under a credit bound. It never
// exits itself on an ordinary stop: main owns the kill (spec §4a).
//
// GROWN FROM `audiocapSmokeChild.ts` (#3194) RATHER THAN REPLACING IT. That
// file's own header promised the production child would "grow from this seed";
// this is that growth, and the seed is deleted with this change. Two things
// carried across unchanged in intent — the ADR-0043 D5 guard and the
// `createRequire(__filename)` loader — and its three process-boundary tests came
// with them into `tests/unit/main/audiocapChild.test.ts`.
//
// WHY THIS FILE LIVES UNDER src/main/ DESPITE RUNNING ELSEWHERE. There is no
// src/utility/ build path: `tsconfig.main.json` includes only `src/main/**` and
// `src/shared/**`, and `npm run build` has three legs (renderer, preload, main).
// `utilityProcess.fork()` takes a path to a built JS file and does not care which
// tsconfig produced it, so compiling through the existing main leg costs nothing,
// where a fourth build target for one file would be the wrong trade.
//
// `native-audio.md:89` constrains where the addon RUNS, not where its source
// file LIVES, and the guard below is what makes that true rather than claimed.
//
// ---------------------------------------------------------------------------
// THE INVARIANTS, STATED BEFORE THE CODE
// ---------------------------------------------------------------------------
//
// C1. THE D5 GUARD IS A HARD THROW AND IT RUNS FIRST. A process that is not a
//     utilityProcess never reaches the loader and never gets to speak.
//
//     WHAT BREAKS IT: "improving" the guard into a posted `fault{stage:'guard'}`
//     so main gets a nicer reason. Reporting is a capability, and the only
//     process that could exercise it here is one that has already proved it is
//     not the process we meant to grant it to — it would be main or the
//     renderer, holding SSO tokens and the update path, having just decided it
//     may load rt/ after all. The `'guard'` member of `AudiocapFaultStage` exists
//     so main can CLASSIFY that stage, never so this file can SEND it; nothing
//     in this file posts it, and nothing should.
//
// C2. THE CHILD NEVER DIES SILENTLY WHERE IT COULD SPEAK. Six stages are
//     reported rather than fallen over: a loader throw is `fault{stage:'load'}`,
//     a `capability()` throw or unusable return is `fault{stage:'capability'}`, a
//     window handle that resolves to no live owner (or a `NoTarget` refusal) is
//     `fault{stage:'target'}` (#3198), a refused or failed `start` is
//     `fault{stage:'start'}`, a second PCM-port protocol violation is
//     `fault{stage:'protocol'}`, and a pump fault that latched AFTER `started` is
//     `fault{stage:'run'}` (#3394 PR 2, the fault watch). That is what gives the
//     parent's `exit` handler a single meaning — the child died without a word,
//     which is the packaging defect it should mean (`audiocapHost.ts` I6) — and
//     it is inherited item (d) from #3194's review.
//
// C3. `Error.cause` NEVER REACHES THE WIRE. The loader sets one
//     (`native/concord-audiocap/index.js:166`) carrying the underlying resolution
//     failure. Every outbound message is built here, and every diagnostic string
//     in one goes through `sanitizeDiagnostic`, whose signature takes a `string`
//     and not an `Error` — that signature IS the mechanism, so a cause has no
//     path to a sink (observability.md principle 3, C8). `errorMessage()` is the
//     only place an `unknown` catch value is read, and it reads `.message` alone.
//
// C4. THE PRODUCER IS THE CLOCK. The drain loop runs only while
//     `outstanding < CREDIT_BOUND`. There is no timer, and there is no
//     batch-to-catch-up. When the loop stops, `rt/` fills the ring and drops the
//     newest — credit exhaustion and ring overflow are the same event with one
//     drop site, counted once (spec §4d, ADR-0043 D4c).
//
//     Draining resumes on exactly two events, and the second is not the consumer
//     asking for more: the addon's availability signal, and a credit ack freeing
//     a slot. The signal rides a threadsafe function with `max_queue_size = 1`
//     called non-blocking, so a signal arriving while we are credit-blocked is
//     DROPPED. Without the ack-driven resume a blocked child would therefore
//     never restart — it would sit on a full ring holding a signal that was
//     coalesced away. The ack finishes work we already knew was there.
//
//     WHAT BREAKS IT: a `setInterval` "to keep audio flowing", or draining a
//     fixed batch per signal. Either turns the consumer into the clock and moves
//     the drop site off the ring, where nothing counts it.
//
// C5. A CREDIT CANNOT RAISE THE BOUND. `outstanding` is floored at zero, so a
//     peer that posts credits it never earned cannot drive it negative and buy
//     itself more than `CREDIT_BOUND` buffers in flight. This is the same shape
//     as the pool-inflation defence spec §4e describes for the (unbuilt) free-list
//     fallback, on the one path that does exist.
//
// C6. THE PCM PORT CARRIES EXACTLY ONE INBOUND SHAPE, AND THE SECOND VIOLATION
//     IS TERMINAL. `{c: 1}` and nothing else. The first violation is counted and
//     tolerated — a single stray message is a plausible teardown race with the
//     preload relay. The second is a peer that is not speaking this protocol:
//     `fault{stage:'protocol'}` to main, port closed, no reply on the port
//     itself (spec §4b: "close the port immediately. Do not reply.").
//
// C7. `parentPort` CARRIES A CLOSED CONTROL SET WITH NO IGNORE BRANCH. `start`
//     and `stop`. Anything else exits the process (spec §4a). There is
//     deliberately no `fault` posted first: main authored the message, so it
//     needs no report from us to know what it sent — and a post issued
//     immediately before `process.exit` races its own delivery, which would make
//     the reported stage arrive sometimes and not others. The exit IS the
//     response, and main reads it as `child-crash`.
//
// C8. THE QUANTUM IS POSTED BY VALUE, NOT WITH A TRANSFER LIST — on THIS hop.
//     `electron.d.ts` types `MessagePortMain.postMessage(message, transfer?)`
//     with `transfer?: MessagePortMain[]`, so an `ArrayBuffer` in that list does
//     not type-check and there is no evidence Electron accepts one. Spec §4e's
//     transfer list is measured (Task 0 spike T0a) on the preload→main-world hop,
//     which is a DOM `MessagePort`; the child→preload hop was never measured.
//     A fresh buffer per quantum is what §4e already sanctions for this side
//     ("the child allocates — the child is a Node process off the real-time
//     thread"), and it is the fail-closed choice: posting by value costs one
//     3872-byte copy per 10 ms, where a transfer list Electron rejects would
//     throw on every quantum and lose the feature outright.

import { createRequire } from 'node:module';
import {
  AUDIOCAP_PROTOCOL,
  CHANNELS,
  CREDIT_BOUND,
  DIAGNOSTIC_MAX_CHARS,
  FAULT_MESSAGE_MAX_CHARS,
  FRAME_COUNT,
  QUANTUM_BYTES,
  QUANTUM_MS,
  RING_SLOTS,
  SAMPLE_RATE,
  sanitizeDiagnostic,
  type AudiocapCapability,
  type AudiocapFault,
  type AudiocapFaultStage,
  type AudiocapHello,
  type AudiocapStarted,
} from '../shared/audiocapProtocol';

// C1. ADR-0043 D5. A memory-safety bug in rt/ must not own the process holding
// SSO tokens and the update path, so the addon may load ONLY in a utilityProcess.
//
// This is a GUARD, not a comment. Electron types process.type as
// 'browser' | 'renderer' | 'service-worker' | 'worker' | 'utility'
// (electron.d.ts), and a plain Node process has no `type` at all — so anything
// that is not a utility child fails here, loudly, on the first wrong call,
// instead of silently widening the trust boundary D5 exists to keep narrow.
if (process.type !== 'utility') {
  throw new Error(
    `audiocapChild must run in a utilityProcess (ADR-0043 D5); process.type was ` +
      `"${String(process.type)}". Loading the native addon in main or the renderer would put ` +
      `a memory-safety bug in the process that holds SSO tokens and the update path.`
  );
}

// ---------------------------------------------------------------------------
// Local shapes
// ---------------------------------------------------------------------------

/**
 * The addon surface this file uses. `native/concord-audiocap/index.d.ts` is the
 * authority; this is a structural restatement rather than an import because
 * `tsconfig.main.json` compiles `src/**` only, and the seed took the same shape.
 *
 * `capability`, `start` and `drain` return `unknown` on purpose. The `.d.ts` is a
 * claim ABOUT a native binary, not a guarantee FROM one, so every return value is
 * narrowed here before it decides anything.
 */
interface AudiocapAddon {
  capability: () => unknown;
  start: (options: AddonStartOptions, onQuantumAvailable: () => void) => unknown;
  drain: (into: ArrayBuffer) => unknown;
  stop: () => void;
  /**
   * The post-mortem read, declared here because the addon HAS it — the binary's
   * five exports are `capability`, `start`, `drain`, `stop` and `status`, and an
   * interface that omitted one made a test mock the more complete description of
   * the addon than the production type was.
   *
   * `unknown` for the reason the other three are: the `.d.ts` is a claim about a
   * native binary, not a guarantee from one.
   *
   * TWO READERS, and they read different things. `reportLiveness` is the
   * post-mortem read on `handleStop`: the R9 liveness counters and the teardown
   * evidence (`quiesceProved`, `destroyFailures`, `lastDeviceStatus`). The fault
   * watch (#3394 PR 2) polls it while a capture is live and reads `faulted` and
   * `faultReason` ONLY — nothing else it returns ever reaches a posted message.
   * `status().poisoned` has no reader yet; it is the signal the still-unbuilt
   * watchdog rail would read to decide the child must be killed rather than
   * asked to stop again.
   */
  status: () => unknown;
  /**
   * The sixth export (#3198). THE ONLY PROCESS THAT EVER LEARNS A PID IS THIS
   * ONE (ADR-0043 D5, invariant I-PID): main holds the handle, the child
   * resolves it, and the result is never posted, logged or counted.
   *
   * Takes `number` rather than `unknown` even though the message field is
   * untrusted — narrowing belongs at the call site, where a non-number is one of
   * the refusals `'target'` already names, not in a signature that would then
   * misdescribe the native contract.
   */
  resolveWindowOwner: (handle: number) => unknown;
}

/**
 * Every field is spelled `typeof <protocol constant>` rather than a repeated
 * literal, so the geometry the addon is handed cannot drift from the geometry
 * `audiocapProtocol.ts` publishes. The addon compares each one for exact
 * equality against the constants compiled into `rt/quantum_header.h`.
 */
interface AddonStartOptions {
  quantumMs: typeof QUANTUM_MS;
  sampleRate: typeof SAMPLE_RATE;
  channels: typeof CHANNELS;
  frameCount: typeof FRAME_COUNT;
  ringSlots: typeof RING_SLOTS;
  /**
   * REQUIRED HERE, though `index.d.ts` declares it optional — this child never
   * starts a capture without a target, so "forgot the target" is a compile error
   * rather than a start the addon refuses with `NoTarget` (or, on a future
   * backend, one that captures more than it was asked to).
   */
  targetPids: number[];
  /**
   * THE REQUIRED LITERAL `true`, not `boolean` and not optional (#3394 PR 2,
   * design spec §3 and §4.1; ADR-0043 § As-built addendum — #3394 PR 2). A
   * window's audio is usually rendered by a helper process the window's owner
   * launched, not by the owner itself, so an owner-only tap is a share that goes
   * live SILENT with no error. The target is therefore the owner's process tree:
   * this child passes exactly ONE root PID, and the addon expands it natively —
   * so no expanded PID ever exists outside the addon (I-PID) — and refuses a
   * second root with `BadOptions`.
   *
   * The literal type is the mechanism: a forgotten flag is a compile error rather
   * than a silent owner-only tap, and `false` cannot be written here at all. What
   * the tree contains — descendants only, never the host's own subtree, a cap that
   * refuses rather than subsets, and no launchd-parented XPC helper, which is not a
   * descendant — is native policy in `rt/process_tree.h`, not a decision this file
   * makes.
   *
   * This REVERSES the #3198 PR 2 position that the option stay deliberately
   * absent pending "its own decision with its own review" — spec §3 is that
   * decision.
   */
  allowDescendants: true;
}

/**
 * The PCM port, structurally.
 *
 * TWO RUNTIME SHAPES REACH THIS, AND THAT IS NOT DEFENSIVENESS. In production it
 * is an Electron `MessagePortMain`, which the Electron docs are explicit about:
 * it "uses the Node.js EventEmitter event system, instead of the DOM EventTarget
 * system", so the listener is `port.on('message', …)`. A unit test cannot
 * construct one, so the harness supplies a DOM `MessagePort`, whose listener is
 * `addEventListener` plus an explicit `start()`. Both are handled because the
 * test could not otherwise reach this code at all.
 */
interface PcmPortMessage {
  data: unknown;
}

interface PcmPort {
  postMessage(message: unknown): void;
  close(): void;
  start?(): void;
  on?(event: 'message', listener: (event: PcmPortMessage) => void): unknown;
  addEventListener?(type: 'message', listener: (event: PcmPortMessage) => void): void;
}

/**
 * `AudioCapStartFailure`, restated — `native/concord-audiocap/index.d.ts` is the
 * authority, and `tsconfig.main.json` compiles `src/**` only, so this cannot be
 * an import. `audiocapChild.failureReasons.test.ts` is what keeps the two from
 * drifting.
 */
type AudioCapStartFailure =
  | 'NoBackend'
  | 'BadOptions'
  | 'BadArguments'
  | 'AlreadyStarted'
  | 'ThreadStartFailed'
  | 'NoTarget'
  | 'PermissionDenied'
  | 'UnsupportedFormat'
  | 'DeviceError'
  | 'Poisoned';

/**
 * Every member of that closed union, mapped to the phrase this build is willing
 * to put in an outbound fault. Closed on purpose — an unrecognised reason is
 * reported generically rather than echoed, because at that point the string is an
 * unvalidated value from a native binary.
 *
 * A `Record` keyed by the union, not an array of names, and the type is the
 * mechanism: a member added to `AudioCapStartFailure` without a phrase here is a
 * COMPILE error, where a missing entry in a `string[]` was only ever a runtime
 * "no recognised reason" — a real outcome degraded into a generic one at the
 * moment a user's share lost its audio.
 *
 * THE WHOLE UNION SHIPS AT ONCE, including members only a real backend can
 * produce (`PermissionDenied` needs a tap to refuse; `NoTarget`, `DeviceError`
 * and `UnsupportedFormat` need one to exist). Declaring them when the vocabulary
 * is designed is what keeps this map from growing a hole between two PRs.
 *
 * The phrases name a MECHANISM and carry nothing from the OS: no error string, no
 * path, no PID (observability.md principles 3 and 7). Main still derives the
 * user-facing `ScreenAudioDegradeReason` from the fault STAGE, never from this
 * text.
 */
export const START_FAILURE_REASONS: Readonly<Record<AudioCapStartFailure, string>> = {
  NoBackend: 'no per-process audio backend is compiled into this build',
  BadOptions: 'the start options did not match the compiled contract',
  BadArguments: 'the addon was called without an availability callback',
  AlreadyStarted: 'a capture is already running in this process',
  ThreadStartFailed: 'the producer thread could not be created',
  NoTarget: 'no capture target was supplied',
  PermissionDenied: 'the operating system refused capture consent',
  UnsupportedFormat: 'the source is not 48 kHz 32-bit float mono or stereo',
  DeviceError: 'the operating system refused to create or start the capture',
  Poisoned: 'this process has already used its one capture',
};

/**
 * `AudioCapStatus.faultReason`, restated for the reason `AudioCapStartFailure`
 * is: `native/concord-audiocap/index.d.ts` is the authority and cannot be an
 * import here. The fault-watch suite in `audiocapChild.test.ts` pins the key set
 * against that union.
 */
type AudioCapFaultReason =
  'None' | 'DeviceLost' | 'PermissionLost' | 'NoCallbacks' | 'FormatChanged';

/**
 * How often the child reads `status().faulted` while a capture is live (#3394
 * PR 2, spec §4.1). One field per second: the watch detects nothing itself, it
 * only notices a latch the native pump already made.
 */
export const FAULT_WATCH_INTERVAL_MS = 1000;

/**
 * A latched pump fault, as the phrase a `fault{stage:'run'}` carries. CLOSED AND
 * PID-FREE BY CONSTRUCTION — every value is a literal written here, so nothing
 * the addon returns is ever echoed (I-PID; observability.md principles 3 and 7).
 * Main derives the renderer's interrupt reason from the STAGE, never from this
 * text, which exists only for a developer reading main's log.
 *
 * A `Record` keyed by the union, like `START_FAILURE_REASONS`, so a member added
 * to the native union without a phrase here is a compile error.
 *
 * Only `FormatChanged` has a macOS producer today (`rt/platform/macos/
 * tap_backend.h`, the `kFormatChanged` fault). The other members are declared by
 * the native contract for the Windows backend and are phrased now so that
 * backend lands as wiring rather than as a hole in this map.
 */
export const RUN_FAULT_PHRASE: Readonly<Record<AudioCapFaultReason, string>> = {
  None: 'the capture reported a fault with no reason',
  FormatChanged: 'the captured audio format changed',
  DeviceLost: 'the capture device was lost', // no producer on macOS; #3196
  PermissionLost: 'capture permission was lost', // no producer on macOS; #3196
  NoCallbacks: 'the capture stopped delivering audio', // no producer on macOS; #3196
};

/**
 * A `faultReason`, named only when it is one this build knows. `Object.hasOwn`
 * for the reason `startFailureMessage` gives: a bare lookup of `'constructor'`
 * would return an inherited FUNCTION and fail the closed set open. Anything else
 * — a non-string, an unknown member, a prototype key — gets the fixed fallback.
 */
function runFaultMessage(reason: unknown): string {
  if (typeof reason === 'string' && Object.hasOwn(RUN_FAULT_PHRASE, reason)) {
    return (RUN_FAULT_PHRASE as Record<string, string>)[reason];
  }
  return 'the capture failed';
}

/**
 * `audiocapProtocol.ts` caps `hello.envKeys` at a module-private 16 and
 * `isAudiocapHello` REJECTS a longer array — and a rejected hello is a killed
 * child with no diagnosis at all. Truncating here keeps that receiver strict
 * without letting an honest child trip it. Keep the two numbers equal.
 */
const HELLO_ENV_KEYS_MAX = 16;

/** C6. The first violation is counted; the second is terminal. */
const PCM_PROTOCOL_FAULT_LIMIT = 2;

// ---------------------------------------------------------------------------
// Mutable state — one capture at a time, one port at a time
// ---------------------------------------------------------------------------

let pcmPort: PcmPort | null = null;
let outstanding = 0;
let protocolViolations = 0;
let capturing = false;
/** The live capture's fault watch, or `null` when none is armed. See `armFaultWatch`. */
let faultWatch: ReturnType<typeof setInterval> | null = null;

// ---------------------------------------------------------------------------
// Diagnostics — the only place an unknown value becomes an outbound string
// ---------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

/**
 * C3. `.message` and nothing else. A caught value that is not an `Error` gets a
 * fixed identifier rather than a coerced stringification, because coercion is how
 * a thrown object's own fields — including a nested cause — reach a sink.
 */
function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error ? err.message : fallback;
}

/**
 * I-PID. `errorMessage` forwards a native `.message` verbatim, and
 * `sanitizeDiagnostic`'s charset (`[^A-Za-z0-9. _-]`) PRESERVES DIGITS — so a
 * backend that names the process it failed on ("CATapDescription failed for pid
 * 4242") sends that PID across the process boundary and into main's
 * `console.warn`. That is the one value ADR-0043 says must never leave this child.
 *
 * Proven as a mechanism by a red-team PoC, with the precondition stated: no
 * first-party binary produces such a string today, because `startFailureMessage`
 * maps a closed reason union to fixed phrases and the `NAPI_CALL` path carries
 * fixed N-API text. It arms the moment a backend author writes a conventional
 * diagnostic — which is exactly what Core Audio and WASAPI error paths do, and
 * #3197's macOS tap backend is the code most likely to do it.
 *
 * Digits are stripped rather than the message dropped: the text is the only
 * diagnostic a packaging or device fault leaves behind, and a PID is the only
 * part of it that is forbidden.
 *
 * TWO CALL SITES, AND THEY ARE A PAIR. Both run only after `resolveTargetPid`
 * succeeded: the `addon.start` catch, and the teardown exception inside
 * `unwindCapture`. The first shipped sanitised and the second did not, which left
 * the invariant broken through the other door — the two halves are concatenated
 * into ONE `postFault(stage, ...)`, so a PID in either reaches the same log line.
 * The teardown half is posted on whichever stage the unwind reports — `'start'`,
 * `'target'` for a `NoTarget` refusal, or `'run'` from the fault watch — and every
 * one of those follows target resolution. If a third forwarding site is ever added
 * after target resolution, it belongs here too; grep `errorMessage(` before
 * assuming there are only two.
 *
 * The `load` and `capability` stages run BEFORE any PID exists and deliberately
 * keep their text intact — sanitising them would cost diagnostics for no gain.
 */
function pidFreeErrorMessage(err: unknown, fallback: string): string {
  return errorMessage(err, fallback).replace(/\d/g, '');
}

/** A capability string, capped and charset-stripped to what a `hello` may carry. */
function diagnostic(value: unknown): string {
  return typeof value === 'string' ? sanitizeDiagnostic(value, DIAGNOSTIC_MAX_CHARS) : '';
}

function postFault(stage: AudiocapFaultStage, message: string): void {
  const fault: AudiocapFault = {
    kind: 'fault',
    stage,
    message: sanitizeDiagnostic(message, FAULT_MESSAGE_MAX_CHARS),
  };
  process.parentPort.postMessage(fault);
}

function postStarted(): void {
  const started: AudiocapStarted = { kind: 'started' };
  process.parentPort.postMessage(started);
}

// ---------------------------------------------------------------------------
// Bring-up: load, probe, announce
// ---------------------------------------------------------------------------

function loadAddon(): AudiocapAddon | null {
  try {
    // createRequire rather than a bare require(): `module: CommonJS` would accept
    // either, but no other file under src/main/ uses a bare require, and this keeps
    // the resolution explicit. Relative to THIS file in both layouts —
    // src/main/ -> client/desktop/native/, and dist/main/ -> /native inside app.asar,
    // which is exactly where the ignore lookahead admits the loader.
    const requireFromHere = createRequire(__filename);
    return requireFromHere('../../native/concord-audiocap') as AudiocapAddon;
  } catch (err) {
    postFault('load', errorMessage(err, 'the native addon could not be loaded'));
    return null;
  }
}

/**
 * Probe the addon and narrow what it returned.
 *
 * The strings are sanitised HERE, on the sending side, even though
 * `isAudiocapHello` will check them again on the receiving side. That is not
 * belt-and-braces: main's check is strict and its failure mode is a killed child,
 * so an honest `reason` carrying an OS message with a colon in it would read as a
 * hostile hello. Sanitising at the sender leaves main's strictness pointed where
 * it belongs — at a child that is lying — rather than at one that is verbose.
 */
function probeCapability(addon: AudiocapAddon): AudiocapCapability | null {
  let raw: unknown;
  try {
    raw = addon.capability();
  } catch (err) {
    postFault('capability', errorMessage(err, 'the capability probe failed'));
    return null;
  }

  // `perProcessAudio` is the one field with no safe default. C9 forbids treating
  // its absence as permission, and coercing a truthy value would be #2161's
  // defect arriving on a different channel — so an unusable shape is a fault, not
  // a `false`.
  if (!isRecord(raw) || typeof raw.perProcessAudio !== 'boolean') {
    postFault('capability', 'the capability probe returned an unusable shape');
    return null;
  }

  return {
    platform: diagnostic(raw.platform),
    osVersion: diagnostic(raw.osVersion),
    perProcessAudio: raw.perProcessAudio,
    reason: diagnostic(raw.reason),
  };
}

/**
 * The allowlisted environment the host actually handed us, by NAME only — never
 * a value. Sorted so the report is deterministic, and truncated so it cannot trip
 * `isAudiocapHello`'s bound.
 */
function describeEnvKeys(): string[] {
  return Object.keys(process.env)
    .map((key) => sanitizeDiagnostic(key, DIAGNOSTIC_MAX_CHARS))
    .filter((key) => key.length > 0)
    .sort((a, b) => a.localeCompare(b))
    .slice(0, HELLO_ENV_KEYS_MAX);
}

function postHello(capability: AudiocapCapability): void {
  const hello: AudiocapHello = {
    kind: 'hello',
    protocol: AUDIOCAP_PROTOCOL,
    capability,
    resourcesPathPresent:
      typeof process.resourcesPath === 'string' && process.resourcesPath.length > 0,
    envKeys: describeEnvKeys(),
  };
  process.parentPort.postMessage(hello);
}

// ---------------------------------------------------------------------------
// The PCM port
// ---------------------------------------------------------------------------

function listenOnPort(port: PcmPort, onMessage: (data: unknown) => void): boolean {
  if (typeof port.addEventListener === 'function') {
    port.addEventListener('message', (event) => {
      onMessage(event.data);
    });
  } else if (typeof port.on === 'function') {
    port.on('message', (event) => {
      onMessage(event.data);
    });
  } else {
    return false;
  }
  // Required on the DOM shape when the listener went on via `addEventListener`,
  // and harmless on `MessagePortMain`, which also queues until `start()`.
  if (typeof port.start === 'function') port.start();
  return true;
}

/**
 * Forget the port before closing it, so a re-entrant call cannot double-close and
 * so a drain already inside its loop finds nothing to post to. `outstanding` goes
 * with it: credits are a property of a live port, and carrying a stale count into
 * a successor capture would silently shrink its bound.
 */
function closePcmPort(): void {
  const port = pcmPort;
  pcmPort = null;
  outstanding = 0;
  if (port) port.close();
}

function isCredit(value: unknown): boolean {
  return isRecord(value) && Number.isInteger(value.c) && value.c === 1;
}

/**
 * C4. The whole flow-control policy, in one loop.
 *
 * `drain()` returning anything but `{ok: true}` means the ring is empty (or the
 * capture is over) and we stop — the addon's contract is "drain in a loop until
 * `drain()` returns `{ok:false}` or you reach the credit bound", and the signal
 * that brought us here means "there MAY be something", never "there is exactly
 * one thing". The `!== true` is strict so a malformed result stops the loop
 * rather than posting a buffer the addon may not have filled.
 */
function drainAvailable(addon: AudiocapAddon): void {
  while (outstanding < CREDIT_BOUND) {
    const port = pcmPort;
    if (!port) return;

    // C8. One fresh buffer per posted quantum; `drain` fills it in place and the
    // post copies it. Reusing a single scratch buffer would rest on the
    // serializer being synchronous, which is true of Electron's but is an
    // unverified assumption this file does not need to make.
    const buffer = new ArrayBuffer(QUANTUM_BYTES);
    const result = addon.drain(buffer);
    if (!isRecord(result) || result.ok !== true) return;

    port.postMessage(buffer);
    outstanding += 1;
  }
}

function handlePcmMessage(addon: AudiocapAddon, data: unknown): void {
  if (isCredit(data)) {
    // C5. Floored, so an unearned credit cannot buy a ninth buffer in flight.
    if (outstanding > 0) outstanding -= 1;
    drainAvailable(addon);
    return;
  }

  // C6. Counted, then terminal.
  protocolViolations += 1;
  if (protocolViolations < PCM_PROTOCOL_FAULT_LIMIT) return;
  // Disarmed BEFORE the post: `fault{protocol}` is this capture's one terminal
  // report, and main retires the child on it. A pump fault latching in the gap
  // before the kill must not add a second, contradictory `fault{run}`.
  clearFaultWatch();
  postFault('protocol', 'the PCM port carried a message that is not a single credit');
  closePcmPort();
}

// ---------------------------------------------------------------------------
// The control channel
// ---------------------------------------------------------------------------

/**
 * Exact equality against the compiled constants, every field (spec §4a). A
 * mismatch means main and this build disagree about the wire, which is a `start`
 * fault rather than something to negotiate: nothing here reinterprets a field.
 */
function matchesCompiledGeometry(message: Record<string, unknown>): boolean {
  return (
    message.quantumMs === QUANTUM_MS &&
    message.sampleRate === SAMPLE_RATE &&
    message.channels === CHANNELS &&
    message.frameCount === FRAME_COUNT &&
    message.creditBound === CREDIT_BOUND &&
    message.ringSlots === RING_SLOTS
  );
}

/**
 * An `ok:false` reason, named only when it is one this build knows.
 *
 * `Object.hasOwn`, NOT a bare `START_FAILURE_REASONS[reason]`. The map is an
 * object literal and therefore inherits `constructor`, `toString`, `valueOf` and
 * the rest of `Object.prototype`; the addon is an unvalidated source, and a
 * `reason` of `'constructor'` would make a bare lookup return a FUNCTION —
 * truthy, defined, and interpolated straight into an outbound fault message.
 * That is the closed set failing open through the one door a closed set is
 * supposed to shut.
 */
export function startFailureMessage(reason: unknown): string {
  if (typeof reason === 'string' && Object.hasOwn(START_FAILURE_REASONS, reason)) {
    const phrase = (START_FAILURE_REASONS as Record<string, string>)[reason];
    return `capture did not start - ${phrase}`;
  }
  return 'capture did not start - the addon gave no recognised reason';
}

/**
 * The handle from the `start` message to the PID that owns it, or `null` for
 * every refusal — a non-numeric field, a handle the OS does not recognise, a
 * window that closed between pick and start, a platform with no implementation,
 * or a native return this build will not believe.
 *
 * `null`, NEVER 0, and the return is re-narrowed rather than trusted: the `.d.ts`
 * is a claim ABOUT a native binary, not a guarantee FROM one, and a numeric 0
 * invites `if (pid)` — the falsy-check shape that turns a refusal into a widened
 * capture (#2161). The addon validates the list again on its own side; the
 * redundancy is correct, there is no TOCTOU fix here, only a refusal at each edge.
 */
function resolveTargetPid(addon: AudiocapAddon, windowHandle: unknown): number | null {
  if (typeof windowHandle !== 'number') return null;

  // GUARDED, like every other native call in this file. It was not, and a red-team
  // PoC proved what that cost: a throw escaped resolveTargetPid -> handleStart ->
  // handleControlMessage -> the parentPort listener, and the child died as an
  // uncaught exception having posted NOTHING. That breaks unwindCapture's own
  // stated invariant ("exactly one fault is posted on every path") and lands main
  // on `child-crash`, which audiocapHost.ts documents as a PACKAGING DEFECT — so
  // the user reads "App sound stopped unexpectedly" for a window that simply could
  // not be resolved.
  //
  // The trigger is not exotic: a `concord_audiocap.node` built before #3198 has no
  // `resolveWindowOwner` export, so the call is a TypeError and EVERY share start
  // kills the capture child with no diagnostic anywhere. A stale dev tree or a
  // half-updated install reaches it.
  //
  // The caught value is DISCARDED, not reported. A throw is one more way the handle
  // did not resolve, and `'target'` already says that; forwarding `err.message`
  // would put native text on the wire from the one function that has a PID in
  // scope (see the digit strip at the `start` unwind below).
  let resolved: unknown;
  try {
    resolved = addon.resolveWindowOwner(windowHandle);
  } catch {
    return null;
  }

  if (typeof resolved !== 'number' || !Number.isInteger(resolved) || resolved <= 0) return null;
  return resolved;
}

/**
 * Handle a `start` control message: validate it, then hand the PCM port to the addon.
 *
 * All four validation rejections call `postFault` and return; nothing propagates out
 * of this function, including a throw from `addon.start()`, which is caught below.
 * That is deliberate — this runs in the utility process, so an escaping throw would
 * reach the host as an opaque child exit rather than a named stage it can report.
 *
 * The `capturing = true` ordering around the addon call is the load-bearing part and
 * is explained at the assignment; do not move it without reading that comment.
 *
 * @param addon - the loaded native addon; owns the OS tap once `start` succeeds.
 * @param port - the PCM port from the control channel, `null` if the message carried
 *   none. A missing port is a fault, not a degraded start: there is nowhere to send
 *   audio, and starting the tap anyway would open a capture nothing drains.
 */
function handleStart(
  addon: AudiocapAddon,
  message: Record<string, unknown>,
  port: PcmPort | null
): void {
  if (!matchesCompiledGeometry(message)) {
    postFault('start', 'the start geometry does not match the compiled constants');
    return;
  }
  if (!port) {
    postFault('start', 'the start message carried no PCM port');
    return;
  }
  if (capturing) {
    postFault('start', 'a capture is already running in this child');
    return;
  }

  // RESOLVE HERE, IN THIS PROCESS, AND NOWHERE ELSE (ADR-0043 D5, invariant
  // I-PID). Main holds the handle; this is the only process that ever learns a
  // PID, and the PID is never sent back, logged or counted.
  //
  // BEFORE the addon is asked to capture and before this child takes ownership
  // of anything — no port listener, no `pcmPort`, no `capturing` — so
  // `fault{stage:'target'}` means NOTHING WAS EVER TAPPED and the refusal needs
  // no unwind. That is precisely why it is not stage 'start'.
  const pid = resolveTargetPid(addon, message.windowHandle);
  if (pid === null) {
    postFault('target', 'window handle did not resolve to a live owner');
    return;
  }

  if (
    !listenOnPort(port, (data) => {
      handlePcmMessage(addon, data);
    })
  ) {
    postFault('start', 'the PCM port exposes no message listener');
    return;
  }

  // Attach BEFORE starting the addon, never after: `start()` may signal
  // availability synchronously, and the coalesced signal that finds no port is
  // one that is dropped rather than queued (C4). Nothing has been posted yet, so
  // a failure below is still a clean unwind.
  pcmPort = port;
  outstanding = 0;
  protocolViolations = 0;

  // ARMED BEFORE THE CALL, NEVER AFTER, and the ordering is the whole point.
  // `addon.start()` can fail two ways that both leave a capture possibly LIVE: it
  // can throw out of the N-API seam after the native side armed itself, and it
  // can return `ok:false` from a backend that acquired an OS artefact before the
  // step that failed (napi/addon.cc tears itself down through its own five steps
  // for exactly that reason). Set after the call, `capturing` was still false on
  // both of those paths — so `handleStop`'s `if (!capturing) return;` REFUSED to
  // stop them. Main asks for teardown, this child does nothing, and the tap
  // outlives the share: the privacy invariant #3197 exists for, inverted by where
  // one line sits.
  //
  // Arming early costs at most one idempotent no-op: `stop()` is documented
  // idempotent and the native `stopCapture()` handles the never-armed case.
  capturing = true;

  let result: unknown;
  try {
    result = addon.start(
      {
        quantumMs: QUANTUM_MS,
        sampleRate: SAMPLE_RATE,
        channels: CHANNELS,
        frameCount: FRAME_COUNT,
        ringSlots: RING_SLOTS,
        // ONE root, and the addon expands it to the owner's process tree natively
        // (see `AddonStartOptions.allowDescendants`). No descendant PID exists here.
        targetPids: [pid],
        allowDescendants: true,
      },
      () => {
        drainAvailable(addon);
      }
    );
  } catch (err) {
    // THROUGH THE TEARDOWN, NOT BY HAND. The unwind IS the teardown — the same
    // rule napi/addon.cc states for its own failure path ("one teardown, not
    // two") — so this closes the port, drops `capturing` and calls
    // `addon.stop()`, where it previously closed the port and left the addon
    // running. Guarded, because the seam has already failed once: see
    // unwindCapture.
    // pidFreeErrorMessage, NOT errorMessage: `pid` is in scope here and the native
    // text is unvalidated. See that function for the I-PID reasoning.
    unwindCapture(addon, pidFreeErrorMessage(err, 'capture did not start'), 'start');
    return;
  }

  if (!isRecord(result) || result.ok !== true) {
    // Read BEFORE the unwind, so the reported reason is the addon's own and
    // cannot be affected by anything teardown does.
    const reason = isRecord(result) ? result.reason : undefined;

    // `NoTarget` IS A TARGET OUTCOME, NOT A BACKEND ONE, and reporting it as
    // `'start'` made the app lie about the machine. MEASURED 2026-09-18: sharing a
    // SILENT app (a Finder window) reaches here with `NoTarget`, because a process
    // that has never produced audio has no audio object for the macOS tap to attach
    // to. `'start'` maps to `'no-backend'`, whose copy reads "App sound isn't
    // available on this computer." — and it demonstrably IS available: the same
    // build captured a browser window seconds later. That is exactly the false claim
    // #3198 exists to delete, arriving through the degrade path instead of the copy.
    //
    // `'target'` maps to `'target-unresolved'` — "We couldn't capture that app's
    // sound." — which is true of every cause collapsed into that member, this one
    // included. The collapse is A7's, and it holds here for the same reason: the
    // distinction between "that app makes no sound" and "the OS refused" is not one
    // the user can act on differently, and splitting it would re-open the
    // discriminator the member exists to close.
    //
    // The UNWIND IS UNCHANGED. `capturing` is already true at this point, so the
    // teardown still runs exactly as before; only the stage reported to main moves.
    // That is why this passes a stage rather than returning early to `postFault`:
    // skipping the unwind here would leave a possibly-armed addon behind, which is
    // the defect the unwind exists to prevent.
    const stage: AudiocapFaultStage = reason === 'NoTarget' ? 'target' : 'start';
    unwindCapture(addon, startFailureMessage(reason), stage);
    return;
  }

  // THE TAP EXISTS. This ack — not `hello` — settles main's start promise (#3394, spec C1).
  // Posted last, after every refusal above has returned, so a refusal can never be
  // preceded by a `started` that already told the renderer the share has sound.
  postStarted();
  // Armed only now, and only here: this is the one line every refusal above returns
  // before, so no watch ever runs for a capture that never started, and main can
  // never receive `fault{run}` ahead of the `started` it describes.
  armFaultWatch(addon);
}

// ---------------------------------------------------------------------------
// The fault watch (#3394 PR 2, spec §4.1)
// ---------------------------------------------------------------------------

/**
 * Disarm the watch. Idempotent. Every path that ends a capture calls it, and
 * `handleStop` calls it as its FIRST statement, ahead of its own `capturing`
 * guard, so no teardown can leave a watch polling a capture that is over.
 */
function clearFaultWatch(): void {
  if (faultWatch === null) return;
  clearInterval(faultWatch);
  faultWatch = null;
}

/**
 * A WATCH, NOT AN EVENT. `QuantumPump::fault()` latches without signalling, and
 * after a format change every callback takes the fault arm, so no quantum is
 * emitted and the drain callback never runs again — an event-driven report would
 * need `rt/` to raise the availability signal on a fault, and that signal is
 * dropped when it lands while this child is credit-blocked (C4). One field read
 * per second has neither problem (spec §4.1, §13 decision 1).
 *
 * THIS IS NOT C4'S FORBIDDEN TIMER. C4 bans a timer that DRAINS, because that
 * makes the consumer the clock and moves the drop site off the ring. This one
 * never touches `drain`, the port or `outstanding`; flow control is unchanged.
 *
 * What it may do is bounded by I1: it can only END a capture, never select a
 * target, a scope or a mode. On a latched fault it disarms itself, then runs the
 * same wrapped teardown a failed start does — the addon is stopped and
 * `reportLiveness` runs BEFORE the fault is posted — and posts exactly one
 * `fault{stage:'run'}` whose text comes from the closed `RUN_FAULT_PHRASE` map.
 * Nothing else `status()` returns reaches the wire (I-PID).
 *
 * A throwing `status()`, a throwing property read, or a malformed snapshot skips
 * that tick silently: the caught value is native and unvalidated, and there is no
 * sink here it may reach (observability.md principle 3). `unref()` so a watch can
 * never be what keeps this process alive.
 */
function armFaultWatch(addon: AudiocapAddon): void {
  clearFaultWatch();
  faultWatch = setInterval(() => {
    let faulted: unknown;
    let reason: unknown;
    try {
      const snapshot = addon.status();
      if (!isRecord(snapshot)) return;
      faulted = snapshot.faulted;
      reason = snapshot.faultReason;
    } catch {
      return;
    }
    if (faulted !== true) return;
    clearFaultWatch();
    unwindCapture(addon, runFaultMessage(reason), 'run');
  }, FAULT_WATCH_INTERVAL_MS);
  faultWatch.unref();
}

/**
 * `stop` stops the capture and closes the port. It deliberately does NOT exit —
 * main kills the child (spec §4a), and a child that exited itself here would
 * reach main's `exit` handler as `child-crash`, i.e. as a packaging defect.
 *
 * `addon.stop()` is documented synchronous and idempotent and is called only after
 * `capturing` is dropped and the port is closed, so a native throw cannot leave the
 * port open. It is deliberately not wrapped HERE: on the ordinary `stop` path main
 * has already asked for teardown and is about to kill us, so an empty catch would
 * only convert a native defect into silence. The two paths that need a report to
 * survive a throwing `stop()` — a failed start and a run fault — reach this through
 * `unwindCapture`, which wraps it.
 */
function handleStop(addon: AudiocapAddon): void {
  // FIRST, AHEAD OF THE GUARD, deliberately. Disarming is correct whether or not a
  // capture is live, and placing it after `if (!capturing) return;` would make it
  // conditional on a flag it has nothing to do with — a watch that outlived its
  // capture would then keep reading `status()` and could post a `fault{run}` after
  // main had already asked for teardown.
  clearFaultWatch();
  // ALSO THE UNWIND FOR A FAILED START AND FOR A RUN FAULT (see unwindCapture),
  // which is why the guard below is a no-op rather than a fault: a `stop` arriving
  // after a capture that already tore itself down is the ordinary case, not a
  // protocol error.
  if (!capturing) return;
  capturing = false;
  closePcmPort();
  addon.stop();
  reportLiveness(addon);
}

/**
 * DESIGN §6.2's CONSENT/LIVENESS LINE — the post-mortem reader of `status()`.
 *
 * It is one of two readers. The fault watch also calls `status()` while a capture
 * is live, but reads only `faulted` and `faultReason`; the counters and the
 * teardown evidence below are read here and nowhere else.
 *
 * R9 measured that TCC denial on macOS 26 yields on-schedule, correctly-shaped,
 * ZERO-FILLED callbacks with every Core Audio API returning `noErr`. There is no
 * error code to report, so `signalTotal` and `silentSinceStart` are the only
 * witnesses that a tap is alive and starved rather than alive and working.
 * Without this call they were computed on every callback and read by nobody:
 * `status()` had no caller anywhere in the desktop tree.
 *
 * WHY IT IS A LOG AND NOT A MESSAGE. The child→main protocol is a closed set —
 * `hello`, `started`, `fault` — and nothing in it carries counters. #3394 PR 2
 * decided against carrying them: its mid-share report rides the existing `fault`
 * kind as stage `'run'` rather than a new kind, and the silence advisory that would
 * have consumed these counters was dropped, because the measurements it was built
 * for showed neither of its triggers occurs as designed. This epic had already
 * decided the underlying question once, too: #3195 DELETED an unwired watchdog
 * rather than ship it inert. So this reports through the channel that already
 * exists and stays a developer diagnostic.
 *
 * It is read only in an unpackaged build — `audiocapHost.ts` echoes this child's
 * stderr there and discards it otherwise — so this is a dev diagnostic by
 * construction, with no UI and therefore no visual-verification obligation.
 *
 * The specific thing it prevents: the Electron postinstall re-sign resets the
 * developer's audio-capture TCC grant on every Electron bump, and the symptom is
 * a dead-silent stream with no error — byte-identical to the revocation case
 * they would be debugging.
 */
function reportLiveness(addon: AudiocapAddon): void {
  let snapshot: unknown;
  try {
    snapshot = addon.status();
  } catch {
    // A throw here must never become the child's cause of death. The capture is
    // already stopped and main is about to kill us; a diagnostic that killed the
    // process it was diagnosing would be worse than no diagnostic.
    return;
  }
  if (!isRecord(snapshot)) return;
  // Counters and booleans only — no key material, no PII, no raw error (C8).
  // console.warn, never debug/log/info: those write to STDOUT, which the host
  // drains unread; it echoes only STDERR. This shipped on console.debug and
  // reached nobody until T0 (#3394) read the log and found it empty.
  console.warn('[audiocap] liveness', {
    callbackTotal: snapshot.callbackTotal,
    quantaTotal: snapshot.quantaTotal,
    signalTotal: snapshot.signalTotal,
    silentSinceStart: snapshot.silentSinceStart,
    overrunTotal: snapshot.overrunTotal,
    faulted: snapshot.faulted,
    poisoned: snapshot.poisoned,
    quiesceProved: snapshot.quiesceProved,
    destroyFailures: snapshot.destroyFailures,
    lastDeviceStatus: snapshot.lastDeviceStatus,
  });
}

/**
 * The unwind for a capture that ended on its own — a FAILED start, or a pump fault
 * the watch found latched mid-share — and the only place `addon.stop()` is wrapped.
 *
 * `handleStop` leaves it unwrapped deliberately (see its docblock), and that is
 * right for the ordinary `stop` path: main has already asked for teardown and is
 * about to kill us, so there is no diagnostic a catch could protect and an empty
 * one would only convert a native defect into silence. Here the situation is the
 * opposite. The seam has ALREADY thrown, refused or faulted once, so a throw from
 * `stop()` on the same broken module is plausible — and it would propagate past
 * the caller's `postFault`, killing the child with the original reason never
 * reported. The whole point of routing these ends through teardown is to end
 * with a clean fault; an unguarded call can swallow exactly that.
 *
 * TEARDOWN FIRST, THEN THE REPORT, on every path. A `fault` main receives means the
 * tap is already destroyed, never that it is about to be.
 *
 * `stage` IS REQUIRED, with no default. It had one (`'start'`), and a defaulted
 * parameter is the `screenAudioRefusalMessage(platform = null)` defect shape: a
 * caller that forgets it compiles, and here it would report a run fault as a
 * failed start — which main maps to "this computer cannot do it" rather than "the
 * share lost its sound". Every caller names its stage.
 *
 * `handleStop` drops `capturing` and closes the PCM port BEFORE calling
 * `addon.stop()`, so a throw from it leaves this side already torn down and only
 * the native side unknown. `postFault` posts on `process.parentPort`, not the PCM
 * port, so it still reaches main afterwards.
 *
 * The second failure is deliberately NOT rethrown. Main reads a child that exits
 * on its own as `child-crash` — a packaging defect (see handleStop's docblock) —
 * so rethrowing would replace an accurate fault with a misleading one. Report
 * both causes in one fault and return; main kills us, which is the documented
 * teardown. Exactly one fault is posted on every path.
 */
function unwindCapture(addon: AudiocapAddon, message: string, stage: AudiocapFaultStage): void {
  try {
    handleStop(addon);
  } catch (unwindErr) {
    // The ORIGINAL reason goes first, deliberately: `postFault` truncates at
    // FAULT_MESSAGE_MAX_CHARS, so the half a reader needs must not be the half
    // that is cut. The separator avoids `(` `)` and `:` because DIAGNOSTIC_STRIP
    // deletes them silently -- composing punctuation the sink is known to remove
    // is how a message ends up reading as though words were missing.
    // BOTH HALVES ARE PID-FREE, not just the caller's. `message` is already
    // PID-free — `pidFreeErrorMessage` at the throw site, a closed phrase map at
    // every other — and this half must match: it is the SAME `postFault(stage, ...)`,
    // on a path that only runs after target resolution, and a native `stop()`
    // diagnostic is target-scoped, so a
    // backend naming the process it failed to release lands the PID here instead.
    // Sanitising the first half and not the second leaves the invariant exactly
    // as broken as before, through the other door (#3198 PR 2 review, CWE-209).
    postFault(
      stage,
      `${message} - teardown also failed - ${pidFreeErrorMessage(unwindErr, 'stop threw')}`
    );
    return;
  }
  postFault(stage, message);
}

function handleControlMessage(
  addon: AudiocapAddon,
  event: { data: unknown; ports?: readonly PcmPort[] }
): void {
  const message = event.data;
  if (isRecord(message)) {
    if (message.kind === 'start') {
      const ports = event.ports;
      handleStart(addon, message, (Array.isArray(ports) ? ports[0] : undefined) ?? null);
      return;
    }
    if (message.kind === 'stop') {
      handleStop(addon);
      return;
    }
  }
  // C7. No unknown-message-ignored branch. Main authored this, and the exit is
  // the whole response.
  process.exit(1);
}

// ---------------------------------------------------------------------------
// Entry
// ---------------------------------------------------------------------------

function run(): void {
  const addon = loadAddon();
  if (!addon) return;

  const capability = probeCapability(addon);
  if (!capability) return;

  postHello(capability);

  // Registered only once bring-up succeeded, so `addon` is a proven non-null
  // capture rather than a module-level nullable every handler must re-check.
  process.parentPort.on('message', (event) => {
    handleControlMessage(addon, event);
  });
}

run();
