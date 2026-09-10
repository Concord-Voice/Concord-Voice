// Task 6 contract tests for the renderer screen-audio bridge (#3195, ADR-0043;
// design §4e, §4f, §6d). `src/renderer/services/voice/screenAudioBridge.ts`
// does not exist yet — this file is written test-first and MUST fail only on
// module resolution until Task 6 lands the module.
//
// CORRECTED INPUT CONTRACT (plan 2026-09-10, after Task 5 landed). There is no
// `window.concord`; this repo's `contextBridge` key is `electron`, and a
// `MessagePort` cannot cross it (C2). The real handoff Task 5 shipped:
//   1. Feature-detect AND obtain the discriminator via
//      `window.electron.audiocap.getPortMessageTag()` — presence is the C4
//      `typeof fn !== 'function'` check, the value is the tag.
//   2. Listen for a `window` `message` whose `data[tag] === true`.
//   3. Take `event.ports[0]`; read `generation` off the same `data`.
// This file drives that contract, not the stale `onPort` shape the plan's
// Task 6 section originally (and wrongly) described.
//
// TWO jsdom GAPS REUSED FROM `tests/unit/preload/audiocapRelay.test.ts` (its
// header documents both; not rediscovered here):
//   1. `window.postMessage` in jsdom is arity-2 and silently drops the
//      transfer list (`Window.js` carries a literal `// TODO: event.ports`),
//      so `event.ports[0]` is always empty without a shim. This file patches
//      `window.postMessage` the same way the relay test does: rebuild a real
//      `MessageEvent` and attach `ports` with `Object.defineProperty`.
//   2. jsdom implements no `MessageChannel`, so a real one falls through to
//      Node's `worker_threads` pair and delivers CROSS-REALM `ArrayBuffer`s
//      that fail `instanceof`. This file sidesteps the gap entirely rather
//      than re-hitting it: the "port" handed to the bridge in the handoff
//      event is a hand-rolled fake (`onmessage` property + `postMessage`/
//      `start`/`close` spies), not a real `MessagePort`. That is also what
//      makes `deliver()` synchronous below (see next paragraph) — a real
//      `MessagePort.postMessage` is inherently task-queued in every
//      implementation this repo runs against, jsdom included, and §6d's
//      "no await between header and `AudioData`" claim is unwritable against
//      an transport that itself imposes a microtask boundary.
//
// WHY `deliver()` IS SYNCHRONOUS. The fake port's `onmessage` is a plain
// property the bridge assigns once at adoption. `deliver()` calls it
// directly — no `postMessage` round trip — so a test can fire N quanta with
// no `await` between them and assert on `written`/`ackOrder` immediately
// afterward. That is the only way to make §6d's synchronous-pipeline
// requirement a positive, checkable assertion rather than a timing hope.
//
// STUBS CHOSEN FOR `AudioData` / `MediaStreamTrackGenerator` (jsdom has
// neither). `AudioData` is stubbed as a plain class recording exactly the
// `AudioDataInit` fields this suite asserts on (`format`, `sampleRate`,
// `numberOfFrames`, `numberOfChannels`, `timestamp`) plus a `closed` flag
// flipped by `close()` — enough to prove the pinned format and the
// close-on-throw path without modelling frame data or `transfer` detachment
// (T0b already proved detachment at the platform level; re-proving it here
// would test jsdom, not the bridge). `MediaStreamTrackGenerator` is stubbed
// as a single-writer object exposing `writable.getWriter()`, matching the
// real WebCodecs shape closely enough that a bridge calling
// `generator.writable.getWriter().write(audioData)` exercises the same call
// shape it will make against the real API.
//
// GENERATION FENCING (design §6b). The public surface is
// `createScreenAudioBridge(generation)` with no second export, so "the current
// generation" that fences a stale bridge's quanta must be module-scoped
// renderer state, bumped by the act of creating a newer bridge — the renderer
// cannot reach main-process state without IPC. `bumpCurrentGeneration()` below
// creates and immediately stops a superseding bridge to advance that mark.
//
// CORRECTED 2026-09-10: an earlier revision of this comment cited
// `audiocapHost.ts`'s `currentAudiocapGeneration()` as the precedent and said
// `killAudiocapHost()` "never rewinds the host's own counter". BOTH HALVES ARE
// FALSE — that function is `return session?.generation ?? 0`, deliberately
// LIVENESS-scoped, and it does rewind to 0 on teardown so one comparison can
// reject a continuation both when a newer share superseded it and when the host
// died under it. The requirement below is still right; only its justification
// was wrong, and the bridge must NOT be "corrected" to match the false premise.
// A rewind here would un-fence a stale bridge the instant its successor stopped
// — the ABA shape this fence exists to prevent.
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createScreenAudioBridge } from '../../../../../src/renderer/services/voice/screenAudioBridge';
import { AUDIOCAP_PORT_TAG } from '../../../../../src/preload/audiocapRelay';
import {
  QUANTUM_BYTES,
  HEADER_BYTES,
  encodeQuantumHeader,
} from '../../../../../src/shared/audiocapProtocol';

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/** A conforming quantum at `seq`, with an optionally overridden `overrunTotal`. */
function quantumWithSeq(seq: number, overrunTotal = 0): ArrayBuffer {
  const buf = new ArrayBuffer(QUANTUM_BYTES);
  encodeQuantumHeader(buf, { seq, captureTimestampNs: 1n, overrunTotal });
  return buf;
}

interface FakeAudioDataInit {
  format: string;
  sampleRate: number;
  numberOfFrames: number;
  numberOfChannels: number;
  timestamp: number;
  /** The REAL bridge passes a `Float32Array` VIEW starting past the header. */
  data: Float32Array;
  transfer?: ArrayBuffer[];
}

interface FakeAudioDataInstance {
  readonly format: string;
  readonly sampleRate: number;
  readonly numberOfFrames: number;
  readonly numberOfChannels: number;
  readonly timestamp: number;
  /** First two samples AS WEBCODECS WOULD SEE THEM — see the stub's comment. */
  readonly firstSamples: readonly number[];
  /** Did the backing buffer actually detach? Models `AudioDataInit.transfer`. */
  readonly detached: boolean;
  closed: boolean;
  close(): void;
}

interface FakePort {
  onmessage: ((event: { data: unknown }) => void) | null;
  start: ReturnType<typeof vi.fn>;
  close: ReturnType<typeof vi.fn>;
  postMessage: ReturnType<typeof vi.fn>;
}

function makeFakePort(ackOrder: string[]): FakePort {
  return {
    onmessage: null,
    start: vi.fn(),
    close: vi.fn(),
    postMessage: vi.fn((data: unknown) => {
      const d = data as { c?: unknown } | null;
      if (d && d.c === 1) ackOrder.push('ack');
    }),
  };
}

/** Registered by `bridgeHarness()`; drained by the global `afterEach` below. */
const cleanups: Array<() => void> = [];

afterEach(() => {
  while (cleanups.length > 0) cleanups.pop()!();
});

interface BridgeHarnessOptions {
  generation?: number;
  writeThrows?: boolean;
  /**
   * `write()` returns a promise that stays PENDING until the test rejects it.
   * Distinct from `writeThrows`, which throws synchronously: the case this
   * exists for is a write still in flight when `stop()` runs, which can only
   * be built if the rejection is under the test's control.
   */
  writeDefers?: boolean;
}

async function bridgeHarness(options: BridgeHarnessOptions = {}) {
  const { generation = 1, writeThrows = false, writeDefers = false } = options;
  let rejectPendingWrite: ((e: Error) => void) | null = null;

  const ackOrder: string[] = [];
  const written: FakeAudioDataInstance[] = [];
  let lastAudioData: FakeAudioDataInstance | null = null;

  class FakeAudioData implements FakeAudioDataInstance {
    readonly format: string;
    readonly sampleRate: number;
    readonly numberOfFrames: number;
    readonly numberOfChannels: number;
    readonly timestamp: number;
    readonly firstSamples: readonly number[];
    readonly detached: boolean;
    closed = false;
    constructor(init: FakeAudioDataInit) {
      this.format = init.format;
      this.sampleRate = init.sampleRate;
      this.numberOfFrames = init.numberOfFrames;
      this.numberOfChannels = init.numberOfChannels;
      this.timestamp = init.timestamp;

      // THE STUB DELIBERATELY MODELS TWO THINGS BEYOND THE SCALAR INIT FIELDS,
      // and the reason is a specific pair of surviving mutants (PR #3245).
      //
      // Recording only `format`/`sampleRate`/`numberOfFrames`/... left BOTH of
      // these alive with all eight contract tests green, and they were written
      // off as "needs a real renderer, a display and a darwin runner". They do
      // not. A stub that models the OUTER EFFECT rather than the argument list
      // kills both here, in jsdom:
      //
      //   M1 — a `data` view built at offset 0 instead of HEADER_BYTES. No
      //        length check can catch it (0 + 3840 <= 3872, so nothing throws);
      //        the samples are simply the header bytes reinterpreted as float32.
      //        Recording what WebCodecs would actually read is what separates
      //        them.
      //   M2 — a missing `transfer` list. `expect(init.transfer).toBe(...)` only
      //        proves a value was passed; `structuredClone` with a transfer list
      //        genuinely detaches in Node, so the copy path becomes observable.
      //
      // Read the samples BEFORE transferring: detaching invalidates the view.
      this.firstSamples = [init.data[0] ?? NaN, init.data[1] ?? NaN];

      const backing = init.transfer?.[0];
      if (backing !== undefined) {
        // Genuinely detach, exactly as `AudioDataInit.transfer` promises.
        structuredClone(backing, { transfer: [backing] });
      }
      this.detached = backing !== undefined && backing.byteLength === 0;

      written.push(this);
      lastAudioData = this;
    }
    close(): void {
      this.closed = true;
    }
  }

  const writer = {
    write: vi.fn((_data: FakeAudioDataInstance) => {
      ackOrder.push('write');
      if (writeThrows) throw new Error('generator write failed (test fixture)');
      if (writeDefers) {
        return new Promise<void>((_resolve, reject) => {
          rejectPendingWrite = reject;
        });
      }
      return undefined;
    }),
    releaseLock: vi.fn(),
  };

  class FakeMediaStreamTrackGenerator {
    readonly kind = 'audio';
    readonly id = 'fake-screen-audio-track';
    readonly writable = { getWriter: () => writer };
    stop = vi.fn();
  }

  const globals = globalThis as unknown as Record<string, unknown>;
  const previousAudioData = globals.AudioData;
  const previousGenerator = globals.MediaStreamTrackGenerator;
  globals.AudioData = FakeAudioData;
  globals.MediaStreamTrackGenerator = FakeMediaStreamTrackGenerator;
  cleanups.push(() => {
    globals.AudioData = previousAudioData;
    globals.MediaStreamTrackGenerator = previousGenerator;
  });

  // `window.electron.audiocap` is the real preload surface (preload.ts §
  // audiocap): its PRESENCE is the C4 feature-detect, its VALUE is the tag
  // the bridge must key its `message` listener on.
  const windowElectron = window as unknown as {
    electron?: Record<string, unknown>;
  };
  const previousElectron = windowElectron.electron;
  const getPortMessageTag = vi.fn(() => AUDIOCAP_PORT_TAG);
  windowElectron.electron = { ...previousElectron, audiocap: { getPortMessageTag } };
  cleanups.push(() => {
    windowElectron.electron = previousElectron;
  });

  // jsdom gap 1 (reused from audiocapRelay.test.ts): rebuild a real
  // `MessageEvent` and attach `ports` by hand, since jsdom's own
  // `window.postMessage` silently drops the transfer list.
  const originalPostMessage = window.postMessage.bind(window);
  window.postMessage = ((data: unknown, _origin?: unknown, transfer?: unknown) => {
    const ports = Array.isArray(transfer) ? transfer : [];
    // `source: window` is REQUIRED, not decoration. A real same-window
    // `postMessage` sets `source` to the sending window, and the bridge refuses
    // any other sender (S2819). Building the event without it left `source`
    // null, so the guard was passing for the wrong reason and a foreign-sender
    // test could not be written at all.
    // `origin` alongside `source`: the bridge requires BOTH, and a
    // MessageEvent built without an origin carries '' -- which would reject
    // every handoff and make the whole suite fail for the wrong reason.
    const event = new MessageEvent('message', {
      data,
      source: window,
      origin: window.location.origin,
    });
    Object.defineProperty(event, 'ports', { value: ports, configurable: true });
    window.dispatchEvent(event);
  }) as typeof window.postMessage;
  cleanups.push(() => {
    window.postMessage = originalPostMessage;
  });

  // Precondition, asserted rather than optional-chained: a bridge that never
  // registers its `message` listener must fail HERE, not silently pass every
  // case below because `deliver()` never reaches a real handler.
  const addSpy = vi.spyOn(window, 'addEventListener');
  const bridge = createScreenAudioBridge(generation);
  expect(addSpy).toHaveBeenCalledWith('message', expect.any(Function));
  addSpy.mockRestore();
  cleanups.push(() => bridge.stop());

  const port = makeFakePort(ackOrder);
  // `window.location.origin`, not '*'. The production relay pins the exact origin
  // whenever the document has one (`handoverTargetOrigin`), so a '*' here would
  // simulate a handover the relay no longer performs on any origin jsdom can
  // produce -- and it trips the wildcard-postMessage scanner rule in a file whose
  // whole purpose is to model the real thing.
  window.postMessage({ [AUDIOCAP_PORT_TAG]: true, generation }, window.location.origin, [
    port,
  ] as unknown as Transferable[]);

  return {
    bridge,
    written,
    ackOrder,
    port,
    getPortMessageTag,
    /** Reject the write left pending by `writeDefers`. */
    rejectPendingWrite(): void {
      expect(typeof rejectPendingWrite).toBe('function');
      rejectPendingWrite!(new Error('generator write rejected after stop (test fixture)'));
    },
    get lastAudioDataClosed(): boolean {
      return lastAudioData?.closed ?? false;
    },
    stats: () => bridge.stats(),
    /** Deliver a quantum on the SAME synchronous run — see the file header. */
    deliver(buf: ArrayBuffer): void {
      // Precondition, asserted rather than optional-chained: if the bridge
      // never adopted the handed-off port, every case below would otherwise
      // pass vacuously by delivering to nothing.
      expect(typeof port.onmessage).toBe('function');
      port.onmessage!({ data: buf });
    },
  };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

describe('screenAudioBridge — port adoption', () => {
  it('reads the port-message tag from window.electron.audiocap, never a hardcoded string', async () => {
    const b = await bridgeHarness();
    // "Passed" (the bridge exists) paired with "obeyed" (it actually called the
    // feature-detected getter rather than assuming `'__concordAudiocap'` by
    // literal — the exact class of bug the plan's Task 6 correction exists to
    // prevent, since there is no `window.concord` for a stale bridge to fall
    // back to).
    expect(b.getPortMessageTag).toHaveBeenCalled();
  });
});

describe('screenAudioBridge — AudioData and the generator', () => {
  it('produces AudioData with the pinned format', async () => {
    const b = await bridgeHarness();
    b.deliver(quantumWithSeq(0));
    expect(b.written[0]).toMatchObject({
      format: 'f32',
      sampleRate: 48000,
      numberOfFrames: 480,
      numberOfChannels: 2,
    });
  });

  it('acks credit in the SAME synchronous run as the generator write', async () => {
    const b = await bridgeHarness();
    b.deliver(quantumWithSeq(0));
    // No await between deliver() and this assertion. Acking on RECEIPT rather
    // than on write makes the bound measure the transport instead of the
    // consumer -- S1's silent-loss mode wearing a counter (design §6d).
    expect(b.ackOrder).toEqual(['write', 'ack']);
  });

  it('performs no await between reading the header and writing the AudioData (§6d)', async () => {
    const b = await bridgeHarness();
    // Two quanta fired with NO await between them. If an await separated the
    // header read from the generator write, neither write could have
    // completed synchronously and `written` would still be empty here --
    // an await there reorders quanta relative to acks and silently converts
    // drop-newest into reorder-newest.
    b.deliver(quantumWithSeq(0));
    b.deliver(quantumWithSeq(1));
    expect(b.written).toHaveLength(2);
    expect(b.ackOrder).toEqual(['write', 'ack', 'write', 'ack']);
  });

  it('closes the AudioData if the generator write throws', async () => {
    const b = await bridgeHarness({ writeThrows: true });
    b.deliver(quantumWithSeq(0));
    expect(b.written).toHaveLength(1);
    expect(b.lastAudioDataClosed).toBe(true);
  });
});

describe('screenAudioBridge — drop witnesses (§4f, two independent observers)', () => {
  it('counts seq gaps independently of the reported overrunTotal', async () => {
    const b = await bridgeHarness();
    b.deliver(quantumWithSeq(0));
    // overrunTotal pinned at 0 on purpose: a gap witness derived from the
    // stream itself must still move even when the addon's own counter lies
    // or simply has not caught up yet (measured up to an 8-quantum lag).
    b.deliver(quantumWithSeq(3, 0));
    expect(b.stats().seqGaps).toBe(2);
  });

  it('handles seq wrap (0xFFFFFFFF -> 0) without reporting a gap', async () => {
    const b = await bridgeHarness();
    b.deliver(quantumWithSeq(0xffffffff));
    b.deliver(quantumWithSeq(0));
    expect(b.stats().seqGaps).toBe(0);
  });
});

describe('screenAudioBridge — generation fencing (§6b)', () => {
  it('discards quanta from a superseded generation', async () => {
    const b = await bridgeHarness({ generation: 1 });
    const superseding = createScreenAudioBridge(2);
    cleanups.push(() => superseding.stop());

    b.deliver(quantumWithSeq(0));

    expect(b.written).toHaveLength(0);
    expect(b.ackOrder).toEqual([]);
  });
});

// ─── PR #3245 red-team regressions ────────────────────────────────────────────
//
// Every case below was a SURVIVING MUTANT or a CONFIRMED bug at the time it was
// written. The first two were recorded in the plan as "needs a renderer, a
// display and a darwin/win32 runner"; they do not — they needed a stub that
// models the OUTER EFFECT instead of the argument list. See `FakeAudioData`.
//
// GENERATIONS ASCEND, AND MUST. The bridge's "current generation" is module
// scope that only ever moves FORWARD (see this file's header — a rewind would
// un-fence a stale bridge the instant its successor stopped). The fencing suite
// above leaves that mark at 2, so a case here built at generation 1 is stale on
// arrival: its port adopts, its quanta are silently discarded, and every
// assertion about `written` fails for a reason that has nothing to do with what
// the case is testing. Each case below therefore takes a fresh, higher number.
// Verified: these pass in isolation at generation 1 and fail in file order,
// which is exactly the shape that makes this worth writing down.

/** Sentinels written where the FIRST TWO PCM samples live, i.e. past the header. */
function quantumWithSamples(a: number, b: number): ArrayBuffer {
  const buf = quantumWithSeq(0);
  const samples = new Float32Array(buf, HEADER_BYTES, 2);
  samples[0] = a;
  samples[1] = b;
  return buf;
}

describe('screenAudioBridge — AudioData frame data and transfer (#3245)', () => {
  it('builds the sample view PAST the header, not at offset 0', async () => {
    const b = await bridgeHarness({ generation: 101 });
    b.deliver(quantumWithSamples(1.5, -2.25));

    expect(b.written).toHaveLength(1);
    // The kill: at offset 0 these read the magic/version/flags/seq bytes
    // reinterpreted as float32 (~1.6e-40), never the sentinels. No length check
    // can catch that mutant -- 0 + 3840 <= 3872, so nothing throws.
    expect(b.written[0].firstSamples).toEqual([1.5, -2.25]);
  });

  it('names the backing buffer in `transfer`, so the buffer actually detaches', async () => {
    const b = await bridgeHarness({ generation: 102 });
    const buf = quantumWithSamples(0.5, 0.25);
    b.deliver(buf);

    expect(b.written).toHaveLength(1);
    // The kill: `expect(init.transfer).toBe(buf)` would only prove a value was
    // passed. Detachment is the property that makes this a hand-over rather
    // than a 3840-byte copy, and it is observable -- `structuredClone` with a
    // transfer list genuinely detaches in Node.
    expect(b.written[0].detached).toBe(true);
    expect(buf.byteLength).toBe(0);
  });
});

describe('screenAudioBridge — handoff is a BROADCAST (#3245 VULN-2)', () => {
  it('does not close a handoff addressed to a DIFFERENT generation', async () => {
    const b = await bridgeHarness({ generation: 103 });

    // A handoff for another generation reaches generation 1's listener too: the
    // handoff is a `window` message broadcast, so every live bridge sees it.
    const foreign = makeFakePort([]);
    window.postMessage({ [AUDIOCAP_PORT_TAG]: true, generation: 104 }, window.location.origin, [
      foreign,
    ] as unknown as Transferable[]);

    // Before the fix this was 1: the older bridge destroyed the successor's
    // port before the successor's own listener ran, leaving no audio, no
    // `faulted`, and nothing logged on either side.
    expect(foreign.close).not.toHaveBeenCalled();
    // And it must not have adopted it either.
    expect(foreign.onmessage).toBeNull();
  });

  it('still closes a DUPLICATE handoff for its own generation rather than leaking it', async () => {
    const b = await bridgeHarness({ generation: 105 });

    const duplicate = makeFakePort([]);
    window.postMessage({ [AUDIOCAP_PORT_TAG]: true, generation: 105 }, window.location.origin, [
      duplicate,
    ] as unknown as Transferable[]);

    // This one IS ours and nobody else will adopt it, so refusing without
    // closing would leak a port that terminates at the native-code process.
    expect(duplicate.close).toHaveBeenCalled();
    // The original port keeps feeding the track.
    b.deliver(quantumWithSeq(0));
    expect(b.written).toHaveLength(1);
  });
});

describe('screenAudioBridge — sender verification (#3245, S2819/CWE-345)', () => {
  it('ignores a tagged handoff whose source is not this window', async () => {
    const b = await bridgeHarness({ generation: 106 });

    // A cross-context sender: an iframe, an opener, a popup. Dispatched by hand
    // because the harness shim always stamps `source: window`.
    const foreign = makeFakePort([]);
    const event = new MessageEvent('message', {
      data: { [AUDIOCAP_PORT_TAG]: true, generation: 106 },
      source: null,
      // Our own origin, deliberately: this case must fail on SOURCE alone, or it
      // would pass for the origin check's reason and pin nothing about identity.
      origin: window.location.origin,
    });
    Object.defineProperty(event, 'ports', { value: [foreign], configurable: true });
    window.dispatchEvent(event);

    // Neither adopted nor closed -- it was never addressed to this bridge.
    expect(foreign.onmessage).toBeNull();
    expect(foreign.close).not.toHaveBeenCalled();
    // And the real port is untouched: a forged handoff must not displace it.
    b.deliver(quantumWithSeq(0));
    expect(b.written).toHaveLength(1);
  });
});

describe('screenAudioBridge — origin verification (#3245, S2819)', () => {
  it('ignores a tagged handoff carrying a FOREIGN origin', async () => {
    const b = await bridgeHarness({ generation: 107 });

    const foreign = makeFakePort([]);
    const event = new MessageEvent('message', {
      data: { [AUDIOCAP_PORT_TAG]: true, generation: 107 },
      // `source: window` deliberately, so this case fails on ORIGIN alone --
      // the sibling above fails on SOURCE alone. Between them neither check can
      // be deleted without a red test.
      source: window,
      origin: 'https://evil.example',
    });
    Object.defineProperty(event, 'ports', { value: [foreign], configurable: true });
    window.dispatchEvent(event);

    expect(foreign.onmessage).toBeNull();
    expect(foreign.close).not.toHaveBeenCalled();
    b.deliver(quantumWithSeq(0));
    expect(b.written).toHaveLength(1);
  });
});

describe('screenAudioBridge — a stopped bridge cannot fault (#3245, Gitar)', () => {
  it('does not set faulted when an in-flight write rejects AFTER stop()', async () => {
    const b = await bridgeHarness({ generation: 108, writeDefers: true });

    // A quantum is written; its promise is still pending.
    b.deliver(quantumWithSeq(0));
    expect(b.written).toHaveLength(1);
    expect(b.stats().faulted).toBe(false);

    // The caller stops deliberately. `stop()` releases the writer lock while
    // that write is still in flight -- which is exactly the window.
    b.bridge.stop();

    // Now the pending write rejects. Without the `stopped` guard this lands in
    // `failClosed` and reports a fault for a teardown the caller asked for.
    b.rejectPendingWrite();
    await new Promise((r) => setTimeout(r, 0));

    expect(b.stats().faulted).toBe(false);
  });

  it('still sets faulted when a write rejects while the bridge is LIVE', async () => {
    // The control. The guard must not swallow a genuine fault.
    const b = await bridgeHarness({ generation: 109, writeDefers: true });

    b.deliver(quantumWithSeq(0));
    expect(b.stats().faulted).toBe(false);

    b.rejectPendingWrite();
    await new Promise((r) => setTimeout(r, 0));

    expect(b.stats().faulted).toBe(true);
  });
});
