/**
 * concord-audiocap — per-process screen-share audio capture.
 * See [internal]0043-per-process-screen-share-audio-capture.md.
 *
 * SURFACE: five functions — `capability`, `start`, `drain`, `stop`, `status`.
 *
 * `AudioCapStartFailure` is a CLOSED union that ships whole in PR 1, including
 * members only a PR-2 real backend can produce (see the member-by-member notes
 * below). TCC/consent status is answered ONLY by `start()` returning
 * `{ ok: false, reason: 'PermissionDenied' }` — there is no non-prompting
 * preflight. `capability()` never reports consent: it answers "is per-process
 * capture available on this OS/version", which is orthogonal to whether the user
 * has granted it, so a `consent` field here would read 'unknown' forever and
 * invite a caller to treat that as a real answer.
 */

export interface AudioCapCapability {
  /** 'win32' | 'darwin' | 'linux' | 'unsupported' */
  platform: string;
  /** Windows: the build number. macOS: kern.osproductversion, e.g. "14.4.1". */
  osVersion: string;
  /**
   * Whether per-process audio capture is available on THIS machine.
   *
   * A false value means the capability ladder's bottom rung applies: share video
   * only, and say so in the UI. It never means "fall back to a system mix" — a
   * window target must never obtain one (ADR-0043 D6, #2161).
   */
  perProcessAudio: boolean;
  /** Empty when supported; otherwise why not, in words fit to show a user. */
  reason: string;
}

export function capability(): AudioCapCapability;

/**
 * The geometry `start()` is handed. Every field is a LITERAL type on purpose: the
 * addon compares each one for exact equality against the constants compiled into
 * `rt/quantum_header.h`, so a caller that cannot express the exact value is a
 * caller that would have been refused at runtime.
 *
 * These mirror `src/shared/audiocapProtocol.ts`, which is the authority. Three
 * copies of these numbers now exist — that file, this one, and the C++ header —
 * and the exact-equality check at `start` is what makes a drift between them a
 * reported fault rather than a silent reinterpretation of the wire.
 */
export interface AudioCapStartOptions {
  quantumMs: 10;
  sampleRate: 48000;
  channels: 2;
  frameCount: 480;
  ringSlots: 8;

  /**
   * OPTIONAL. The processes to capture: 1..8 PIDs, every one a positive integer.
   *
   * Not a literal type, because unlike the geometry above it is a REQUEST rather
   * than a mirror of a compiled constant. It is still validated exactly: a
   * non-array, an empty array, more than 8 entries, a non-number element, a
   * non-integer, a zero and a negative are each `BadOptions`. Nothing is
   * truncated and nothing is coerced — a target selector quietly reinterpreted
   * is the same defect as one silently ignored (#2161).
   *
   * ABSENT is not an error and is the PR-1 state: a real backend fails closed
   * with `NoTarget`, and the synthetic backend ignores the target entirely
   * because it is a generator, not a tap. #3198 is the first caller that
   * supplies it.
   *
   * WHY A BOUNDED LIST AND NOT ONE PID. The window's PID is not necessarily the
   * process rendering the audio (ADR-0043 risk 2). Windows expresses that with
   * `INCLUDE_TARGET_PROCESS_TREE` on one PID; macOS cannot — `CATapDescription`
   * takes an explicit process-object list. 8 is a bound on a caller-supplied
   * array length, not a claim about how many helper processes a browser has.
   */
  targetPids?: number[];

  /**
   * OPTIONAL, default false. Honoured natively by Windows; on macOS the list is
   * documented as ALREADY EXPANDED by the caller.
   *
   * Only meaningful alongside `targetPids`, and supplying it without one is
   * `BadOptions` rather than being ignored — there is no list for it to expand,
   * so the only alternative is to accept a request the addon did not honour. A
   * non-boolean is `BadOptions` too: widening a capture to a process tree on a
   * truthy coercion is the permissive reading of an ambiguous request.
   */
  allowDescendants?: boolean;
}

/**
 * Why capture did not start. A CLOSED union rather than `string`, so a future
 * backend adding a reason is a compile error at every exhaustive `switch` rather
 * than a value that falls through a `default` into the permissive branch.
 *
 * THE WHOLE SET IS DECLARED HERE, INCLUDING MEMBERS ONLY A REAL BACKEND CAN
 * PRODUCE. One vocabulary, defined once at the point it is designed, with the
 * child's `START_FAILURE_REASONS` map asserted exhaustive against it by a
 * table-driven test. A union that grew when the macOS backend landed would make
 * that test change twice and invite an unmapped member in between — which
 * degrades a real outcome into "no recognised reason" at the moment a user's
 * share loses its audio.
 *
 * - `NoBackend`      — a release build. There is no producer compiled in at all.
 *                      The host resolves this to video-only WITH A REASON; it
 *                      never means fall back to a system mix (#2161, C9). An OS
 *                      below the capture floor collapses to this member too: from
 *                      the caller's side the outcome is identical, and
 *                      `capability()` is where the difference is reported.
 * - `BadOptions`     — the geometry did not match the compiled constants exactly,
 *                      or `targetPids`/`allowDescendants` was malformed.
 * - `BadArguments`   — no callback, or not a function.
 * - `AlreadyStarted` — a capture is already running in this process.
 * - `ThreadStartFailed` — the producer thread could not be created.
 *                      DECLARED BUT NOT CURRENTLY PRODUCED: `rt/capture_backend.h`
 *                      mandates `kDeviceError` for a failed spawn, so a
 *                      thread-driven backend reports `DeviceError` today. It stays
 *                      in the union because the exhaustive mapping test asserts a
 *                      mapping EXISTS for every member, not that every member is
 *                      reachable, and a member deleted here is one a later backend
 *                      re-adds without a mapping.
 * - `NoTarget`       — a backend that requires a target was given none. This is
 *                      the PR-1 state of every real backend: the feature is dark
 *                      until #3198 supplies `targetPids`.
 * - `PermissionDenied` — the OS refused capture consent.
 * - `UnsupportedFormat` — the source is not 48 kHz float32 mono/stereo. NOTHING
 *                      RESAMPLES and nothing downmixes, in `rt/` or anywhere
 *                      else; an unusable source is refused.
 * - `DeviceError`    — the OS refused to create or start the capture.
 * - `Poisoned`       — THIS PROCESS HAS USED ITS ONE CAPTURE. Either a teardown
 *                      could not show the sink empty and the producer stopped, so
 *                      its threadsafe function was abandoned rather than released,
 *                      or a capture was simply armed here already — which includes
 *                      a `start()` that FAILED, because a backend may have acquired
 *                      something before it said no. The sink gate is armed once per
 *                      process and `close()` is permanent, so in either case there
 *                      is no state left in which a second capture could be safe.
 *                      The host forks one child per share, so this is a refusal of
 *                      something production never asks for: a poisoned process can
 *                      only be killed, never made to capture again.
 */
export type AudioCapStartFailure =
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

export type AudioCapStartResult = { ok: true } | { ok: false; reason: AudioCapStartFailure };

/**
 * Begins capture and registers a bare availability signal.
 *
 * ONCE PER PROCESS. The sink gate is armed at the first `start()` and is never
 * replaced, because nothing observable here can prove that the previous capture's
 * producer is gone — see `Poisoned` above. Every later `start()`, including one
 * after a clean `stop()` and one after a `start()` that failed, returns
 * `{ ok: false, reason: 'Poisoned' }`.
 *
 * `onQuantumAvailable` CARRIES NO DATA and is COALESCED: it rides a threadsafe
 * function with `max_queue_size = 1`, always called non-blocking, so a signal
 * arriving while one is pending is dropped. That is what keeps the credit bound
 * measuring the consumer rather than a queue behind it. Treat a signal as "there
 * may be something to drain", never as "there is exactly one thing to drain":
 * drain in a loop until `drain()` returns `{ ok: false }` or you reach the credit
 * bound.
 */
export function start(
  options: AudioCapStartOptions,
  onQuantumAvailable: () => void
): AudioCapStartResult;

/**
 * Moves one whole quantum — `header(32) || 480 x 2 interleaved f32` = 3872 bytes —
 * into `into`, in place, allocating nothing on the payload path.
 *
 * `into` MUST be exactly `QUANTUM_BYTES`. Anything else, a detached buffer, an
 * empty ring, or a call made after `stop()` returns `{ ok: false }` and writes
 * nothing. There is no partial fill: a short read would hand the far end stale
 * bytes past the end of the audio, which is indistinguishable downstream from
 * a correctly transported quantum.
 */
export function drain(into: ArrayBuffer): { ok: boolean };

/**
 * Stops capture, destroys every OS artefact the backend created, shows that nobody
 * is inside the sink AND that the producer stopped moving, and discards anything
 * still queued. Synchronous and idempotent.
 *
 * It costs one settle window — 50 ms — on the ordinary path, because "nobody is
 * inside the gate" is an instant and "the producer stopped" is a window. A backend
 * that kept producing after its own `stop()` returned is detected there, and the
 * process is poisoned rather than reused.
 */
export function stop(): void;

/**
 * What the producer has done, as counters. A PULL: it carries no data path, does
 * not widen `drain`, adds no second threadsafe function, and cannot smuggle
 * audio.
 *
 * `callbackTotal`, `quantaTotal` and `overrunTotal` are SATURATING and are never
 * reset — not by `start()`, not by `stop()`, not by a fault. They are cumulative
 * for the life of the process, and the host forks one child per share, so process
 * lifetime is share lifetime. A wrapped counter would read as "nothing ever
 * happened", which is the one answer they must never be able to give.
 *
 * The fields are read with independent relaxed loads and are therefore NOT a
 * consistent instant. Every one is monotonic, so a torn read is stale rather than
 * wrong.
 *
 * `overrunTotal` and a gap in the header `seq` at the far end are two INDEPENDENT
 * drop witnesses, and both are required: the counter is stamped at capture time
 * and can arrive up to a ring's worth of quanta late, while the seq gap is
 * immediate. They fail in opposite directions.
 */
export interface AudioCapStatus {
  running: boolean;
  /** Saturating; OS callbacks (or synthetic ticks) since process start. */
  callbackTotal: number;
  /** Saturating; whole quanta actually pushed to the ring. */
  quantaTotal: number;
  /** Saturating; mirrors the ring's drop counter. */
  overrunTotal: number;
  faulted: boolean;
  /**
   * Why the producer stopped producing. Closed, for the same reason
   * `AudioCapStartFailure` is. THE FIRST FAULT WINS — a device loss that later
   * surfaces as a format change must not overwrite the cause with its own
   * consequence.
   */
  faultReason: 'None' | 'DeviceLost' | 'PermissionLost' | 'NoCallbacks' | 'FormatChanged';
  /**
   * TRUE ONCE A TEARDOWN COULD NOT SHOW THE SINK EMPTY AND THE PRODUCER STOPPED,
   * and it never goes back to false. The threadsafe function was ABANDONED rather
   * than released, `start()` answers `Poisoned` from here on, and the only
   * remaining way to destroy a tap whose backend refused to destroy it is to KILL
   * THIS PROCESS — which is what the host is expected to do with it (design
   * §4.3), and why it is reported rather than merely latched.
   *
   * A caller must not treat it as a reason to retry, restart, or re-probe: the
   * process cannot capture again by construction.
   */
  poisoned: boolean;
}

/**
 * A pure read of the producer's atomics. Allocation-free on the producer side,
 * and SAFE AFTER `stop()` — it touches neither the sink gate, the backend, nor
 * the threadsafe function, all three of which teardown may have destroyed or
 * abandoned. That is what makes it a post-mortem rather than a monitor.
 *
 * There is deliberately no JS timer here that computes a deadline from these
 * numbers. Whatever watches for a starved capture watches NATIVELY; a signal
 * that crosses a dispatcher inherits the dispatcher's lateness (#2992).
 */
export function status(): AudioCapStatus;
