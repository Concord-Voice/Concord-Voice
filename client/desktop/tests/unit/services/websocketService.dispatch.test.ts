import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import { WebSocketService } from '@/renderer/services/websocketService';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
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

  it('voice rich-presence payload passes the dispatch schema boundary', () => {
    const handler = vi.fn();
    svc.on('rich_presence_update', handler);

    fire({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'server_voice',
        minimized: true,
        payload: { channel_id: UUID_A, server_id: UUID_B },
        updated_at: 1,
      },
    });

    expect(handler).toHaveBeenCalledTimes(1);
    expect(useConnectionStore.getState().wireViolationCount).toBe(0);
  });

  it('rejects malformed rich-presence frames without invoking subscribers or logging payload data', () => {
    const SENTINEL = 'private-user-name-and-participant-id';
    const handler = vi.fn();
    svc.on('rich_presence_update', handler);
    fire({
      type: 'rich_presence_update',
      data: {
        user_id: 'not-a-uuid',
        category: 'private_call',
        minimized: true,
        payload: { call_type: 'group', participant_count: 99, participant: SENTINEL },
        updated_at: 0,
      },
    });
    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);
    const loggedArgs = consoleErrorSpy.mock.calls.flat();
    const logged = loggedArgs
      .map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg)))
      .join(' ');
    expect(logged).not.toContain(SENTINEL);
    expect(logged).not.toContain('not-a-uuid');
    expect(logged).not.toContain('participant_count');
    const containsError = (value: unknown): boolean => {
      if (value instanceof Error) return true;
      if (Array.isArray(value)) return value.some(containsError);
      if (value !== null && typeof value === 'object')
        return Object.values(value).some(containsError);
      return false;
    };
    expect(loggedArgs.some(containsError)).toBe(false);
  });

  it('dispatches a valid detailed private-call category frame', () => {
    const handler = vi.fn();
    svc.on('rich_presence_update', handler);
    fire({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'private_call',
        minimized: false,
        payload: { call_type: 'group', participant_count: 3 },
        updated_at: 2,
      },
    });
    expect(handler).toHaveBeenCalledTimes(1);
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

  it('JSON.parse failure never logs exception text derived from the malformed frame', () => {
    const SENTINEL = 'malformed-frame-content-must-not-reach-logs';
    const parseSpy = vi.spyOn(JSON, 'parse').mockImplementationOnce(() => {
      throw new SyntaxError(SENTINEL);
    });

    try {
      fireRaw('{');

      expect(useConnectionStore.getState().wireViolationCount).toBe(0);
      const allLogged = consoleErrorSpy.mock.calls
        .flatMap((call) => call.map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg))))
        .join(' ');
      expect(allLogged).toContain('Failed to parse');
      expect(allLogged).not.toContain(SENTINEL);
      expect(allLogged).toContain('SyntaxError');
    } finally {
      parseSpy.mockRestore();
    }
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

  it('schema rejection logs omit strict unknown property names', () => {
    const UNKNOWN_KEY = 'attacker-controlled-user-id';
    const handler = vi.fn();
    svc.on('rich_presence_update', handler);

    fire({
      type: 'rich_presence_update',
      data: {
        user_id: UUID_A,
        category: 'custom_text',
        payload: { text: 'working', [UNKNOWN_KEY]: 'sensitive-value' },
        updated_at: 1,
      },
    });

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);
    const allLogged = consoleErrorSpy.mock.calls
      .flatMap((call) => call.map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg))))
      .join(' ');
    expect(allLogged).not.toContain(UNKNOWN_KEY);
    expect(allLogged).toContain('unrecognized_keys');
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

  it('presence snapshots with unknown envelope or data keys are rejected before dispatch', () => {
    const handler = vi.fn();
    svc.on('presence_snapshot', handler);
    const validData = { users: [{ user_id: UUID_A, status: 'online' }] };

    fire({ type: 'presence_snapshot', data: validData, unexpected: true });
    fire({ type: 'presence_snapshot', data: { ...validData, unexpected: true } });

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(2);
  });

  it('unknown event type → log + counter += 1 + distinct "unknown event type" message', () => {
    const UNKNOWN_TYPE = 'attacker-controlled-event-id';
    const handler = vi.fn();
    svc.on('dm_message', handler);

    fire({
      type: UNKNOWN_TYPE,
      data: {},
    });

    expect(handler).not.toHaveBeenCalled();
    expect(useConnectionStore.getState().wireViolationCount).toBe(1);

    const allLogged = consoleErrorSpy.mock.calls
      .flatMap((call) => call.map((arg) => (typeof arg === 'string' ? arg : JSON.stringify(arg))))
      .join(' ');
    expect(allLogged).toContain('unknown event type');
    expect(allLogged).not.toContain(UNKNOWN_TYPE);
    expect(allLogged).toContain('invalid_union');
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
