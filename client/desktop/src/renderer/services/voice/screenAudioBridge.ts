/**
 * Renderer screen-audio bridge (#3195, ADR-0043; design §4c, §4e, §4f, §6b, §6d).
 *
 * The main-world end of the per-process screen-share audio transport. It adopts
 * the `MessagePort` the preload relay hands over, turns each 3872-byte quantum
 * into an `AudioData`, writes it to a `MediaStreamTrackGenerator`, and returns
 * one unit of credit to the child — in that order, in one synchronous run.
 *
 * WRITE-THEN-ACK, AND WHY THE ORDER IS THE WHOLE POINT (§6d). The credit ack is
 * emitted by the same code that has just handed the samples to the generator.
 * Acking on RECEIPT instead would make the outstanding bound measure the
 * TRANSPORT rather than the CONSUMER: credit would be returned for a quantum
 * nothing had consumed, the child would keep producing, and the excess would
 * pile up on this side where no counter watches it. That is S1's silent-loss
 * mode wearing a counter — the exact failure this transport exists to replace.
 * For the same reason there is NO `await` anywhere between reading the header
 * and writing the `AudioData`: an await there reorders quanta relative to acks
 * and silently converts drop-newest into reorder-newest.
 *
 * ZERO RENDERER ALLOCATION (§4e; T0b measured PASS on Electron 44.1.1). The
 * quantum arrives as one `ArrayBuffer` that already crossed the isolated/main
 * world boundary by TRANSFER, and it leaves the same way: `transfer: [buffer]`
 * makes WebCodecs take ownership instead of copying. MDN states the contract
 * exactly — *"an array of ArrayBuffers that AudioData will detach and take
 * ownership of. If the array contains the ArrayBuffer backing `data`, AudioData
 * will use that buffer directly instead of copying from it"* — which is also
 * what licenses `data` being a VIEW at `HEADER_BYTES` rather than the whole
 * buffer. Passing the whole 3872-byte buffer would feed the 32-byte header into
 * the first eight samples of every quantum. The spec's capped-free-list fallback
 * is deliberately NOT built: T0b passed, so it would be dead code, and with no
 * free list the pool-inflation attack surface never exists.
 *
 * IT LOGS NOTHING. Every value that reaches this file is either user audio or a
 * number an untrusted child chose, so there is no diagnostic worth a sink
 * (`observability.md` principles 1, 2 and 4). Counters are returned from
 * `stats()` instead, where the caller decides what to do with them.
 *
 * NO WATCHDOG LEASE TO RENEW. The design left Task 6 to choose between renewing
 * `audiocapHost`'s lease from this ack and renewing it from a child heartbeat on
 * `parentPort`; #3195 answered neither, because the rail was deleted rather than
 * shipped inert — it guards a capturing child and this PR forks only main's
 * app-start capability probe. Should #3198 bring the rail back with that child,
 * the argument recorded against this ack still stands: `noteAudiocapCreditAck`
 * lives in the MAIN process, so driving it from here needs a renderer→main IPC
 * channel carried 100 times a second for the life of every share.
 */

import {
  CHANNELS,
  FRAME_COUNT,
  HEADER_BYTES,
  SAMPLE_RATE,
  decodeQuantumHeader,
} from '../../../shared/audiocapProtocol';

/** Interleaved 32-bit float — the one format the wire carries and `rt/` produces. */
const PINNED_FORMAT = 'f32';

/** Samples in one quantum across both channels; the length of the view we hand over. */
const SAMPLES_PER_QUANTUM = FRAME_COUNT * CHANNELS;

/** The header carries nanoseconds; `AudioData.timestamp` is documented in microseconds. */
const NS_PER_US = 1000n;

/**
 * The high-water generation any bridge has ever been created for.
 *
 * Module-scoped because the fence has to outlive the bridge it fences: a stale
 * bridge learns it was superseded from the ACT of a newer one being created, and
 * there is nothing else in the renderer that both instances can see. It is
 * monotone and `stop()` never rewinds it — a rewind would un-fence a stale
 * bridge the moment its successor shut down, which is the ABA shape this exists
 * to prevent. Generations are minted by `audiocapHost` in the main process, so
 * this side only ever records them.
 */
let currentGeneration = 0;

export interface ScreenAudioBridgeStats {
  /**
   * The highest `overrunTotal` the child has reported — observer 2 of §4f.
   *
   * It is stamped at CAPTURE time, so quanta already queued carry the older
   * count and this value lags a live overrun by up to `CREDIT_BOUND` quanta.
   * That lag is why it cannot be the only witness.
   */
  overrun: number;
  /**
   * Quanta missing from the `seq` sequence — observer 1 of §4f.
   *
   * Derived from the stream itself, so it still moves when `overrunTotal` is
   * hardcoded, stale, or lying. The two observers fail in opposite directions;
   * §4f requires both, or neither is evidence.
   */
  seqGaps: number;
  /** True once the port has been closed for a protocol or generator fault. */
  faulted: boolean;
}

export interface ScreenAudioBridge {
  /** The audio track to publish. Owned by the caller's capture stream, not by this bridge. */
  readonly track: MediaStreamTrack;
  stop(): void;
  stats(): ScreenAudioBridgeStats;
}

/**
 * `MediaStreamTrackGenerator` is Chromium-only and absent from TypeScript's DOM
 * library, so it is declared here rather than in `types/` — this is its only
 * consumer, and a global declaration would advertise it as ambient.
 */
interface AudioTrackGenerator extends MediaStreamTrack {
  readonly writable: WritableStream<AudioData>;
}

type AudioTrackGeneratorConstructor = new (init: { kind: 'audio' }) => AudioTrackGenerator;

/**
 * Build the bridge for one capture generation.
 *
 * Throws when the shell cannot support per-process audio at all — an older shell
 * whose preload predates the relay, or an engine without
 * `MediaStreamTrackGenerator`. Fail closed and let the caller go video-only
 * (C9): a missing capability must never degrade to the system mix.
 */
export function createScreenAudioBridge(generation: number): ScreenAudioBridge {
  // Recorded before anything that can throw: a newer generation supersedes an
  // older bridge whether or not this one manages to start, because the host has
  // already killed the child the older bridge was reading from.
  if (generation > currentGeneration) currentGeneration = generation;

  // The three throws below are TypeError, not Error (typescript:S7786). That was
  // free to change here only because #3195 ships this module dark — it has no
  // production caller yet, so nothing exists to discriminate on the constructor.
  // #3196-#3198 give it one. Any catch written then must branch on TypeError (or
  // on `instanceof Error`, which still matches); a `err.constructor === Error`
  // check would silently never fire. The contract moved ahead of its first
  // consumer, which is exactly when it is easy to miss.
  //
  // C4 feature detection: the PRESENCE of the function is the capability probe,
  // its VALUE is the discriminator. Never a hardcoded tag — a literal would
  // silently keep matching a tag the preload had moved on from, and there is no
  // second surface for this bridge to fall back to.
  const getPortMessageTag = globalThis.electron?.audiocap?.getPortMessageTag;
  if (typeof getPortMessageTag !== 'function') {
    throw new TypeError('screen-audio relay unavailable');
  }
  const tag = getPortMessageTag();
  if (typeof tag !== 'string' || tag.length === 0) {
    throw new TypeError('screen-audio relay unavailable');
  }

  const Generator = (globalThis as { MediaStreamTrackGenerator?: AudioTrackGeneratorConstructor })
    .MediaStreamTrackGenerator;
  if (typeof Generator !== 'function') {
    throw new TypeError('screen-audio generator unavailable');
  }

  const generator = new Generator({ kind: 'audio' });
  // Taken once. `getWriter()` locks the stream, so a per-quantum call would
  // throw on the second quantum.
  const writer = generator.writable.getWriter();

  let port: MessagePort | null = null;
  let stopped = false;
  let faulted = false;
  let overrun = 0;
  let seqGaps = 0;
  let lastSeq: number | null = null;

  function closePort(): void {
    if (port === null) return;
    port.onmessage = null;
    port.close();
    port = null;
  }

  /**
   * Terminal. Reached by a non-`ArrayBuffer` message, a header that does not
   * decode, or a generator that refuses the write.
   *
   * All three are unrecoverable: the protocol has no repair path (`null` from
   * `decodeQuantumHeader` means close the port), and a `WritableStream` error is
   * terminal by definition, so a generator that rejected one quantum will never
   * accept another. Closing the port stalls the child on credit rather than
   * leaving it feeding a track that discards everything.
   */
  function failClosed(): void {
    // A STOPPED bridge cannot fault. `handleQuantum` fires `writer.write()`
    // without awaiting it (§6d requires no await between header and write), and
    // `stop()` calls `writer.releaseLock()` while such a write may still be in
    // flight -- so a write that rejects AFTER a deliberate stop would otherwise
    // land here and set `faulted`. `stats()` is public and readable after
    // `stop()`, so that turns a clean teardown into a reported fault.
    //
    // Skipping `closePort()` on this path is safe rather than a shortcut:
    // `stop()` already called it, and it is a no-op once `port` is null.
    //
    // Found by Gitar on PR #3245 and confirmed at source before being fixed.
    if (stopped) return;
    faulted = true;
    closePort();
  }

  /**
   * Count quanta the stream itself says are missing.
   *
   * `seq` is u32 and WRAPS, so the delta is taken with `>>> 0` and
   * `0xFFFFFFFF → 0` reads as a delta of 1, not as 4.29 billion missing quanta.
   * A delta of 0 (a repeat) adds nothing.
   */
  function noteSeq(seq: number): void {
    if (lastSeq !== null) {
      const delta = (seq - lastSeq) >>> 0;
      if (delta > 1) seqGaps += delta - 1;
    }
    lastSeq = seq;
  }

  function handleQuantum(adopted: MessagePort, data: unknown): void {
    // §6b fence, FIRST: a quantum from a superseded generation is discarded —
    // not written, and NOT acked. Acking it would hold a dead child's credit
    // window open and let it keep producing into a track nobody consumes.
    if (currentGeneration > generation) return;

    // §4b puts the `instanceof` check on the port boundary, which is here;
    // `decodeQuantumHeader` deliberately does not repeat it. Unlike the preload
    // relay — which needs a brand check because its harness delivers cross-realm
    // buffers — a transfer into the main world materialises the buffer in THIS
    // realm, so `instanceof` is exact.
    if (!(data instanceof ArrayBuffer)) {
      failClosed();
      return;
    }

    const header = decodeQuantumHeader(data);
    if (header === null) {
      failClosed();
      return;
    }

    noteSeq(header.seq);
    // Monotone-non-decreasing per §4c. A regression is clamped rather than
    // treated as a fault: this field is diagnostic, it steers no code path, and
    // closing the port over a bad counter would cost the user their audio.
    if (header.overrunTotal > overrun) overrun = header.overrunTotal;

    // ---- No `await` from here to the ack. See the module header (§6d). ----
    let audioData: AudioData | null = null;
    try {
      audioData = new AudioData({
        format: PINNED_FORMAT,
        sampleRate: SAMPLE_RATE,
        numberOfFrames: FRAME_COUNT,
        numberOfChannels: CHANNELS,
        timestamp: Number(header.captureTimestampNs / NS_PER_US),
        // The view starts past the header; `transfer` names its BACKING buffer,
        // which is what makes this a hand-over rather than a 3840-byte copy.
        data: new Float32Array(data, HEADER_BYTES, SAMPLES_PER_QUANTUM),
        transfer: [data],
      });
      // Not awaited, by requirement. A rejection still has to be observed: an
      // unhandled one is invisible, and the stream it came from is already dead.
      void Promise.resolve(writer.write(audioData)).catch(failClosed);
    } catch {
      // A leaked `AudioData` pins a decoder buffer, so it is closed on every
      // path where the generator did not take it.
      audioData?.close();
      failClosed();
      return;
    }

    // Write, THEN ack — minted here, never forwarded, so the child only ever
    // receives a value this bridge constructed.
    adopted.postMessage({ c: 1 });
  }

  function adoptPort(candidate: MessagePort | undefined, handoffGeneration: unknown): void {
    if (candidate === undefined) return;
    // A handoff stamped with ANOTHER generation is addressed to a DIFFERENT
    // bridge, and is not this one's to destroy. Leave it entirely alone.
    //
    // This used to fall into the `candidate.close()` below, which was a real bug
    // with no attacker in it (PR #3245 red-team, VULN-2). The handoff is a
    // `window` message BROADCAST, so every live bridge sees every handoff; a
    // bridge that has not been `stop()`ped when the host mints generation N+1
    // therefore destroyed the successor's port before the successor's own
    // listener ran -- listener registration order guarantees the older bridge
    // goes first. `close()` disentangles the pair, so the relay's end died too:
    // no audio, `faulted === false`, and nothing logged on either side by
    // design. The same silent shape the `windowLoaded` note guards against,
    // reached through a different door.
    if (handoffGeneration !== generation) return;
    // A duplicate for THIS generation, or one arriving after `stop()`, IS ours:
    // nobody else will adopt it, so close it rather than leak it. A second port
    // would otherwise leave two feeding one track.
    if (port !== null || stopped) {
      candidate.close();
      return;
    }
    port = candidate;
    candidate.onmessage = (event: MessageEvent<unknown>): void => {
      handleQuantum(candidate, event.data);
    };
    candidate.start();
  }

  const onWindowMessage = (event: MessageEvent<unknown>): void => {
    // VERIFY THE SENDER BEFORE READING THE PAYLOAD (SonarQube S2819, CWE-345).
    //
    // The handoff is posted by THIS document's preload onto THIS window, so the
    // only legitimate sender is the window itself. A `message` event whose
    // `source` is anything else came from another browsing context -- an iframe,
    // an opener, a popup -- and has no business handing this bridge a port that
    // terminates at the process which loaded native code.
    //
    // `source` rather than `origin` is the load-bearing half, and the reason is
    // the same WHATWG rule that shaped the relay's `handoverTargetOrigin`: the
    // bundled origins `app://concord` and `spa-cache://concord` are non-special
    // schemes whose origin serializes to the literal string "null". Comparing
    // origins there compares "null" to "null", which every opaque origin
    // satisfies -- so an origin test alone would admit exactly the cross-context
    // sender it is meant to exclude, while ALSO risking a false reject on the
    // remote SPA. Identity of the window object has no such degenerate case.
    //
    // WHAT THIS DOES NOT CLAIM. It does not defend against a compromised MAIN
    // WORLD in this same document: that code can forge a handoff, and it already
    // holds `voiceService` outright, so there is nothing left to protect. The
    // discriminator `tag` is a public constant by construction (`contextBridge`
    // hands it to any caller), so no secret can help here. This narrows the
    // sender set to one window; it is not authentication.
    // TWO CHECKS, AND THEY DO DIFFERENT WORK. Both must hold.
    //
    // 1. ORIGIN. `event.origin` is the SENDER's origin, and preload posts from
    //    this same document, so it must equal ours. This is the check S2819
    //    names, and it is the one that carries weight on the REMOTE SPA, where
    //    the origin is a real `https://` value a different document could not
    //    forge.
    // 2. SOURCE IDENTITY. Strict `=== window`, and `null` is NOT accepted (that
    //    is what a hand-constructed `MessageEvent` dispatched by page script
    //    carries).
    //
    // Neither subsumes the other, which is why both are here rather than one.
    // On the bundled origins the ORIGIN check degenerates: `app://concord` and
    // `spa-cache://concord` are non-special schemes whose origin serializes to
    // the literal string "null", so the comparison is "null" === "null", which
    // ANY opaque origin satisfies. There the source check is doing all the work.
    // Conversely a same-document attacker can dispatch an event carrying our own
    // origin string, and only the source check refuses that.
    if (event.origin !== globalThis.location.origin) return;
    // `window` here, unlike the origin check above, because `globalThis` does not
    // typecheck against `MessageEventSource` (TS2367 — lib.dom merges `Window` into
    // the `window` declaration, not into `typeof globalThis`). The only way to the
    // globalThis spelling is a double cast, and blinding the compiler on the one
    // check that refuses a same-document attacker is not worth the consistency.
    if (event.source !== window) return;
    const data: unknown = event.data;
    if (typeof data !== 'object' || data === null) return;
    if ((data as Record<string, unknown>)[tag] !== true) return;
    const candidate: MessagePort | undefined = event.ports[0];
    adoptPort(candidate, (data as Record<string, unknown>).generation);
  };

  // Registered before returning, so a handoff that lands in the same task as
  // construction is not missed.
  globalThis.addEventListener('message', onWindowMessage);

  return {
    track: generator,
    stop(): void {
      if (stopped) return;
      stopped = true;
      globalThis.removeEventListener('message', onWindowMessage);
      closePort();
      writer.releaseLock();
      generator.stop();
    },
    stats(): ScreenAudioBridgeStats {
      return { overrun, seqGaps, faulted };
    },
  };
}
