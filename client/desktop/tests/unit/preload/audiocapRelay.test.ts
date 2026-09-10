// @vitest-environment jsdom
//
// Task 5 contract tests for the preload audiocap relay (#3195, design §5 Q6 "Option B′").
//
// WHY A SEPARATE MODULE RATHER THAN TESTING preload.ts WHOLESALE.
// `preload.ts` is bundled by `scripts/build-preload.mjs` with `bundle: true` and
// `external: ['electron']`, and every non-electron import it carries today is an
// `import type` — erased at compile time. The repo's only existing preload test
// (`tests/integration/preload-sandbox-contract.test.ts`) inspects the built bundle's
// text rather than importing the module, because importing it executes the whole
// bridge. Putting the relay in its own module keeps `preload.ts` thin, makes the
// hot path unit-testable, and still inlines into `preload.js` — so the sandbox
// contract ("only requires electron as a runtime external") is unaffected.
//
// NOTE ON COVERAGE. `src/preload/**` is in `sonar.coverage.exclusions`
// (`sonar-project.properties:47+`) and outside the Istanbul `include`
// (`vite.config.ts:67-76`). None of this file's coverage reaches the Quality Gate.
// It is here because the relay is a trust boundary, not because a number needs it.
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';

import {
  AUDIOCAP_PORT_CHANNEL,
  AUDIOCAP_PORT_TAG,
  installAudiocapRelay,
} from '../../../src/preload/audiocapRelay';
import { QUANTUM_BYTES, encodeQuantumHeader } from '../../../src/shared/audiocapProtocol';

/** A conforming quantum: a real 32-byte header plus the PCM tail. */
function validQuantum(seq = 0): ArrayBuffer {
  const buf = new ArrayBuffer(QUANTUM_BYTES);
  encodeQuantumHeader(buf, { seq, captureTimestampNs: 1n, overrunTotal: 0 });
  return buf;
}

/** A right-sized buffer whose header will not decode — magic zeroed. */
function badHeaderQuantum(): ArrayBuffer {
  const buf = validQuantum();
  new DataView(buf).setUint16(0, 0x0000, true);
  return buf;
}

type Handler = (event: unknown, payload: unknown) => void;

/** Teardown registered by `harness()`; drained in the global `afterEach` below. */
const cleanups: Array<() => void> = [];

afterEach(() => {
  while (cleanups.length > 0) cleanups.pop()!();
});

/**
 * TWO jsdom GAPS THIS HARNESS PAPERS OVER. Both were measured, not assumed, and
 * both make the naive version of this file fail against a CORRECT relay.
 *
 * 1. `window.postMessage` in jsdom is `function (message, targetOrigin)` —
 *    arity 2, and `Window.js` carries a literal `// TODO: event.ports`. The
 *    transfer list is silently dropped, so `event.ports[0]` is always empty and
 *    the main world never receives its port. Since a `MessagePort` cannot cross
 *    `contextBridge` (C2), `window.postMessage` is the ONLY mechanism Electron
 *    documents for isolated-world → main-world port transfer — there is no
 *    production-correct substitute to test instead. So the harness patches it.
 *
 * 2. jsdom implements no `MessageChannel` at all, so vitest leaves Node's
 *    `worker_threads` pair in place. That pair deserializes into the NODE realm,
 *    and a delivered buffer fails `instanceof ArrayBuffer` while being a perfectly
 *    good ArrayBuffer. The relay therefore brand-checks via the
 *    `ArrayBuffer.prototype.byteLength` getter rather than `instanceof` — which is
 *    also strictly stronger in production, because `instanceof` accepts
 *    `Object.create(ArrayBuffer.prototype)`, which has no buffer behind it and
 *    would make `new DataView` THROW OUT of the message handler instead of
 *    closing the port. A fail-open, reached by a hostile main world.
 */
function harness() {
  const ipcHandlers = new Map<string, Handler>();
  const ipcRenderer = {
    on: vi.fn((channel: string, handler: Handler) => {
      ipcHandlers.set(channel, handler);
    }),
  };

  // Gap 1: honour the transfer list jsdom drops.
  const realPostMessage = window.postMessage;
  window.postMessage = ((data: unknown, _origin?: unknown, transfer?: unknown) => {
    const ports = Array.isArray(transfer) ? (transfer as MessagePort[]) : [];
    setTimeout(() => {
      // `origin` and `source` are stamped because a real same-window
      // `postMessage` sets both, and the collector below verifies them. Built
      // without them the event carries origin '' and source null, the collector
      // drops everything, and every case fails for a reason unrelated to what it
      // tests.
      const event = new MessageEvent('message', {
        data,
        origin: window.location.origin,
        source: window,
      });
      Object.defineProperty(event, 'ports', { value: ports, configurable: true });
      window.dispatchEvent(event);
    }, 0);
  }) as typeof window.postMessage;
  cleanups.push(() => {
    window.postMessage = realPostMessage;
  });

  // The "child" end. The relay is handed `port2`; the test drives `port1`.
  const childPair = new MessageChannel();
  const childClosed = vi.fn();
  // jsdom does not report a close on the peer, so observe the relay's own close
  // by patching the port instance it receives.
  const relayFacingPort = childPair.port2 as MessagePort & { close: () => void };
  const realClose = relayFacingPort.close.bind(relayFacingPort);
  relayFacingPort.close = () => {
    childClosed();
    realClose();
  };

  const mainWorldMessages: Array<{ data: unknown; ports: readonly MessagePort[] }> = [];
  const onWindowMessage = (e: MessageEvent) => {
    // VERIFY THE SENDER, even though this is a test collector. Two reasons, and
    // the second is the one that matters. (1) CodeQL's `js/missing-origin-check`
    // flags any `message` listener without one, and it is merge-gating here --
    // dismissing it as "only a test" spends a false-positive on code that can
    // simply be correct instead. (2) This collector stands in for the MAIN WORLD,
    // which in production does check both (`screenAudioBridge.ts`). A harness
    // that accepts what production would refuse is modelling the wrong thing,
    // and would keep passing if the relay ever started posting from elsewhere.
    if (e.origin !== window.location.origin) return;
    if (e.source !== window) return;
    mainWorldMessages.push({ data: e.data, ports: e.ports });
  };
  window.addEventListener('message', onWindowMessage);
  // Without this removal a previous test's listener keeps collecting, and the
  // `load` dispatch in a later `beforeEach` resumes that test's still-pending
  // relay -- which can make a case pass by receiving the PREVIOUS test's port.
  // Observed while mutation-testing this suite.
  cleanups.push(() => window.removeEventListener('message', onWindowMessage));

  installAudiocapRelay(ipcRenderer, window);

  return {
    ipcRenderer,
    ipcHandlers,
    childPort: childPair.port1,
    relayFacingPort,
    childClosed,
    mainWorldMessages,
    /** Deliver the port to the relay exactly as main's `webContents.postMessage` would. */
    deliverPort(generation = 1) {
      const handler = ipcHandlers.get(AUDIOCAP_PORT_CHANNEL);
      // Precondition, asserted rather than optional-chained: a relay that never
      // registered its listener must fail here, not silently pass every case below.
      expect(typeof handler).toBe('function');
      handler!({ ports: [relayFacingPort] }, { generation });
    },
    /** Let the structured-clone/port message queue drain. */
    async flush() {
      await new Promise((r) => setTimeout(r, 0));
      await new Promise((r) => setTimeout(r, 0));
    },
  };
}

/** The port the relay handed the main world, if it handed one over. */
function mainWorldPort(h: ReturnType<typeof harness>): MessagePort | undefined {
  const tagged = h.mainWorldMessages.find(
    (m) => (m.data as { [k: string]: unknown } | null)?.[AUDIOCAP_PORT_TAG] === true
  );
  return tagged?.ports?.[0];
}

describe('preload audiocap relay — startup ordering', () => {
  let addSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    addSpy = vi.spyOn(window, 'addEventListener');
  });
  afterEach(() => {
    addSpy.mockRestore();
  });

  it('registers its load listener during install, before any load event fires', () => {
    installAudiocapRelay({ on: vi.fn() }, window);
    // The `windowLoaded` promise must be CREATED at install time. Deferring it until
    // the IPC message arrives races app startup: if `load` has already fired, the
    // await never settles and the port is never handed over — an intermittent
    // no-audio bug that reproduces only on fast machines.
    expect(addSpy).toHaveBeenCalledWith('load', expect.any(Function));
  });

  it('subscribes to the audiocap port channel and to nothing else', () => {
    const ipcRenderer = { on: vi.fn() };
    installAudiocapRelay(ipcRenderer, window);
    expect(ipcRenderer.on).toHaveBeenCalledTimes(1);
    expect(ipcRenderer.on).toHaveBeenCalledWith(AUDIOCAP_PORT_CHANNEL, expect.any(Function));
  });
});

describe('preload audiocap relay — forwarding', () => {
  beforeEach(() => {
    window.dispatchEvent(new Event('load'));
  });

  it('hands the main world exactly one port, once, at handover', async () => {
    const h = harness();
    h.deliverPort();
    await h.flush();

    const tagged = h.mainWorldMessages.filter(
      (m) => (m.data as { [k: string]: unknown } | null)?.[AUDIOCAP_PORT_TAG] === true
    );
    expect(tagged).toHaveLength(1);
    expect(tagged[0].ports).toHaveLength(1);
  });

  it('forwards a conforming 3872-byte quantum to the main world', async () => {
    const h = harness();
    h.deliverPort();
    await h.flush();

    const received: ArrayBuffer[] = [];
    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.onmessage = (m) => received.push(m.data as ArrayBuffer);
    port!.start();

    h.childPort.postMessage(validQuantum(0), [validQuantum(0)] as unknown as Transferable[]);
    h.childPort.postMessage(validQuantum(1));
    await h.flush();

    // "Passed" (a quantum crossed) paired with "obeyed" (it arrived intact and is
    // the size the wire format fixes it at, not a truncated or re-wrapped copy).
    expect(received.length).toBeGreaterThanOrEqual(1);
    expect(received[0].byteLength).toBe(QUANTUM_BYTES);
    expect(h.childClosed).not.toHaveBeenCalled();
  });
});

describe('preload audiocap relay — hostile input closes the port', () => {
  beforeEach(() => {
    window.dispatchEvent(new Event('load'));
  });

  // Every one of these is "close the port and forward NOTHING". There is no
  // ignore-and-continue branch: §4b's closed two-kind set IS the enforcement of
  // "must not become a general RPC channel", and a comment is not enforcement.
  it.each([
    ['a 3871-byte buffer', () => new ArrayBuffer(QUANTUM_BYTES - 1)],
    ['a 3873-byte buffer', () => new ArrayBuffer(QUANTUM_BYTES + 1)],
    ['a zero-length buffer', () => new ArrayBuffer(0)],
    ['a plain object', () => ({ kind: 'quantum' })],
    ['a string', () => 'quantum'],
    ['a number', () => 42],
    ['null', () => null],
    ['a right-sized buffer with a bad header', () => badHeaderQuantum()],
  ])('closes the port on %s and forwards nothing', async (_label, make) => {
    const h = harness();
    h.deliverPort();
    await h.flush();

    const received: unknown[] = [];
    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.onmessage = (m) => received.push(m.data);
    port!.start();

    h.childPort.postMessage(make() as unknown as never);
    await h.flush();

    expect(h.childClosed).toHaveBeenCalled();
    expect(received).toHaveLength(0);
  });

  it('closes the port when the main world returns a credit that is not exactly 1', async () => {
    const h = harness();
    h.deliverPort();
    await h.flush();

    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.start();
    port!.postMessage({ c: 2 });
    await h.flush();

    expect(h.childClosed).toHaveBeenCalled();
  });

  it('MINTS its own credit rather than forwarding the main world object', async () => {
    // THIS IS THE ENTIRE DELTA OVER OPTION A, and it survived mutation until this
    // test existed. `isCreditMessage` only checks that `c === 1`, so an object
    // carrying extra properties passes it. If the relay forwarded that object,
    // main-world JavaScript -- which on this app is code fetched from a REMOTE SPA --
    // could put an arbitrary structured-cloneable value onto a port held by the
    // process that loads native code. Option B′ exists to make that structurally
    // impossible, not merely validated.
    const h = harness();
    h.deliverPort();
    await h.flush();

    const toChild: unknown[] = [];
    h.childPort.onmessage = (m) => toChild.push(m.data);
    h.childPort.start();

    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.start();

    // EARN the ack first. The relay only mints against a quantum it actually
    // forwarded (the `owed` counter, #3245), so an unearned ack is now a
    // protocol violation that closes both ports -- and this test would then
    // observe zero child-bound messages and fail for a reason that has nothing
    // to do with the property it pins. Forwarding one quantum keeps the case
    // about mint-vs-forward, which is what it is for.
    h.childPort.postMessage(validQuantum(0));
    await h.flush();
    // The forwarded quantum is not an ack; nothing has reached the child yet.
    expect(toChild).toHaveLength(0);

    port!.postMessage({ c: 1, smuggled: 'reaches-the-native-process' });
    await h.flush();

    expect(toChild).toHaveLength(1);
    // Asserted on the KEY SET, not on `c`: a forwarded object also has `c === 1`,
    // so checking only that would pass against the very thing this test forbids.
    expect(Object.keys(toChild[0] as object).sort()).toEqual(['c']);
    expect(toChild[0]).toEqual({ c: 1 });
  });

  it('does not forward a quantum received BEFORE the main world attached', async () => {
    // Preload buffers nothing — it is a proxy, not a queue. A preload that drained
    // at full rate while the main world lagged would reintroduce S1's silent-loss
    // mode with a credit counter attached to it.
    const h = harness();
    h.deliverPort();
    await h.flush();

    h.childPort.postMessage(validQuantum(0));
    await h.flush();

    const received: unknown[] = [];
    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.onmessage = (m) => received.push(m.data);
    port!.start();
    await h.flush();

    // Whatever the relay did with that quantum, it must not have accumulated a
    // backlog to replay: at most the one message the port itself queued.
    expect(received.length).toBeLessThanOrEqual(1);
  });
});

// ── The handover targetOrigin (#3245 review, Semgrep wildcard-postMessage) ──
//
// These drive a STUB window rather than jsdom's real one, because the property
// under test is what the relay does for an origin jsdom cannot produce: the
// opaque `"null"` that `app://concord` and `spa-cache://concord` yield, per the
// WHATWG non-special-scheme rule. Overriding `window.location` in jsdom is
// fragile; passing a stub is what `installAudiocapRelay`'s second parameter is
// for.
describe('audiocap relay handover targetOrigin', () => {
  interface Sent {
    readonly origin: string;
  }

  function driveWith(origin: string | undefined): Sent[] {
    const sent: Sent[] = [];
    const handlers = new Map<string, (e: { ports: readonly MessagePort[] }, p: unknown) => void>();
    const stub = {
      addEventListener: (_type: 'load', _listener: () => void) => {
        /* readyState below settles `windowLoaded`; no load event needed. */
      },
      postMessage: (_message: unknown, targetOrigin: string, _transfer: MessagePort[]) => {
        sent.push({ origin: targetOrigin });
      },
      document: { readyState: 'complete' },
      ...(origin === undefined ? {} : { location: { origin } }),
    };
    installAudiocapRelay(
      {
        on: (
          channel: string,
          listener: (e: { ports: readonly MessagePort[] }, p: unknown) => void
        ) => {
          handlers.set(channel, listener);
        },
      },
      stub
    );
    const handler = handlers.get(AUDIOCAP_PORT_CHANNEL);
    expect(typeof handler).toBe('function');
    handler!({ ports: [new MessageChannel().port2] }, { generation: 1 });
    return sent;
  }

  const flush = () => new Promise((r) => setTimeout(r, 0));

  it('pins the exact origin when the document has a real one', async () => {
    const sent = driveWith('https://spa.concordvoice.chat');
    await flush();
    expect(sent).toHaveLength(1);
    expect(sent[0].origin).toBe('https://spa.concordvoice.chat');
    // The whole point: never the wildcard when a real origin was available.
    expect(sent[0].origin).not.toBe('*');
  });

  // `app://concord` / `spa-cache://concord` are non-special schemes, so
  // `URL.origin` is the literal string "null". `'/'` would be the textbook
  // answer and is wrong -- an opaque origin is never same-origin with anything,
  // itself included -- so the handover would be silently dropped. '*' is the
  // only value that can be expressed here, and this test pins that as a
  // deliberate fallback rather than an oversight.
  it('falls back to the wildcard for an opaque origin, which cannot be named', async () => {
    const sent = driveWith('null');
    await flush();
    expect(sent).toHaveLength(1);
    expect(sent[0].origin).toBe('*');
  });

  it('falls back to the wildcard when the window exposes no location at all', async () => {
    const sent = driveWith(undefined);
    await flush();
    expect(sent).toHaveLength(1);
    expect(sent[0].origin).toBe('*');
  });

  it('falls back to the wildcard for an empty-string origin', async () => {
    const sent = driveWith('');
    await flush();
    expect(sent).toHaveLength(1);
    expect(sent[0].origin).toBe('*');
  });
});

// ─── #3245 red-team: the ack is METERED, not merely shape-checked ─────────────
describe('audiocap relay credit metering', () => {
  it('closes both ports on an UNEARNED ack instead of minting one', async () => {
    const h = harness();
    h.deliverPort();
    await h.flush();

    const toChild: unknown[] = [];
    h.childPort.onmessage = (m) => toChild.push(m.data);
    h.childPort.start();

    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.start();

    // 500 acks, zero quanta forwarded. Before the `owed` counter every one of
    // these became a structured clone plus an IPC hop into the process that
    // loaded native code -- an unmetered rate channel, which is the thing
    // Option B′ exists to deny. `isCreditMessage` cannot see it: the VALUE is
    // legal every time; only the COUNT is wrong.
    for (let i = 0; i < 500; i += 1) port!.postMessage({ c: 1 });
    await h.flush();

    expect(toChild).toHaveLength(0);
    expect(h.childClosed).toHaveBeenCalled();
  });

  it('mints exactly one ack per forwarded quantum, and no more', async () => {
    const h = harness();
    h.deliverPort();
    await h.flush();

    const toChild: unknown[] = [];
    h.childPort.onmessage = (m) => toChild.push(m.data);
    h.childPort.start();

    const port = mainWorldPort(h);
    expect(port).toBeDefined();
    port!.start();

    // Two quanta forwarded earns exactly two acks. A third ack is unearned and
    // closes the pair, so the total never exceeds what was forwarded.
    h.childPort.postMessage(validQuantum(0));
    h.childPort.postMessage(validQuantum(1));
    await h.flush();
    port!.postMessage({ c: 1 });
    port!.postMessage({ c: 1 });
    await h.flush();

    expect(toChild).toHaveLength(2);
    expect(toChild.every((m) => JSON.stringify(m) === JSON.stringify({ c: 1 }))).toBe(true);
    // The pair is still alive: earned acks are ordinary traffic, not faults.
    expect(h.childClosed).not.toHaveBeenCalled();
  });
});
