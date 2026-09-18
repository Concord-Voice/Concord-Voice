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

/**
 * THE HANDOFF CAN LAND BEFORE ITS BRIDGE EXISTS, so the listener cannot live on the
 * bridge (#3198 PR 3).
 *
 * Until PR 3 nothing started a capture, so no handoff was ever posted and the listener
 * `createScreenAudioBridge` installed for itself was sufficient by vacuity. With the
 * capture leg live the ordering is decided in the MAIN process and loses: main posts the
 * port to preload inside the same turn that settles the `audiocap:start` promise, and the
 * renderer can only construct a bridge after that promise crosses IPC and resolves. The
 * relay hands the main world its end with ONE `postMessage` and never repeats it — see
 * `audiocapRelay.ts`, "Hand the main world its end exactly once" — so a handoff with no
 * listener is not delayed, it is destroyed.
 *
 * This is the `windowLoaded` trap from `[internal]rules/electron.md` § "IPC contract v27",
 * one layer up: *"Deferring construction until the IPC message with the port arrives
 * races app startup … the symptom is an intermittent, silent 'screen share has no audio'
 * with nothing in any log."* That note is written about preload; the identical shape is
 * reachable here, and the remedy it prescribes is the one taken below — register at
 * MODULE SCOPE, before anything can arrive.
 *
 * A second property falls out for free, and it retires a real bug rather than a
 * hypothetical one. The handoff is a `window` message, so a per-bridge listener made it a
 * BROADCAST every live bridge saw — which is what let a not-yet-stopped bridge destroy its
 * successor's port (PR #3245 red-team, VULN-2, recorded at `adoptPort` below). Dispatching
 * by generation means a bridge is only ever offered its own port, so that collision is
 * unrepresentable instead of guarded against.
 */
const awaitingPort = new Map<number, (port: MessagePort) => void>();

/**
 * Ports that arrived before their bridge registered, keyed by generation.
 *
 * BOUNDED, because this map is fed by a message the renderer does not initiate. One share
 * is live at a time and a superseded generation's port is closed the moment a newer bridge
 * registers, so the steady state is 0 or 1; the cap only bounds a pathological producer.
 */
const unclaimedPorts = new Map<number, MessagePort>();
const UNCLAIMED_PORT_LIMIT = 4;

function dropUnclaimed(generation: number): void {
  const stale = unclaimedPorts.get(generation);
  if (stale === undefined) return;
  unclaimedPorts.delete(generation);
  stale.close();
}

/**
 * Route a verified handoff to its bridge, or hold it until that bridge registers.
 *
 * Holding is the whole point: a port with no claimant is the one case the old per-bridge
 * listener could not represent, because a listener that does not exist cannot buffer.
 */
function deliverHandoff(generation: number, port: MessagePort): void {
  const claim = awaitingPort.get(generation);
  if (claim !== undefined) {
    claim(port);
    return;
  }

  // A duplicate for a generation already waiting unclaimed. Close the older one rather
  // than leak it — two ports feeding one track is the failure `adoptPort` already refuses.
  dropUnclaimed(generation);

  if (unclaimedPorts.size >= UNCLAIMED_PORT_LIMIT) {
    // Drop the OLDEST. `Map` preserves insertion order, so the first key is the stalest
    // claim, and a stale claim is the one least likely to still have a bridge coming.
    const oldest = unclaimedPorts.keys().next();
    if (!oldest.done) dropUnclaimed(oldest.value);
  }
  unclaimedPorts.set(generation, port);
}

/**
 * The tag, resolved per message rather than captured once.
 *
 * `createScreenAudioBridge` THROWS when the bridge is missing, because there the absence
 * is a capability answer the caller must act on. Here it is not: this listener runs from
 * module evaluation, which can precede anything, and a throw would escape into an
 * unrelated `message` dispatch. Returning `null` simply declines the message.
 */
function portMessageTag(): string | null {
  const getPortMessageTag = globalThis.electron?.audiocap?.getPortMessageTag;
  if (typeof getPortMessageTag !== 'function') return null;
  const tag = getPortMessageTag();
  return typeof tag === 'string' && tag.length > 0 ? tag : null;
}

/**
 * The ONE handoff listener, installed at module evaluation.
 *
 * Its security checks are unchanged from the per-bridge listener this replaces, and both
 * are still required — see the long note that used to sit here, preserved verbatim:
 *
 * 1. ORIGIN. `event.origin` is the SENDER's origin, and preload posts from this same
 *    document, so it must equal ours. This is the check SonarQube S2819 names, and it
 *    carries weight on the REMOTE SPA where the origin is a real `https://` value another
 *    document could not forge.
 * 2. SOURCE IDENTITY. Strict `=== window`, and `null` is NOT accepted — that is what a
 *    hand-constructed `MessageEvent` dispatched by page script carries.
 *
 * Neither subsumes the other. On the bundled origins `app://concord` and
 * `spa-cache://concord` the ORIGIN check degenerates: they are non-special schemes whose
 * origin serializes to the literal string "null", which every opaque origin satisfies, so
 * there the source check does all the work. Conversely a same-document attacker can
 * dispatch an event carrying our own origin string, and only the source check refuses it.
 *
 * WHAT THIS DOES NOT CLAIM. It does not defend against a compromised MAIN WORLD in this
 * document: that code can forge a handoff, and it already holds `voiceService` outright.
 * The discriminator is a public constant by construction (`contextBridge` hands it to any
 * caller), so no secret can help. This narrows the sender set to one window; it is not
 * authentication.
 */
globalThis.addEventListener('message', (event: MessageEvent<unknown>): void => {
  if (event.origin !== globalThis.location.origin) return;
  // `window`, not `globalThis`: the latter does not typecheck against
  // `MessageEventSource` (TS2367 — lib.dom merges `Window` into the `window` declaration,
  // not into `typeof globalThis`), and blinding the compiler with a double cast on the one
  // check that refuses a same-document attacker is not worth the consistency.
  if (event.source !== window) return;

  const data: unknown = event.data;
  if (typeof data !== 'object' || data === null) return;

  const tag = portMessageTag();
  if (tag === null) return;
  if ((data as Record<string, unknown>)[tag] !== true) return;

  const port: MessagePort | undefined = event.ports[0];
  if (port === undefined) return;

  const generation = (data as Record<string, unknown>).generation;
  // The generation is chosen by main and mirrored back by preload; a handoff that does not
  // carry a real one cannot be routed to any bridge, so it is declined rather than guessed.
  if (typeof generation !== 'number' || !Number.isInteger(generation) || generation < 0) {
    port.close();
    return;
  }

  deliverHandoff(generation, port);
});

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
    // DEFENCE IN DEPTH SINCE #3198 PR 3, not the live fence it used to be. Dispatch
    // is now keyed by generation in `deliverHandoff`, so a bridge is only ever
    // offered its own port and this branch is unreachable from the module listener.
    // It stays because the check is one comparison and its absence was once a real
    // defect — the history below is why.
    //
    // This used to fall into the `candidate.close()` below, which was a real bug
    // with no attacker in it (PR #3245 red-team, VULN-2). The handoff was a
    // `window` message BROADCAST and every bridge installed its own listener, so
    // every live bridge saw every handoff; a
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

  // REGISTER, then DRAIN. Both halves are required and the order is the design: the
  // handoff for this generation may already be sitting unclaimed (main posts it inside the
  // turn that settles `audiocap:start`, which is strictly before this constructor can run),
  // or it may still be in flight. Registering first means an in-flight one is routed the
  // moment it lands; draining second means an already-arrived one is picked up now.
  //
  // Registering AFTER the drain would reopen the race in miniature -- a handoff landing
  // between the two statements would find no claimant and be buffered, with nothing left to
  // drain it until the next bridge for the same generation, which never comes.
  awaitingPort.set(generation, (handed: MessagePort): void => {
    adoptPort(handed, generation);
  });

  // Anything older than this bridge is superseded and will never be claimed: the host has
  // already killed the child it belonged to. Closing it here is what keeps `unclaimedPorts`
  // at 0 or 1 in steady state rather than relying on the cap.
  //
  // Iterating the live key set rather than a copy is safe HERE and only here:
  // `dropUnclaimed` deletes exactly the key it is handed, and a Map iterator
  // that has already yielded an entry is unaffected by that entry's removal.
  // Widening the loop body to delete some OTHER generation would break that.
  for (const pending of unclaimedPorts.keys()) {
    if (pending < generation) dropUnclaimed(pending);
  }

  const buffered = unclaimedPorts.get(generation);
  if (buffered !== undefined) {
    unclaimedPorts.delete(generation);
    adoptPort(buffered, generation);
  }

  return {
    track: generator,
    stop(): void {
      if (stopped) return;
      stopped = true;
      // A TOMBSTONE CLAIM, NOT A DELETION — and the difference is the whole fix (Gitar
      // review, PR #3349). The listener is module-scoped and outlives every bridge, so a
      // handoff for this generation can still land after `stop()` returns; teardown
      // racing the handoff is exactly the case. Deleting the claim sent that port to
      // `deliverHandoff`'s no-claimant branch, which BUFFERS it — held open until some
      // later bridge's supersession sweep or until `UNCLAIMED_PORT_LIMIT` evicts it.
      //
      // Gitar proposed `dropUnclaimed(generation)` here instead. MEASURED: that is inert
      // for its own scenario, because `stop()` runs BEFORE the late port arrives and the
      // buffer is empty at this moment. A claim that closes on arrival is what actually
      // closes it, and it removes the dependence on a later bridge ever existing.
      //
      // It removes itself, so the map holds at most one dead entry per stopped
      // generation and nothing at all once the late port lands. A generation is unique
      // per share, so this can never shadow a live claim.
      awaitingPort.set(generation, (late: MessagePort): void => {
        awaitingPort.delete(generation);
        late.close();
      });
      // The other ordering: a port already buffered for this generation when `stop()`
      // runs. Reachable only via a duplicate handoff — `deliverHandoff` buffers the
      // second of two for one generation — but it costs one call to cover.
      dropUnclaimed(generation);
      closePort();
      writer.releaseLock();
      generator.stop();
    },
    stats(): ScreenAudioBridgeStats {
      return { overrun, seqGaps, faulted };
    },
  };
}
