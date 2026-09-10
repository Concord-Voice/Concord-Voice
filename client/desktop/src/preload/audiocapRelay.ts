/**
 * Preload audiocap PCM relay (#3195, ADR-0043; design §4b, §5 Q6 "Option B′").
 *
 * WHAT MAIN HANDS US. Main forks the `utilityProcess` that loads
 * `concord-audiocap`, creates a `MessageChannelMain`, keeps `port1` on the
 * child and pushes `port2` to THIS context with `webContents.postMessage` on
 * `AUDIOCAP_PORT_CHANNEL`. The relay validates every inbound message against
 * the wire format in `../shared/audiocapProtocol` and forwards the payload to
 * the main world over a SECOND, preload-created `MessageChannel` whose
 * main-world end is handed over exactly once, at handover.
 *
 * WHY THE SECOND CHANNEL — the security property, stated precisely so it is not
 * oversold. Option B′ does NOT hide the samples from the main world; the main
 * world has to build the `AudioData` and the track. What it buys is that the
 * main world is STRUCTURALLY UNABLE to send anything to the process that loads
 * native code. The only value this relay ever puts on the child port is a
 * freshly minted `{ c: 1 }` of its own construction — NEVER the object the main
 * world sent — so a compromised main world (remote-SPA code included) cannot
 * reach the child with one arbitrary structured-cloneable value the way Option A
 * would have allowed. Do not "simplify" the credit hop by forwarding the
 * main-world message object: that single line is the whole delta.
 *
 * EXACTLY TWO MESSAGE KINDS, AND NO IGNORE-AND-CONTINUE BRANCH. Inbound: a bare
 * transferable `ArrayBuffer` whose header decodes. Outbound: `{ c: 1 }`.
 * Anything else closes BOTH ports. The closed set IS the enforcement of "this
 * must not become a general RPC channel"; a comment is not enforcement (§4a).
 *
 * IT BUFFERS NOTHING. It is a proxy, not a queue. A preload that drained the
 * child at full rate while the main world lagged would reintroduce S1's silent
 * loss mode with a credit counter attached to it.
 *
 * IT LOGS NOTHING. Every value crossing here is either user audio or a value an
 * untrusted child chose; there is no diagnostic worth the sink
 * (`observability.md` principles 1, 2 and 4).
 *
 * WHY ITS OWN MODULE rather than inline in `preload.ts`: `scripts/build-preload.mjs`
 * runs esbuild with `bundle: true, external: ['electron']`, so this import is
 * INLINED into `dist/preload/preload.js` — no new runtime `require`, and the
 * sandbox contract in `tests/integration/preload-sandbox-contract.test.ts` still
 * holds. Splitting it also makes the hot path unit-testable without executing
 * the whole `contextBridge` surface.
 *
 * C2. A `MessagePort` cannot cross `contextBridge`. That is precisely why the
 * port rides `window.postMessage` — the mechanism Electron documents for
 * isolated-world → main-world port transfer — while the CAPABILITY flag rides
 * the bridge (`preload.ts` § audiocap).
 */

import { decodeQuantumHeader } from '../shared/audiocapProtocol';

/**
 * The `ArrayBuffer.prototype.byteLength` getter, captured once at module load.
 * Calling it against a value is a BRAND check: it reads the internal
 * `[[ArrayBufferData]]` slot and throws a `TypeError` for anything that does not
 * have one.
 *
 * WHY NOT `instanceof ArrayBuffer`, which is what §4b's table writes. Two
 * reasons, and the first is the one that matters:
 *
 *  1. `instanceof` tests the PROTOTYPE CHAIN, so `Object.create(ArrayBuffer.prototype)`
 *     passes it and has no buffer behind it — `decodeQuantumHeader`'s `new DataView`
 *     would then THROW out of the message handler instead of closing the port. A
 *     structured clone cannot deliver a prototype-forged object today, which is
 *     what makes `instanceof` safe HERE, but a check that does not depend on that
 *     argument is simply better.
 *  2. `instanceof` is realm-scoped. That costs nothing in the preload, which is
 *     one realm — but it makes the positive-path assertion (C7: "passed" paired
 *     with "obeyed") structurally unwritable in the unit harness, where jsdom has
 *     no `MessageChannel` at all and vitest falls through to Node's
 *     `worker_threads` pair, which deserializes into a different realm.
 *
 * The predicate is otherwise identical, and it is strictly narrower — nothing
 * that fails `instanceof ArrayBuffer` in one realm passes this.
 */
const getArrayBufferByteLength = Object.getOwnPropertyDescriptor(
  ArrayBuffer.prototype,
  'byteLength'
)?.get;

/**
 * Main → preload channel carrying `port2` of the child's `MessageChannelMain`.
 *
 * Declared here because the relay is its only renderer-side consumer.
 * `tsconfig.main.json` does not compile `src/preload/**`, so the main-process
 * sender cannot import this constant; if a third consumer appears, re-home it
 * into `src/shared/audiocapProtocol.ts`, which both build legs see.
 */
export const AUDIOCAP_PORT_CHANNEL = 'audiocap:port';

/**
 * Discriminator on the one-shot `window` message that carries the main world's
 * end of the relay-owned channel. The HANDOVER rides a `window` message event
 * because that is the only transfer Electron documents across the isolated /
 * main world boundary; per-quantum traffic never does, which is the point of
 * §5 Q6.
 */
export const AUDIOCAP_PORT_TAG = '__concordAudiocap';

/** The slice of `IpcRendererEvent` this relay reads. */
interface AudiocapPortEvent {
  readonly ports: readonly MessagePort[];
}

/** The slice of `ipcRenderer` this relay uses. */
export interface AudiocapIpcRenderer {
  on(channel: string, listener: (event: AudiocapPortEvent, payload: unknown) => void): unknown;
}

/**
 * The slice of `window` this relay uses. Structural rather than `Window`
 * because `tsconfig.preload.json` pins `lib` to ES2022 with no DOM.
 */
export interface AudiocapRelayWindow {
  addEventListener(type: 'load', listener: () => void): void;
  postMessage(message: unknown, targetOrigin: string, transfer: MessagePort[]): void;
  readonly document?: { readonly readyState: string };
  readonly location?: { readonly origin?: string };
}

/**
 * The `targetOrigin` for the one-shot port handover.
 *
 * WHY NOT A BARE `'*'`, AND WHY NOT A BARE `'/'` EITHER. This posts to preload's
 * OWN window, so there is no cross-origin recipient to leak to and `'*'` is not
 * the information-disclosure the generic rule describes. What `'*'` genuinely
 * costs is a NAVIGATION race: `adoptPort` awaits `windowLoaded`, and a document
 * swap inside that await would hand the port -- and with it the only channel
 * into the process that loaded native code -- to a document that is not the one
 * this port was minted for. `spaLoader` performs exactly those swaps between
 * `https://spa.concordvoice.chat`, `app://concord` and `spa-cache://concord`.
 *
 * `'/'` (same-origin-only) is the textbook answer and is WRONG here: both
 * bundled origins are non-special schemes, whose `URL.origin` is the literal
 * string `"null"` (see `[internal]rules/electron.md` § WHATWG URL gotchas). An
 * opaque origin is never same-origin with anything, itself included, so `'/'`
 * would silently drop the handover on the bundled path -- a no-audio bug with
 * nothing in any log, which is the exact failure shape the `windowLoaded`
 * ordering note above exists to prevent.
 *
 * So: pin the exact origin whenever the document HAS one, which covers the
 * remote SPA -- the only origin an attacker-controlled document could plausibly
 * be swapped in from -- and fall back to `'*'` only for the opaque bundled
 * origins, where no other value can be expressed. This narrows the race rather
 * than closing it; the residual is a bundled-origin-to-bundled-origin swap
 * inside the await, which needs `app://concord` or `spa-cache://concord` to
 * already be serving hostile content.
 */
function handoverTargetOrigin(windowRef: AudiocapRelayWindow): string {
  const origin = windowRef.location?.origin;
  return typeof origin === 'string' && origin.length > 0 && origin !== 'null' ? origin : '*';
}

/** A real `ArrayBuffer`, by internal slot. Fails closed if the getter is gone. */
function isArrayBuffer(value: unknown): value is ArrayBuffer {
  if (getArrayBufferByteLength === undefined) return false;
  try {
    getArrayBufferByteLength.call(value);
    return true;
  } catch {
    return false;
  }
}

/**
 * `{ c: 1 }` and nothing else. `c === 1` already implies integrality, so no
 * separate `Number.isInteger` test is needed; a `'1'` or a `1.0000001` fails it.
 */
function isCreditMessage(value: unknown): boolean {
  return typeof value === 'object' && value !== null && (value as { c?: unknown }).c === 1;
}

/**
 * The host generation this port belongs to. Narrowed rather than trusted: it is
 * forwarded into the main world, where Task 6's bridge uses it as a fence, and a
 * non-integer would poison that fence rather than fail it. `null` means refuse.
 */
function readGeneration(payload: unknown): number | null {
  if (typeof payload !== 'object' || payload === null) return null;
  const generation = (payload as { generation?: unknown }).generation;
  return typeof generation === 'number' && Number.isInteger(generation) && generation >= 0
    ? generation
    : null;
}

/**
 * Install the relay. Call once, at preload module scope.
 *
 * The `windowLoaded` promise MUST be created HERE, at install time, so the
 * `load` listener is registered before the event fires. Deferring it until the
 * IPC message arrives races app startup: on a fast machine `load` has already
 * fired by then, the await never settles, the port is never handed over, and
 * the symptom is intermittent silent screen shares. The `readyState` test is the
 * other half of the same guarantee — it covers the case where this module is
 * evaluated after the document already finished loading, which no listener can
 * recover from.
 */
export function installAudiocapRelay(
  ipc: AudiocapIpcRenderer,
  windowRef: AudiocapRelayWindow
): void {
  const windowLoaded = new Promise<void>((resolve) => {
    windowRef.addEventListener('load', () => resolve());
    if (windowRef.document?.readyState === 'complete') resolve();
  });

  // One relay at a time. A second delivery is a new capture generation, so the
  // previous pair is closed rather than left forwarding a dead child's quanta
  // into a port the bridge has already discarded.
  let closeActive: (() => void) | null = null;

  ipc.on(AUDIOCAP_PORT_CHANNEL, (event, payload) => {
    void adoptPort(event, payload);
  });

  async function adoptPort(event: AudiocapPortEvent, payload: unknown): Promise<void> {
    const childPort: MessagePort | undefined = event?.ports?.[0];
    if (!childPort) return;

    const generation = readGeneration(payload);
    if (generation === null) {
      childPort.close();
      return;
    }

    await windowLoaded;

    closeActive?.();

    const channel = new MessageChannel();
    let closed = false;
    const closeBoth = (): void => {
      if (closed) return;
      closed = true;
      childPort.close();
      channel.port1.close();
    };
    closeActive = closeBoth;

    // Acks the main world is OWED: one per quantum this relay actually forwarded.
    //
    // WHY A COUNTER, WHEN `isCreditMessage` ALREADY CONSTRAINS THE VALUE. Because
    // it constrains the value and says nothing about the COUNT. Without this, a
    // compromised main world loops `postMessage({ c: 1 })` and the relay turns
    // every iteration into a structured clone plus an IPC hop into the process
    // that loaded `rt/` -- an unmetered rate channel into the one process Option
    // B′ exists to keep it away from. The child's own `outstanding > 0` floor
    // (audiocapChild.ts) keeps the PROTOCOL sound, so this is not a flow-control
    // break; it is work the main world should not be able to impose at all.
    //
    // The module header's "structurally unable to send anything" was precise
    // about the value and silent about the count. That gap is what a red-team
    // pass on PR #3245 found, with a proof-of-concept that drove 500 child-bound
    // messages having forwarded zero quanta.
    //
    // An unearned ack is a PROTOCOL VIOLATION, not a no-op: it closes both ports,
    // which is this file's "no ignore-and-continue branch" rule (§4a). Merely
    // dropping it would turn an unbounded flood into a bounded one and still let
    // it run forever.
    let owed = 0;

    childPort.onmessage = (message): void => {
      const data: unknown = message.data;
      // "Is it an ArrayBuffer" is the port boundary's own check (§4b); the
      // 3872-byte length is deliberately NOT re-tested here because
      // `decodeQuantumHeader` leads with it, and duplicating it would add a
      // branch nothing can reach.
      if (!isArrayBuffer(data) || decodeQuantumHeader(data) === null) {
        closeBoth();
        return;
      }
      // Transfer, never copy: T0a proved the cross-world hop detaches, which is
      // what makes the renderer's steady-state allocation zero.
      owed += 1;
      channel.port1.postMessage(data, [data]);
    };

    channel.port1.onmessage = (message): void => {
      if (!isCreditMessage(message.data) || owed === 0) {
        closeBoth();
        return;
      }
      owed -= 1;
      // Minted here, never forwarded. See the module header.
      childPort.postMessage({ c: 1 });
    };

    // Hand the main world its end exactly once, at handover. See
    // `handoverTargetOrigin` for why this is neither a bare '*' nor a bare '/'.
    windowRef.postMessage(
      { [AUDIOCAP_PORT_TAG]: true, generation },
      handoverTargetOrigin(windowRef),
      [channel.port2]
    );

    childPort.start();
    channel.port1.start();
  }
}
