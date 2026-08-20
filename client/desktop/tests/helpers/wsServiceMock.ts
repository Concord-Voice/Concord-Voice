import { vi } from 'vitest';

/**
 * Signature for a handler captured by {@link createMockWsService}.
 *
 * Deliberately `unknown[]` rather than narrowed against `WebSocketEvent` from
 * `src/renderer/types/ws-events.ts`. The real `wsService.on` is generic
 * (`on<T extends WSEventType>(type: T, handler: (event: Extract<WebSocketEvent,
 * { type: T }>) => void)`), so mirroring it here is possible — but the suites
 * deliberately dispatch malformed payloads such as
 * `handler({ type: 'message_reaction_added', data: {} })` to exercise the
 * hook's defensive paths, and a narrowed signature rejects those at compile
 * time. Narrowing would therefore force casts back onto the very call sites it
 * was meant to type.
 *
 * `unknown` accepts every existing call site while keeping this file free of
 * `any` — and so free of the `@typescript-eslint/no-explicit-any` disable that
 * the twelve local copies each carried.
 */
export type CapturedHandler = (...args: unknown[]) => void;

export type MockWsService = ReturnType<typeof createMockWsService>;

/**
 * A mock `wsService` whose `on()` captures handlers into a Map keyed by event
 * type, so tests can drive a handler directly.
 *
 * This is the superset of the fourteen local definitions it replaces: most
 * needed only `handlers` / `on` / `onConnectionChange`, three also stubbed
 * `disconnect`, and one captured the connection listener so a test could drive
 * it via `emitConnectionChange`. The extra members are inert for suites that
 * do not touch them — the connection listener is only ever invoked by an
 * explicit `emitConnectionChange` call.
 *
 * Pass to the hook as `useWebSocketMessages(ws as never)`: the mock implements
 * the handful of methods under test, not the full service class.
 */
export function createMockWsService() {
  const handlers = new Map<string, CapturedHandler>();
  let connectionListener: ((state: unknown) => void) | undefined;

  return {
    handlers,
    on: vi.fn((type: string, handler: CapturedHandler) => {
      handlers.set(type, handler);
      return () => handlers.delete(type);
    }),
    onConnectionChange: vi.fn((cb: (state: unknown) => void) => {
      connectionListener = cb;
      return () => {
        connectionListener = undefined;
      };
    }),
    /** Test helper: drive the captured connection listener. */
    emitConnectionChange: (state: unknown) => connectionListener?.(state),
    disconnect: vi.fn(),
  };
}

/**
 * Fetch a captured handler, throwing if the hook never registered one for
 * `eventName` — a missing handler otherwise surfaces as an opaque
 * "handler is not a function" at the call site.
 */
export function requireHandler(ws: MockWsService, eventName: string): CapturedHandler {
  const handler = ws.handlers.get(eventName);
  if (!handler) throw new Error(`missing ${eventName} handler`);
  return handler;
}
