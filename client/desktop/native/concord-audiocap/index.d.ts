/**
 * concord-audiocap — per-process screen-share audio capture.
 * See [internal]0043-per-process-screen-share-audio-capture.md.
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
}

/**
 * Why capture did not start. A CLOSED union rather than `string`, so a future
 * backend adding a reason is a compile error at every exhaustive `switch` rather
 * than a value that falls through a `default` into the permissive branch.
 *
 * - `NoBackend`      — a release build. There is no producer compiled in at all.
 *                      The host resolves this to video-only WITH A REASON; it
 *                      never means fall back to a system mix (#2161, C9).
 * - `BadOptions`     — the geometry did not match the compiled constants exactly.
 * - `BadArguments`   — no callback, or not a function.
 * - `AlreadyStarted` — a capture is already running in this process.
 * - `ThreadStartFailed` — the producer thread could not be created.
 */
export type AudioCapStartFailure =
  'NoBackend' | 'BadOptions' | 'BadArguments' | 'AlreadyStarted' | 'ThreadStartFailed';

export type AudioCapStartResult = { ok: true } | { ok: false; reason: AudioCapStartFailure };

/**
 * Begins capture and registers a bare availability signal.
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
 * Stops capture, joins the producer, and discards anything still queued.
 * Synchronous and idempotent.
 */
export function stop(): void;
