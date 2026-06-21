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
    MockBroadcastChannel.instances.push(this);
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

  /** Clear all instances and reset */
  static reset(): void {
    MockBroadcastChannel.instances = [];
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
