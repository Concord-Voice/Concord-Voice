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
// C2. THE CHILD NEVER DIES SILENTLY WHERE IT COULD SPEAK. Four stages are
//     reported rather than fallen over: a loader throw is `fault{stage:'load'}`,
//     a `capability()` throw or unusable return is `fault{stage:'capability'}`, a
//     refused or failed `start` is `fault{stage:'start'}`, and a second PCM-port
//     protocol violation is `fault{stage:'protocol'}`. That is what gives the
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
 * `AudioCapStartFailure`, restated. Closed on purpose — an unrecognised reason is
 * reported as a generic start fault rather than echoed, because at that point the
 * string is an unvalidated value from a native binary and the closed set is the
 * only thing that makes it safe to name.
 */
const START_FAILURE_REASONS: readonly string[] = [
  'NoBackend',
  'BadOptions',
  'BadArguments',
  'AlreadyStarted',
  'ThreadStartFailed',
];

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

/** An `ok:false` reason, named only when it is one this build knows. */
function startFailureMessage(reason: unknown): string {
  return typeof reason === 'string' && START_FAILURE_REASONS.includes(reason)
    ? `capture did not start - ${reason}`
    : 'capture did not start - the addon gave no recognised reason';
}

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

  let result: unknown;
  try {
    result = addon.start(
      {
        quantumMs: QUANTUM_MS,
        sampleRate: SAMPLE_RATE,
        channels: CHANNELS,
        frameCount: FRAME_COUNT,
        ringSlots: RING_SLOTS,
      },
      () => {
        drainAvailable(addon);
      }
    );
  } catch (err) {
    closePcmPort();
    postFault('start', errorMessage(err, 'capture did not start'));
    return;
  }

  if (!isRecord(result) || result.ok !== true) {
    closePcmPort();
    postFault('start', startFailureMessage(isRecord(result) ? result.reason : undefined));
    return;
  }

  capturing = true;
}

/**
 * `stop` stops the capture and closes the port. It deliberately does NOT exit —
 * main kills the child (spec §4a), and a child that exited itself here would
 * reach main's `exit` handler as `child-crash`, i.e. as a packaging defect.
 *
 * `addon.stop()` is documented synchronous and idempotent and is called LAST, so
 * a native throw cannot leave the port open. It is deliberately not wrapped: at
 * this point main has already asked for teardown and is about to kill us, so an
 * empty catch would only convert a native defect into silence.
 */
function handleStop(addon: AudiocapAddon): void {
  if (!capturing) return;
  capturing = false;
  closePcmPort();
  addon.stop();
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
