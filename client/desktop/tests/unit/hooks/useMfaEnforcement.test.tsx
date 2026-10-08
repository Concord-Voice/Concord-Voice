import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, renderHook, waitFor } from '@testing-library/react';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';
import { jsonResponse, readOffers, STEP_UP_READ_PATH } from '../../helpers/stepUpApi';
import { createMockWsService, requireHandler } from '../../helpers/wsServiceMock';

// The model behind the "Require MFA for dangerous actions" switch (#3456 §3.5):
// what is known and when it is read. The component's own file proves the
// markup; this one proves the state machine under it, including the orderings
// a rendered switch cannot show. The service, the adapter and the request
// context are real; only `apiFetch` and the WebSocket service are replaced.
// "Mutant:" comments name the production change each case turns red.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

const wsHolder = vi.hoisted(() => ({ current: null as unknown }));
// `server_permissions_changed` re-reads through the shared jittered timer (§3.8),
// which draws its delay from `fullJitter`; 0 unless a case sets one.
const jitterDraw = vi.hoisted(() => vi.fn((_maxMs: number) => 0));
vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => wsHolder.current,
  fullJitter: (maxMs: number) => jitterDraw(maxMs),
}));

import { useMfaEnforcement } from '@/renderer/hooks/auth/useMfaEnforcement';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useClientConfigStore } from '@/renderer/stores/ui/clientConfigStore';

const SERVER = 'srv-1';
const PATH = `/api/v1/servers/${SERVER}/mfa-enforcement`;

type Reply = Response | Promise<Response>;
interface Script {
  get: () => Reply;
  put: () => Reply;
  read: () => Reply;
}
const settingResponse = (enforcing: boolean) =>
  jsonResponse(200, { enforce_mfa_dangerous_actions: enforcing });
const script: Script = {
  get: () => settingResponse(false),
  put: () => jsonResponse(200, {}),
  read: () => readOffers(['totp']),
};
const serve = (overrides: Partial<Script> = {}) =>
  Object.assign(script, {
    get: () => settingResponse(false),
    put: () => jsonResponse(200, {}),
    read: () => readOffers(['totp']),
    ...overrides,
  });

let ws: ReturnType<typeof createMockWsService>;
const putsSent = () =>
  mockApiFetch.mock.calls.filter((c) => (c[1] as RequestInit | undefined)?.method === 'PUT');
const getsSent = () =>
  mockApiFetch.mock.calls.filter(
    (c) => c[0] === PATH && ((c[1] as RequestInit | undefined)?.method ?? 'GET') === 'GET'
  );
const readsSent = () => mockApiFetch.mock.calls.filter((c) => c[0] === STEP_UP_READ_PATH);

async function mounted(serverId = SERVER) {
  const hook = renderHook(() => useMfaEnforcement(serverId));
  await waitFor(() => expect(hook.result.current.read.kind).not.toBe('pending'));
  return hook;
}

beforeEach(() => {
  resetAllStores();
  mockApiFetch.mockReset().mockImplementation(async (path: string, init?: RequestInit) => {
    if (path === STEP_UP_READ_PATH) return script.read();
    if (path === PATH) return (init?.method ?? 'GET') === 'PUT' ? script.put() : script.get();
    return jsonResponse(404);
  });
  serve();
  ws = createMockWsService();
  wsHolder.current = ws;
  jitterDraw.mockReset().mockImplementation(() => 0);
});

afterEach(() => {
  vi.useRealTimers();
});

describe('what is known at first', () => {
  // Mutant: the initial read is anything but pending, or announces/blocks before a read.
  it('starts pending, unknown, unblocked and silent', async () => {
    const reply = deferred<Response>();
    serve({ get: () => reply.promise });
    const { result } = renderHook(() => useMfaEnforcement(SERVER));

    expect(result.current).toMatchObject({
      read: { kind: 'pending' },
      enrolment: 'unknown',
      capability: 'loading',
      onBlock: null,
      turningOn: false,
      error: null,
      announcement: null,
    });
    await act(async () => reply.resolve(settingResponse(false)));
  });

  // Mutant: the requirements read runs before the switch is shown (spends the 20/min bucket for everyone).
  it('does not read the requirements until the switch is shown', async () => {
    const reply = deferred<Response>();
    serve({ get: () => reply.promise });
    renderHook(() => useMfaEnforcement(SERVER));
    await waitFor(() => expect(getsSent()).toHaveLength(1));

    expect(readsSent()).toHaveLength(0);
    await act(async () => reply.resolve(settingResponse(false)));
    await waitFor(() => expect(readsSent()).toHaveLength(1));
  });
});

describe('what blocks ON', () => {
  const capabilities = ['loading', 'supported', 'confirmed-unsupported', 'error'] as const;
  type Expect = 'unsupported' | 'unenrolled' | null;
  // enrolment x capability, for a setting that is OFF.
  const matrix: [string, string[] | null, (typeof capabilities)[number], Expect][] = [
    ['enrolled', ['totp'], 'supported', null],
    ['enrolled', ['totp'], 'confirmed-unsupported', 'unsupported'],
    ['enrolled', ['totp'], 'loading', null],
    ['enrolled', ['totp'], 'error', null],
    ['unenrolled', [], 'supported', 'unenrolled'],
    ['unenrolled', [], 'loading', 'unenrolled'],
    ['unenrolled', [], 'error', 'unenrolled'],
    // Mutant: the old-server check ranks below enrolment (an old server is told to enrol).
    ['unenrolled', [], 'confirmed-unsupported', 'unsupported'],
    ['unknown', null, 'supported', null],
    ['unknown', null, 'confirmed-unsupported', 'unsupported'],
  ];

  it.each(matrix)('%s on a %s server capability', async (_name, methods, capability, expected) => {
    serve({ read: () => (methods === null ? jsonResponse(500, {}) : readOffers(methods)) });
    useClientConfigStore.getState().setMfaEnforcementCapability({ status: capability });
    const { result } = await mounted();
    await waitFor(() => expect(readsSent()).toHaveLength(1));
    await act(async () => {});

    expect(result.current.onBlock).toBe(expected);
  });

  // Mutant: the `read.enforcing` clause dropped from `onBlockOf` (an enforcing server cannot be turned OFF by an unenrolled owner).
  it('blocks nothing once the setting is ON', async () => {
    serve({ get: () => settingResponse(true), read: () => readOffers([]) });
    useClientConfigStore
      .getState()
      .setMfaEnforcementCapability({ status: 'confirmed-unsupported' });
    const { result } = await mounted();
    await waitFor(() => expect(result.current.enrolment).toBe('unenrolled'));

    expect(result.current.onBlock).toBeNull();
  });

  // Mutant: an older requirements answer lands after a newer one (the effect returns no abort).
  it('a requirements read superseded by permissions_changed cannot overwrite the newer answer', async () => {
    const first = deferred<Response>();
    serve({ read: () => first.promise });
    const { result } = await mounted();
    await waitFor(() => expect(readsSent()).toHaveLength(1));

    serve({ read: () => readOffers(['totp']) });
    await act(async () => requireHandler(ws, 'permissions_changed')({ data: {} }));
    await waitFor(() => expect(result.current.enrolment).toBe('enrolled'));
    await act(async () => first.resolve(readOffers([])));

    expect(result.current.enrolment).toBe('enrolled');
  });
});

describe('the setting read', () => {
  // Mutant: retry is bound to nothing; only a never-succeeded read becomes failed.
  it('a failed first read can be retried into the value', async () => {
    serve({ get: () => jsonResponse(500, {}) });
    const { result } = await mounted();
    expect(result.current.read).toEqual({ kind: 'failed' });

    serve({ get: () => settingResponse(true) });
    act(() => result.current.retry());

    await waitFor(() => expect(result.current.read).toEqual({ kind: 'ready', enforcing: true }));
  });

  // Mutant: a second failure of the first read leaves `pending`, or a failed refresh turns a shown value into 'failed'.
  it('a refresh that fails keeps a shown value, and one that is refused as absent removes it', async () => {
    serve({ get: () => settingResponse(true) });
    const { result } = await mounted();

    serve({ get: () => jsonResponse(500, {}) });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );
    await waitFor(() => expect(getsSent()).toHaveLength(2));
    expect(result.current.read).toEqual({ kind: 'ready', enforcing: true });

    // The member lost the permission to see it: the switch goes with it.
    serve({ get: () => jsonResponse(403, {}) });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );
    await waitFor(() => expect(result.current.read).toEqual({ kind: 'absent' }));
  });

  // Mutant: the effect cleanup is dropped, so a read for a server we left still lands.
  it('a read that is still out when the server changes cannot land on the new server', async () => {
    const stale = deferred<Response>();
    serve({ get: () => stale.promise });
    const { result, rerender } = renderHook(({ id }) => useMfaEnforcement(id), {
      initialProps: { id: SERVER },
    });
    await waitFor(() => expect(getsSent()).toHaveLength(1));

    mockApiFetch.mockImplementation(async (path: string) =>
      path === '/api/v1/servers/srv-2/mfa-enforcement' ? jsonResponse(403, {}) : stale.promise
    );
    rerender({ id: 'srv-2' });
    await waitFor(() => expect(result.current.read).toEqual({ kind: 'absent' }));
    await act(async () => stale.resolve(settingResponse(true)));

    expect(result.current.read).toEqual({ kind: 'absent' });
  });
});

describe('turning ON', () => {
  // Mutant: the ref guard dropped (two synchronous activations send two PUTs).
  it('sends one PUT however many times it is called before the answer', async () => {
    const reply = deferred<Response>();
    serve({ put: () => reply.promise });
    const { result } = await mounted();

    act(() => {
      result.current.turnOn();
      result.current.turnOn();
    });

    expect(putsSent()).toHaveLength(1);
    expect(result.current.turningOn).toBe(true);
    await act(async () => reply.resolve(jsonResponse(200, {})));
    await waitFor(() => expect(result.current.turningOn).toBe(false));
  });

  // Mutant: `turningOnRef` is not released after an answer (ON can never be retried after a refusal).
  it('can be tried again after a refusal, and the retry clears the earlier error while it is out', async () => {
    serve({ put: () => jsonResponse(500, {}) });
    const { result } = await mounted();
    act(() => result.current.turnOn());
    await waitFor(() => expect(result.current.error).toEqual({ kind: 'refused', status: 500 }));

    const reply = deferred<Response>();
    serve({ put: () => reply.promise });
    act(() => result.current.turnOn());

    expect(putsSent()).toHaveLength(2);
    expect(result.current.error).toBeNull();
    await act(async () => reply.resolve(jsonResponse(200, {})));
    await waitFor(() => expect(result.current.read).toEqual({ kind: 'ready', enforcing: true }));
    expect(result.current.announcement).toBe('on');
  });

  // Mutant: any adapted refusal is taken for "enrol first" (an mfa_required answer to a code-less ON marks the account unenrolled).
  it('a refusal that is not about enrolment does not change what is known about enrolment', async () => {
    serve({
      put: () =>
        jsonResponse(403, {
          error: 'MFA verification required',
          mfa_required: true,
          methods: ['totp'],
        }),
    });
    const { result } = await mounted();
    await waitFor(() => expect(result.current.enrolment).toBe('enrolled'));

    act(() => result.current.turnOn());

    await waitFor(() => expect(result.current.error).toEqual({ kind: 'refused', status: 403 }));
    expect(result.current.enrolment).toBe('enrolled');
    expect(result.current.announcement).toBeNull();
  });

  // Mutant: the enrolment refusal is shown as a generic error, or leaves enrolment unknown.
  it('a refusal for enrolment records the account as unenrolled and announces it', async () => {
    serve({ put: () => jsonResponse(403, { error: 'x', mfa_enrollment_required: true }) });
    const { result } = await mounted();

    act(() => result.current.turnOn());

    await waitFor(() => expect(result.current.enrolment).toBe('unenrolled'));
    expect(result.current.announcement).toBe('enrolment');
    expect(result.current.error).toBeNull();
    expect(result.current.read).toEqual({ kind: 'ready', enforcing: false });
    expect(result.current.onBlock).toBe('unenrolled');
  });

  // Mutant: the context check dropped (an answer for the account that was signed in applies to the one that is).
  it('an answer that arrives after an account switch applies to nothing', async () => {
    const reply = deferred<Response>();
    serve({ put: () => reply.promise });
    const { result } = await mounted();
    act(() => result.current.turnOn());
    await waitFor(() => expect(putsSent()).toHaveLength(1));

    act(() =>
      useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
    );
    await act(async () => reply.resolve(jsonResponse(200, {})));

    await waitFor(() => expect(result.current.turningOn).toBe(false));
    expect(result.current.read).toEqual({ kind: 'ready', enforcing: false });
    expect(result.current.announcement).toBeNull();
    expect(result.current.error).toBeNull();
  });

  // Mutant: 'aborted' is treated as a transport failure (a network error and a re-read for a request that never left).
  it('a request that never left shows nothing and re-reads nothing', async () => {
    serve({ put: () => Promise.reject(new DOMException('fenced', 'AbortError')) });
    const { result } = await mounted();
    const getsBefore = getsSent().length;

    act(() => result.current.turnOn());

    await waitFor(() => expect(result.current.turningOn).toBe(false));
    expect(result.current.error).toBeNull();
    expect(result.current.announcement).toBeNull();
    expect(getsSent()).toHaveLength(getsBefore);
  });

  // Mutant: the transport arm does not re-read.
  it('a lost answer shows the network error and re-reads the setting', async () => {
    serve({ put: () => Promise.reject(new TypeError('network')) });
    const { result } = await mounted();

    act(() => result.current.turnOn());

    await waitFor(() => expect(result.current.error).toEqual({ kind: 'transport' }));
    await waitFor(() => expect(getsSent()).toHaveLength(2));
  });

  // Mutant: the answer after unmount still sets state (no crash, but the in-flight flag must clear for a remount's sake).
  it('an answer after unmount is dropped quietly', async () => {
    const reply = deferred<Response>();
    serve({ put: () => reply.promise });
    const { result, unmount } = await mounted();
    act(() => result.current.turnOn());
    await waitFor(() => expect(putsSent()).toHaveLength(1));
    const errors = vi.spyOn(console, 'error').mockImplementation(() => {});

    unmount();
    await act(async () => reply.resolve(jsonResponse(200, {})));

    expect(errors).not.toHaveBeenCalled();
  });
});

describe('settleOff', () => {
  // Mutant: settleOff leaves the value, the stale error or the announcement.
  it('sets the value OFF, clears an earlier error and announces it', async () => {
    serve({ get: () => settingResponse(true), put: () => jsonResponse(500, {}) });
    const { result } = await mounted();
    act(() => result.current.settleOff());

    expect(result.current.read).toEqual({ kind: 'ready', enforcing: false });
    expect(result.current.announcement).toBe('off');
    expect(result.current.error).toBeNull();
  });

  // Mutant: settleOff does not supersede a read that began before the confirmed write.
  it('a read that began before it cannot land on top of it', async () => {
    serve({ get: () => settingResponse(true) });
    const { result } = await mounted();
    const stale = deferred<Response>();
    serve({ get: () => stale.promise });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );
    await waitFor(() => expect(getsSent()).toHaveLength(2));

    act(() => result.current.settleOff());
    await act(async () => stale.resolve(settingResponse(true)));

    expect(result.current.read).toEqual({ kind: 'ready', enforcing: false });
  });
});

// The re-GET on `server_permissions_changed` goes through the §3.8 scheduler: a
// 0-5 s jitter, one pending timer (an event while it is pending is dropped), and
// a cancel on unmount or server change. Only `setTimeout`/`clearTimeout` are
// faked, and real timers are restored before any `waitFor`, which cannot run
// under a faked `setTimeout`.
describe('the re-read on server_permissions_changed (§3.8)', () => {
  const OTHER = 'srv-2';
  const fakeTimers = () => vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
  const event = (serverId = SERVER) =>
    act(() => requireHandler(ws, 'server_permissions_changed')({ data: { server_id: serverId } }));
  const advance = (ms: number) => act(() => vi.advanceTimersByTime(ms));

  // Mutant: the handler calls refresh() directly (the GET goes out at once, with no jitter drawn).
  it('draws a 0-5 s delay and re-reads only when it has passed', async () => {
    const { result } = await mounted();
    serve({ get: () => settingResponse(true) });
    fakeTimers();
    jitterDraw.mockReturnValue(3000);

    event();

    expect(jitterDraw).toHaveBeenCalledExactlyOnceWith(5000);
    expect(getsSent()).toHaveLength(1);
    advance(2999);
    expect(getsSent()).toHaveLength(1);
    advance(1);
    vi.useRealTimers();
    await waitFor(() => expect(result.current.read).toEqual({ kind: 'ready', enforcing: true }));
    expect(getsSent()).toHaveLength(2);
  });

  // Mutant: the timer is re-armed (or a second one set) instead of the event being dropped.
  it('drops an event that arrives while one is pending, so one delay governs one re-read', async () => {
    await mounted();
    fakeTimers();
    jitterDraw.mockReturnValue(1000);

    event();
    advance(500);
    event();

    expect(jitterDraw).toHaveBeenCalledTimes(1);
    expect(vi.getTimerCount()).toBe(1);
    advance(500);
    vi.useRealTimers();
    await waitFor(() => expect(getsSent()).toHaveLength(2));
    await act(async () => {});
    expect(getsSent()).toHaveLength(2);
  });

  // Mutant: the effect cleanup does not cancel the pending timer.
  it('cancels a pending re-read when the hook unmounts', async () => {
    const { unmount } = await mounted();
    fakeTimers();
    jitterDraw.mockReturnValue(1000);
    event();
    expect(vi.getTimerCount()).toBe(1);

    unmount();

    expect(vi.getTimerCount()).toBe(0);
    advance(5000);
    vi.useRealTimers();
    await act(async () => {});
    expect(getsSent()).toHaveLength(1);
  });

  // Mutant: the timer outlives a serverId change, so it re-reads the server the row has left.
  it('cancels a pending re-read when the server changes', async () => {
    const { result, rerender } = renderHook(({ id }) => useMfaEnforcement(id), {
      initialProps: { id: SERVER },
    });
    await waitFor(() => expect(result.current.read.kind).not.toBe('pending'));
    fakeTimers();
    jitterDraw.mockReturnValue(1000);
    event();

    rerender({ id: OTHER });
    expect(vi.getTimerCount()).toBe(0);
    advance(5000);
    vi.useRealTimers();
    await act(async () => {});

    expect(getsSent()).toHaveLength(1);
  });

  // Mutant: `authGeneration` dropped from the event effect, so a re-read the previous
  // account's event armed goes out as the next account.
  it('cancels a pending re-read when the account changes', async () => {
    await mounted();
    fakeTimers();
    jitterDraw.mockReturnValue(1000);
    event();
    expect(vi.getTimerCount()).toBe(1);

    act(() =>
      useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
    );

    expect(vi.getTimerCount()).toBe(0);
    advance(5000);
    vi.useRealTimers();
    await act(async () => {});
    expect(getsSent()).toHaveLength(1);
  });

  // Mutant: the server filter dropped (another server's event arms this row's timer).
  it("arms nothing for another server's event", async () => {
    await mounted();
    fakeTimers();

    event(OTHER);

    expect(jitterDraw).not.toHaveBeenCalled();
    expect(vi.getTimerCount()).toBe(0);
  });
});

describe('a read that lands for an account no longer signed in', () => {
  // Mutant: the context check dropped from the read's answer.
  it('is dropped, so the row keeps what it showed', async () => {
    const held = deferred<Response>();
    serve({ get: () => held.promise });
    const { result } = renderHook(() => useMfaEnforcement(SERVER));

    act(() =>
      useAuthStore.setState({ authGeneration: useAuthStore.getState().authGeneration + 1 })
    );
    await act(async () => {
      held.resolve(settingResponse(true));
      await held.promise;
    });

    expect(result.current.read).toEqual({ kind: 'pending' });
  });
});

describe('a refused ON restarts the read that turnOn aborted', () => {
  // Mutant: the refused arm does not call refresh() (the row keeps the value from before the event).
  it.each([
    ['for another reason', () => jsonResponse(500, {})],
    ['to enrol first', () => jsonResponse(403, { error: 'x', mfa_enrollment_required: true })],
  ])('after a refusal %s', async (_name, refusal) => {
    const { result } = await mounted();
    const inFlight = deferred<Response>();
    let reads = 0;
    // The first re-read is held, and is the one turnOn aborts; the next one sees ON.
    serve({ get: () => (++reads === 1 ? inFlight.promise : settingResponse(true)), put: refusal });
    await act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );
    await waitFor(() => expect(getsSent()).toHaveLength(2));

    act(() => result.current.turnOn());

    await waitFor(() => expect(result.current.read).toEqual({ kind: 'ready', enforcing: true }));
    expect(getsSent()).toHaveLength(3);
    await act(async () => inFlight.resolve(settingResponse(false)));
    expect(result.current.read).toEqual({ kind: 'ready', enforcing: true });
  });
});

describe('the ON error against a later refresh', () => {
  async function refusedOn() {
    serve({ put: () => jsonResponse(500, {}) });
    const hook = await mounted();
    act(() => hook.result.current.turnOn());
    await waitFor(() =>
      expect(hook.result.current.error).toEqual({ kind: 'refused', status: 500 })
    );
    // The refusal restarts the read; let it settle (the setting is still OFF).
    await waitFor(() => expect(getsSent()).toHaveLength(2));
    await act(async () => {});
    return hook;
  }
  const event = () =>
    act(async () =>
      requireHandler(ws, 'server_permissions_changed')({ data: { server_id: SERVER } })
    );

  // Mutant: the refresh never clears the error (a banner under a switch that now shows ON).
  it('is cleared by a refresh that finds the setting ON', async () => {
    const { result } = await refusedOn();
    serve({ get: () => settingResponse(true), put: () => jsonResponse(500, {}) });

    await event();

    await waitFor(() => expect(result.current.read).toEqual({ kind: 'ready', enforcing: true }));
    expect(result.current.error).toBeNull();
  });

  // Mutant: the read a failed ON starts clears the failure it was started to qualify (a lost
  // answer that turned out to have applied would be shown to nobody).
  it('is not cleared by the read that the failure itself starts', async () => {
    let reads = 0;
    serve({
      get: () => settingResponse(++reads > 1),
      put: () => Promise.reject(new TypeError('network')),
    });
    const { result } = await mounted();

    act(() => result.current.turnOn());

    await waitFor(() => expect(result.current.read).toEqual({ kind: 'ready', enforcing: true }));
    expect(result.current.error).toEqual({ kind: 'transport' });
  });

  // Mutant: any successful refresh clears it (the failure of a change that did not apply is hidden).
  it('is kept by a refresh that finds the setting still OFF', async () => {
    const { result } = await refusedOn();

    await event();
    await waitFor(() => expect(getsSent()).toHaveLength(3));
    await act(async () => {});

    expect(result.current.read).toEqual({ kind: 'ready', enforcing: false });
    expect(result.current.error).toEqual({ kind: 'refused', status: 500 });
  });
});
