/**
 * A `BroadcastChannel` double with REAL fan-out semantics.
 *
 * `broadcastChannelMock.ts` is a single-instance capture harness: `postMessage`
 * records into `posted`, and only an explicit `simulateMessage` delivers
 * anything, to the instance it is called on. That models "the proxy and the one
 * PiP under test" — which is precisely the shape that HID the #3104 D6 finding,
 * because it can never show a message reaching a third document.
 *
 * The WHATWG contract is: a message posted on a channel is delivered to every
 * OTHER `BroadcastChannel` with the same name in the same origin and storage
 * partition, and never back to the poster. This implements that, so a document
 * that merely constructs `new BroadcastChannel(name)` sees exactly what a real
 * same-origin eavesdropper would.
 *
 * Adapted from the #3104 red-team harness that demonstrated the three exploits
 * this suite now asserts are repelled.
 */

type Listener = (event: MessageEvent) => void;

export class FanoutBroadcastChannel {
  readonly name: string;
  onmessage: Listener | null = null;
  onmessageerror: Listener | null = null;

  /** Everything THIS instance posted. */
  readonly posted: unknown[] = [];
  /** Everything THIS instance received from other instances. */
  readonly received: unknown[] = [];

  private closed = false;
  private readonly extraListeners: Listener[] = [];

  static instances: FanoutBroadcastChannel[] = [];
  private static original: unknown = undefined;

  constructor(name: string) {
    this.name = name;
    FanoutBroadcastChannel.instances.push(this);
  }

  addEventListener(type: string, fn: Listener): void {
    if (type === 'message') this.extraListeners.push(fn);
  }

  removeEventListener(type: string, fn: Listener): void {
    if (type !== 'message') return;
    const i = this.extraListeners.indexOf(fn);
    if (i >= 0) this.extraListeners.splice(i, 1);
  }

  postMessage(data: unknown): void {
    if (this.closed) throw new DOMException('BroadcastChannel is closed');
    this.posted.push(data);
    for (const other of [...FanoutBroadcastChannel.instances]) {
      if (other === this || other.closed || other.name !== this.name) continue;
      const cloned = jsonClone(data);
      other.received.push(cloned);
      const evt = { data: cloned } as MessageEvent;
      other.onmessage?.(evt);
      for (const fn of [...other.extraListeners]) fn(evt);
    }
  }

  close(): void {
    this.closed = true;
    const i = FanoutBroadcastChannel.instances.indexOf(this);
    if (i >= 0) FanoutBroadcastChannel.instances.splice(i, 1);
  }

  static install(): void {
    FanoutBroadcastChannel.instances = [];
    FanoutBroadcastChannel.original = (globalThis as Record<string, unknown>).BroadcastChannel;
    (globalThis as Record<string, unknown>).BroadcastChannel = FanoutBroadcastChannel;
  }

  static uninstall(): void {
    for (const ch of [...FanoutBroadcastChannel.instances]) ch.close();
    FanoutBroadcastChannel.instances = [];
    (globalThis as Record<string, unknown>).BroadcastChannel = FanoutBroadcastChannel.original;
  }

  /** Live channels carrying an exact name. */
  static named(name: string): FanoutBroadcastChannel[] {
    return FanoutBroadcastChannel.instances.filter((c) => c.name === name);
  }
}

/**
 * Structured clone stand-in. Every payload asserted on in the PiP-auth suite is
 * plain JSON data, and a JSON round-trip additionally proves the value survives
 * serialization — i.e. that it is exfiltratable, which a `CryptoKey` would not be.
 */
function jsonClone<T>(value: T): T {
  try {
    return JSON.parse(JSON.stringify(value)) as T;
  } catch {
    return value;
  }
}
