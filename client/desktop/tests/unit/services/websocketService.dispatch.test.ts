import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import { WebSocketService } from '@/renderer/services/websocketService';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Valid RFC 4122 v4 UUIDs — the `4` at position 13 marks version=4, and the
// `8` at position 17 sets the variant bits per RFC 4122 §4.4. Required because
// zod 4.x's `z.string().uuid()` rejects strings that lack the proper version
// and variant bits (the all-1s form passes pattern-match but fails the
// version-bit check). See WebSocketEventSchema's `UUID = z.string().uuid()`.
const UUID_A = '11111111-1111-4111-8111-111111111111';
const UUID_B = '22222222-2222-4222-8222-222222222222';

describe('WebSocketService.handleMessage — dispatch validation', () => {
  let svc: WebSocketService;
  let consoleErrorSpy: ReturnType<typeof vi.spyOn>;
  let consoleDebugSpy: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    resetAllStores();
    svc = new WebSocketService();
    consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    consoleDebugSpy = vi.spyOn(console, 'debug').mockImplementation(() => {});
  });

  afterEach(() => {
    consoleErrorSpy.mockRestore();
    consoleDebugSpy.mockRestore();
  });

  /**
   * Helper: invoke the private handleMessage with a synthetic MessageEvent.
   * Using `svc as any` for the private-method access is the standard Vitest
   * pattern for testing internals; the alternative (making handleMessage
   * public) would expand the API surface for tests' sake.
   */
  function fire(payload: unknown): void {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (svc as any).handleMessage({ data: JSON.stringify(payload) } as MessageEvent);
  }

  function fireRaw(rawString: string): void {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (svc as any).handleMessage({ data: rawString } as MessageEvent);
  }

  it('valid payload → handler invoked with narrowed type', () => {
    const handler = vi.fn();
    svc.on('friend_removed', handler);

    fire({ type: 'friend_removed', data: { user_id: UUID_A } });

    expect(handler).toHaveBeenCalledTimes(1);
    expect(handler.mock.calls[0][0]).toEqual({
      type: 'friend_removed',
      data: { user_id: UUID_A },
    });
    expect(useConnectionStore.getState().wireViolationCount).toBe(0);
  });

  it('JSON.parse failure → log + drop, counter NOT incremented', () => {
    const handler = vi.fn();
    svc.on('message', handler);

    fireRaw('this is not valid JSON {{{');

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(0);
    const allLogged = consoleErrorSpy.mock.calls
      .flatMap((call) => call.map((arg) => (typeof arg === 'string' ? arg : '')))
      .join(' ');
    expect(allLogged).toContain('Failed to parse');
  });

  it('schema rejection (known type, bad shape) → counter += 1, payload values NOT leaked (PII sentinel)', () => {
    const SENTINEL = 'sensitive-dm-content-do-not-leak-via-logs';
    const handler = vi.fn();
    svc.on('dm_message', handler);

    fire({
      type: 'dm_message',
      data: { content: SENTINEL /* missing required conversation_id + user_id */ },
    });

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);

    // PII assertion — check ALL captured console.error calls. The scrubZodIssues
    // helper must strip the `received` field from each zod issue so the rejected
    // payload's content never reaches the log sink. See [internal]rules/observability.md.
    const allLogged = consoleErrorSpy.mock.calls
      .flatMap((call) => call.map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg))))
      .join(' ');
    expect(allLogged).not.toContain(SENTINEL);
    expect(allLogged).toContain('[WS] wire violation');
  });

  it('entitlements_changed schema rejection (missing required field) → counter += 1, handler not invoked', () => {
    const handler = vi.fn();
    svc.on('entitlements_changed', handler);

    // `tier` present but the schema-required capability fields are absent.
    // Verifies the generic dispatch boundary rejects the #1297 event like any
    // other discriminant and increments the wire-violation counter (spec §6).
    fire({ type: 'entitlements_changed', data: { tier: 'free' } });

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);
  });

  it('unknown event type → log + counter += 1 + distinct "unknown event type" message', () => {
    const handler = vi.fn();
    svc.on('dm_message', handler);

    fire({
      type: 'event_from_future_server_version',
      data: {},
    });

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);

    const allLogged = consoleErrorSpy.mock.calls
      .flatMap((call) => call.map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg))))
      .join(' ');
    expect(allLogged).toContain('unknown event type');
    expect(allLogged).toContain('event_from_future_server_version');
  });

  it('handler exception → sibling handlers still invoked, counter NOT incremented', () => {
    const throwingHandler = vi.fn(() => {
      throw new Error('boom');
    });
    const goodHandler = vi.fn();
    svc.on('typing', throwingHandler);
    svc.on('typing', goodHandler);

    fire({
      type: 'typing',
      data: {
        channel_id: UUID_A,
        user_id: UUID_B,
        is_typing: true,
      },
    });

    expect(throwingHandler).toHaveBeenCalledTimes(1);
    expect(goodHandler).toHaveBeenCalledTimes(1);
    expect(useConnectionStore.getState().wireViolationCount).toBe(0);
  });

  it('counter resets to 0 on connected envelope', () => {
    // Seed the counter with a non-zero value (as if prior wire violations occurred):
    useConnectionStore.getState().incrementWireViolation();
    useConnectionStore.getState().incrementWireViolation();
    useConnectionStore.getState().incrementWireViolation();
    expect(useConnectionStore.getState().wireViolationCount).toBe(3);

    fire({
      type: 'connected',
      data: { client_id: UUID_A, user_id: UUID_B },
    });

    expect(useConnectionStore.getState().wireViolationCount).toBe(0);
  });
});
