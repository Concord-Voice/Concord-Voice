/**
 * Shared audiocap wire protocol (#3195, ADR-0043).
 *
 * The one module every leg of the transport sees. `tsconfig.main.json`
 * compiles `src/main/**` + `src/shared/**`; the renderer leg compiles all of
 * `src/`. So main, the utilityProcess child, the preload relay and the
 * renderer bridge all narrow child-originated messages with the SAME
 * predicates, and there is exactly one definition of the 32-byte quantum
 * header rather than one per consumer that can drift.
 *
 * Consumed by:
 *   - src/main/audiocapHost.ts   — `parentPort` control messages
 *   - src/main/audiocapChild.ts  — header encode, control messages
 *   - src/preload/preload.ts     — per-quantum validation before relay
 *   - src/renderer/services/voice/screenAudioBridge.ts — header decode
 *
 * TRUST BOUNDARY (design section 7). Everything the child sends is untrusted.
 * Three things cross and NONE of them is authorization:
 *   - `capability.perProcessAudio` — a *necessary input* main ANDs with facts
 *     it owns (platform, packaged-ness, the resolved addon path). Never a
 *     grant by itself.
 *   - `capability.platform` / `osVersion` / `reason` — diagnostics only,
 *     length-capped and charset-restricted here, before any sink.
 *   - quantum header fields — arithmetic only. No field selects a code path
 *     other than accept-or-drop.
 *
 * Deliberately NO schema library. `src/shared/**` compiles into the main leg,
 * so a runtime dependency here widens the main-process dependency surface for
 * a handful of hand-writable narrowings (design section 4a).
 */

// ---------------------------------------------------------------------------
// Fixed wire constants
// ---------------------------------------------------------------------------

/** Version of the `parentPort` CONTROL channel, carried by `hello`. */
export const AUDIOCAP_PROTOCOL = 1;

/** header(32) + 480 frames x 2 channels x 4 bytes of interleaved f32 = 3872. */
export const QUANTUM_BYTES = 3872;

export const HEADER_BYTES = 32;

export const MAGIC = 0xca57;

/**
 * Pinned, not negotiated: the encoder is Opus with `opusStereo: true` and the
 * repo standardises on `new AudioContext({ sampleRate: 48000 })`. NOTHING
 * RESAMPLES AND NOTHING DOWNMIXES, in `rt/` or anywhere else — a source that is
 * not 48 kHz binary32 mono/stereo is refused with `UnsupportedFormat`
 * (`rt::acceptsSourceFormat`, `rt/capture_backend.h`). Mono is duplicated to
 * stereo by a byte shuffle, which is not a downmix.
 */
export const SAMPLE_RATE = 48000;
export const CHANNELS = 2;
export const FRAME_COUNT = 480;

/** One quantum is 480 frames at 48 kHz. */
export const QUANTUM_MS = 10;

/**
 * `CREDIT_BOUND === RING_SLOTS` on purpose (~80 ms of audio). The child drains
 * only while `outstanding < CREDIT_BOUND`; when it stops draining, `rt/` fills
 * the ring and drops. Credit exhaustion and ring overflow are therefore the
 * same event with a single drop site, counted once (design section 4d).
 */
export const CREDIT_BOUND = 8;
export const RING_SLOTS = 8;

/** Ceiling on child spawn to `hello`; past it the host kills and goes video-only. */
export const HANDSHAKE_TIMEOUT_MS = 10000;

/** Cap for every diagnostic string on a `hello` (design section 4a). */
export const DIAGNOSTIC_MAX_CHARS = 64;

/** Cap applied to a `fault` message at the sink, which truncates rather than rejects. */
export const FAULT_MESSAGE_MAX_CHARS = 200;

/**
 * Wire-format version in the header's second byte. Distinct from
 * `AUDIOCAP_PROTOCOL`, which versions the control channel, even though both
 * read 1 today — the two channels can version independently. Module-private
 * because `encodeQuantumHeader` is the only writer and `decodeQuantumHeader`
 * the only reader: an unknown version has exactly one handling (close the
 * port) and must never become a branch a child can steer.
 */
const HEADER_VERSION = 1;

/**
 * Bound on `hello.envKeys`. The host's `ENV_ALLOWLIST` is six entries plus
 * `CONCORD_AUDIOCAP_PATH`; the headroom is deliberate, because this is a
 * denial-of-service bound on an attacker-controlled array length, not an
 * assertion about the allowlist's current contents.
 */
const ENV_KEYS_MAX = 16;

/**
 * Charset for every diagnostic string that crosses from the child (C8
 * principle 5). Non-global on purpose: a `/g` regex carries `lastIndex`
 * between `.test()` calls, so the same input would alternate true/false.
 */
const DIAGNOSTIC_CHARSET = /^[A-Za-z0-9. _-]*$/;

/** The same charset as a strip set, for the sanitiser. `/g` is correct for `.replace`. */
const DIAGNOSTIC_STRIP = /[^A-Za-z0-9. _-]/g;

/**
 * Header field offsets, little-endian. One table read by both the encoder and
 * the decoder so the two cannot drift apart by eye (design section 4c).
 */
const OFFSET = {
  magic: 0,
  version: 2,
  flags: 3,
  seq: 4,
  sampleRate: 8,
  channels: 12,
  frameCount: 14,
  captureTimestampNs: 16,
  overrunTotal: 24,
  reserved: 28,
} as const;

// ---------------------------------------------------------------------------
// `parentPort` control messages — main <-> child, control only, never PCM
// ---------------------------------------------------------------------------

/**
 * The closed set of stages a `fault` may name. Closed-ness IS the enforcement
 * that this channel does not become a general RPC surface; a comment is not
 * enforcement (design section 4a).
 */
export type AudiocapFaultStage = 'guard' | 'load' | 'capability' | 'start' | 'protocol';

const AUDIOCAP_FAULT_STAGES: readonly string[] = [
  'guard',
  'load',
  'capability',
  'start',
  'protocol',
];

export interface AudiocapCapability {
  platform: string;
  osVersion: string;
  perProcessAudio: boolean;
  reason: string;
}

export interface AudiocapHello {
  kind: 'hello';
  protocol: typeof AUDIOCAP_PROTOCOL;
  capability: AudiocapCapability;
  resourcesPathPresent: boolean;
  envKeys: string[];
}

export interface AudiocapFault {
  kind: 'fault';
  stage: AudiocapFaultStage;
  message: string;
}

export interface AudiocapStart {
  kind: 'start';
  quantumMs: typeof QUANTUM_MS;
  sampleRate: typeof SAMPLE_RATE;
  channels: typeof CHANNELS;
  frameCount: typeof FRAME_COUNT;
  creditBound: typeof CREDIT_BOUND;
  ringSlots: typeof RING_SLOTS;
}

export interface AudiocapStop {
  kind: 'stop';
}

/**
 * The whole message set. There is no `pause`, `resume`, `getStats` or
 * `setSource`, and no "unknown message ignored" branch anywhere: a message
 * that matches none of these means kill the child.
 */
export type AudiocapControlMessage = AudiocapHello | AudiocapFault | AudiocapStart | AudiocapStop;

export interface QuantumHeader {
  seq: number;
  captureTimestampNs: bigint;
  overrunTotal: number;
}

// ---------------------------------------------------------------------------
// Narrowing predicates
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

/**
 * A diagnostic string is length-capped AND charset-restricted before it is
 * allowed to exist, so nothing downstream has to remember to sanitise it. The
 * length test runs first so the regex only ever sees a bounded string.
 */
function isDiagnosticString(v: unknown): v is string {
  return typeof v === 'string' && v.length <= DIAGNOSTIC_MAX_CHARS && DIAGNOSTIC_CHARSET.test(v);
}

function isDiagnosticStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.length <= ENV_KEYS_MAX && v.every(isDiagnosticString);
}

function isAudiocapCapability(v: unknown): v is AudiocapCapability {
  return (
    isRecord(v) &&
    isDiagnosticString(v.platform) &&
    isDiagnosticString(v.osVersion) &&
    // Strict `typeof === 'boolean'`, so a truthy `1` from the child is
    // REJECTED rather than coerced. This field is a necessary input that main
    // ANDs with facts it owns; it is never authorization, and treating a
    // coerced truthy value as `true` here would be #2161's defect arriving on
    // a different channel (design section 7).
    typeof v.perProcessAudio === 'boolean' &&
    isDiagnosticString(v.reason)
  );
}

/**
 * Written as one `&&` chain rather than a ladder of early returns because a
 * `hello` is accept-or-kill: there is no partial acceptance to express, and no
 * field whose failure deserves its own handling.
 */
export function isAudiocapHello(v: unknown): v is AudiocapHello {
  return (
    isRecord(v) &&
    v.kind === 'hello' &&
    v.protocol === AUDIOCAP_PROTOCOL &&
    isAudiocapCapability(v.capability) &&
    typeof v.resourcesPathPresent === 'boolean' &&
    isDiagnosticStringArray(v.envKeys)
  );
}

/**
 * A `fault` message is truncated and stripped at the sink rather than rejected
 * on length — unlike a `hello`'s diagnostics — because a fault is the child's
 * last word before it is killed and losing it to a length check would cost the
 * only explanation of why capture failed. Pass it through
 * `sanitizeDiagnostic(message, FAULT_MESSAGE_MAX_CHARS)` before any sink.
 */
export function isAudiocapFault(v: unknown): v is AudiocapFault {
  return (
    isRecord(v) &&
    v.kind === 'fault' &&
    typeof v.stage === 'string' &&
    AUDIOCAP_FAULT_STAGES.includes(v.stage) &&
    typeof v.message === 'string'
  );
}

// ---------------------------------------------------------------------------
// Quantum header codec
// ---------------------------------------------------------------------------

/**
 * Returns a fully-populated header or `null`. `null` means CLOSE THE PORT —
 * there is no repair path, no "ignore this one", and no partially-populated
 * result: every fixed field is checked before the result object is
 * constructed, so a caller can never receive an object whose later fields were
 * filled from a buffer that failed an earlier check.
 *
 * `instanceof ArrayBuffer` is deliberately NOT re-checked here; the port
 * boundary that receives the transferable owns that check (design section 4b),
 * and duplicating it would put an unreachable branch in the one file whose
 * coverage this PR is measured on.
 */
export function decodeQuantumHeader(buf: ArrayBuffer): QuantumHeader | null {
  if (buf.byteLength !== QUANTUM_BYTES) return null;

  // Bounded to the header, so a mis-offset read throws instead of silently
  // returning PCM bytes reinterpreted as a field.
  const view = new DataView(buf, 0, HEADER_BYTES);

  if (view.getUint16(OFFSET.magic, true) !== MAGIC) return null;
  if (view.getUint8(OFFSET.version) !== HEADER_VERSION) return null;
  if (view.getUint8(OFFSET.flags) !== 0) return null;
  if (view.getUint32(OFFSET.sampleRate, true) !== SAMPLE_RATE) return null;
  if (view.getUint16(OFFSET.channels, true) !== CHANNELS) return null;
  if (view.getUint16(OFFSET.frameCount, true) !== FRAME_COUNT) return null;
  if (view.getUint32(OFFSET.reserved, true) !== 0) return null;

  // The three variable fields are arithmetic and are NOT checked here: `seq`
  // wraps, and both it and `overrunTotal` are judged by the consumer against
  // what it saw last (a gap is a drop witness). Neither steers control flow.
  return {
    seq: view.getUint32(OFFSET.seq, true),
    captureTimestampNs: view.getBigUint64(OFFSET.captureTimestampNs, true),
    overrunTotal: view.getUint32(OFFSET.overrunTotal, true),
  };
}

/**
 * Writes the 32-byte header in place at the front of `into`. Every fixed field
 * comes from this module's constants, never from an argument, so the only
 * things a caller can vary are the three the decoder treats as arithmetic.
 * Throws if `into` is shorter than the header — the encoder is a local
 * operation on a buffer we allocated, so a wrong size there is a programming
 * error rather than a protocol event.
 */
export function encodeQuantumHeader(into: ArrayBuffer, h: QuantumHeader): void {
  const view = new DataView(into, 0, HEADER_BYTES);

  view.setUint16(OFFSET.magic, MAGIC, true);
  view.setUint8(OFFSET.version, HEADER_VERSION);
  view.setUint8(OFFSET.flags, 0);
  view.setUint32(OFFSET.seq, h.seq, true);
  view.setUint32(OFFSET.sampleRate, SAMPLE_RATE, true);
  view.setUint16(OFFSET.channels, CHANNELS, true);
  view.setUint16(OFFSET.frameCount, FRAME_COUNT, true);
  view.setBigUint64(OFFSET.captureTimestampNs, h.captureTimestampNs, true);
  view.setUint32(OFFSET.overrunTotal, h.overrunTotal, true);
  view.setUint32(OFFSET.reserved, 0, true);
}

// ---------------------------------------------------------------------------
// Diagnostics
// ---------------------------------------------------------------------------

/**
 * Strips everything outside the diagnostic charset, then caps at `max`.
 *
 * It takes a `string` and not an `Error` or an `unknown` ON PURPOSE: that
 * signature is the mechanism by which `Error.cause` has no path to a log sink
 * (observability principle 3, C8). The loader sets a `cause` carrying the
 * underlying load failure, and a sanitiser that accepted an `Error` would
 * eventually be handed one and asked to be clever about it. Callers pass
 * `err.message`, which is the whole of what may be logged.
 *
 * `Math.max`/`Math.trunc` rather than a guard on `max`: a non-finite or
 * negative cap collapses to an empty string instead of `slice`'s
 * count-from-the-end behaviour, and it does so without a branch.
 */
export function sanitizeDiagnostic(s: string, max: number): string {
  const limit = Math.max(0, Math.trunc(max));
  return s.replace(DIAGNOSTIC_STRIP, '').slice(0, limit);
}
