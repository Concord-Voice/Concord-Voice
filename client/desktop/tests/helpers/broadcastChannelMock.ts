/**
 * Mock BroadcastChannel for PiP signaling tests.
 *
 * Captures posted messages and allows simulating incoming messages.
 * Optionally auto-responds to RPC requests via a configurable responder.
 */

export type AutoResponder = (data: unknown) => unknown | undefined;

export class MockBroadcastChannel {
  readonly name: string;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onmessageerror: ((event: MessageEvent) => void) | null = null;

  /** All messages posted via postMessage() */
  readonly posted: unknown[] = [];

  /** Optional auto-responder: return a value to auto-send an rpc-response */
  autoResponder: AutoResponder | null = null;

  private _closed = false;

  constructor(name: string) {
    this.name = name;
    // Seeded from the class-level default so a test can arm a responder BEFORE
    // the code under test opens its channel. PiP channels are now created inside
    // `PipVoiceClient.init()` (the name derives from a capability fetched over
    // IPC, #3104 D6), so an instance-only responder would arrive too late.
    this.autoResponder = MockBroadcastChannel.defaultAutoResponder;
    MockBroadcastChannel.instances.push(this);
    MockBroadcastChannel.all.push(this);
  }

  postMessage(data: unknown): void {
    if (this._closed) throw new DOMException('BroadcastChannel is closed');
    this.posted.push(data);

    // Auto-respond to RPC requests if a responder is configured
    if (this.autoResponder) {
      const response = this.autoResponder(data);
      if (response !== undefined) {
        // Deliver synchronously to the same instance (PipVoiceClient
        // posts and listens on the same BroadcastChannel instance)
        this.simulateMessage(response);
      }
    }
  }

  close(): void {
    this._closed = true;
    const idx = MockBroadcastChannel.instances.indexOf(this);
    if (idx >= 0) MockBroadcastChannel.instances.splice(idx, 1);
  }

  /** Simulate receiving a message (as if posted by another context) */
  simulateMessage(data: unknown): void {
    this.onmessage?.({ data } as MessageEvent);
  }

  // ── Static helpers ─────────────────────────────────────────────

  /** All live MockBroadcastChannel instances (for cross-instance delivery) */
  static instances: MockBroadcastChannel[] = [];

  /**
   * Applied to every instance constructed after it is set. Use this when the
   * channel does not exist yet at arm time; `install()`/`reset()` clear it.
   */
  static defaultAutoResponder: AutoResponder | null = null;

  /**
   * Every instance constructed since the last reset, INCLUDING closed ones.
   * `instances` models live delivery and therefore shrinks on close; assertions
   * that inspect what a channel posted usually run after teardown closed it.
   */
  static all: MockBroadcastChannel[] = [];

  /** Clear all instances and reset */
  static reset(): void {
    MockBroadcastChannel.instances = [];
    MockBroadcastChannel.all = [];
    MockBroadcastChannel.defaultAutoResponder = null;
  }

  /** Original BroadcastChannel saved during install() */
  private static _originalBC: unknown = undefined;

  /** Install as global BroadcastChannel (scoped — only replaces BroadcastChannel) */
  static install(): void {
    MockBroadcastChannel.reset();
    MockBroadcastChannel._originalBC = globalThis.BroadcastChannel;
    (globalThis as any).BroadcastChannel = MockBroadcastChannel;
  }

  /** Restore original BroadcastChannel (scoped — does not affect other globals) */
  static uninstall(): void {
    MockBroadcastChannel.reset();
    (globalThis as any).BroadcastChannel = MockBroadcastChannel._originalBC;
  }

  /** Get the most recently created instance */
  static get latest(): MockBroadcastChannel | undefined {
    return MockBroadcastChannel.instances[MockBroadcastChannel.instances.length - 1];
  }
}

/**
 * Creates an auto-responder for PiP RPC requests that returns
 * configurable results per method.
 */
export function createRpcResponder(responses: Record<string, unknown>): AutoResponder {
  return (data: unknown) => {
    const msg = data as { kind?: string; id?: string; method?: string };
    if (msg.kind !== 'rpc-request' || !msg.id) return undefined;

    const result = responses[msg.method ?? ''];
    if (result === undefined) {
      return { kind: 'rpc-response', id: msg.id, error: `No mock for ${msg.method}` };
    }
    return { kind: 'rpc-response', id: msg.id, result };
  };
}
