import {
  fetchEligibility,
  peekEligibility,
  prefetchEligibility,
  clearFriendEligibilityCache,
} from '@/renderer/services/friendEligibility';

vi.mock('@/renderer/services/apiClient', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '@/renderer/services/apiClient';
const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;

const ok = (eligible: boolean) =>
  ({ ok: true, status: 200, json: async () => ({ eligible }) }) as unknown as Response;
const status = (code: number) =>
  ({ ok: false, status: code, json: async () => ({}) }) as unknown as Response;

describe('friendEligibility', () => {
  beforeEach(() => {
    clearFriendEligibilityCache();
    mockApiFetch.mockReset();
  });

  it('maps 200 true to eligible and 200 false to ineligible', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(true));
    expect(await fetchEligibility('u1')).toBe('eligible');
    mockApiFetch.mockResolvedValueOnce(ok(false));
    expect(await fetchEligibility('u2')).toBe('ineligible');
  });

  it('calls the :user_id route via apiFetch', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(true));
    await fetchEligibility('u1');
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/users/u1/friend-request-eligibility',
      // A timeout signal is mandatory: an unsettled probe would otherwise pin
      // the promise in the cache and hide the affordance for the whole session.
      expect.objectContaining({ signal: expect.anything() })
    );
  });

  // Load-bearing: MemberProfileCard renders SendFriendRequestButton, so the hook
  // runs twice CONCURRENTLY for one userId. A boolean cache cannot dedupe that
  // first pair — only a promise inserted before the await can.
  it('dedupes concurrent calls for the same user to ONE request', async () => {
    let resolveFn!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        resolveFn = r;
      })
    );
    const a = fetchEligibility('u1');
    const b = fetchEligibility('u1');
    resolveFn(ok(true));
    expect(await a).toBe('eligible');
    expect(await b).toBe('eligible');
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it('caches a resolved verdict for the session', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(false));
    await fetchEligibility('u1');
    await fetchEligibility('u1');
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it('404 yields unknown and latches process-wide so no further user is probed', async () => {
    mockApiFetch.mockResolvedValueOnce(status(404));
    expect(await fetchEligibility('u1')).toBe('unknown');
    expect(await fetchEligibility('u2')).toBe('unknown');
    expect(await fetchEligibility('u3')).toBe('unknown');
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it.each([429, 500, 502, 401, 400])('status %i yields unknown and is NOT cached', async (code) => {
    mockApiFetch.mockResolvedValue(status(code));
    expect(await fetchEligibility('u1')).toBe('unknown');
    expect(await fetchEligibility('u1')).toBe('unknown');
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('a rejected transport yields unknown and is not cached', async () => {
    mockApiFetch.mockRejectedValueOnce(new Error('offline'));
    expect(await fetchEligibility('u1')).toBe('unknown');
    mockApiFetch.mockResolvedValueOnce(ok(true));
    expect(await fetchEligibility('u1')).toBe('eligible');
  });

  it('peek returns pending while in flight and the verdict once resolved', async () => {
    let resolveFn!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        resolveFn = r;
      })
    );
    const p = fetchEligibility('u1');
    expect(peekEligibility('u1')).toBe('pending');
    resolveFn(ok(true));
    await p;
    expect(peekEligibility('u1')).toBe('eligible');
  });

  it('peek returns pending for an unfetched user and never issues a request', () => {
    expect(peekEligibility('never-seen')).toBe('pending');
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('prefetch warms the cache without the caller awaiting', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(true));
    prefetchEligibility('u1');
    await vi.waitFor(() => expect(peekEligibility('u1')).toBe('eligible'));
  });

  // Cross-account leak: a module-scope cache that survives logout would serve
  // the previous account's verdicts to the next user on a shared device.
  it('clear empties the cache AND releases the unsupported latch', async () => {
    mockApiFetch.mockResolvedValueOnce(status(404));
    await fetchEligibility('u1');
    clearFriendEligibilityCache();
    mockApiFetch.mockResolvedValueOnce(ok(true));
    expect(await fetchEligibility('u1')).toBe('eligible');
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});

// ── Cross-account leak through a logout that races an in-flight probe ────────
//
// Found by an adversarial review pass. Clearing is synchronous; a probe already
// in flight is not. Its `.then` continuation runs AFTER the clear and, before
// the generation fence, wrote straight back into the structures the clear had
// just emptied — carrying account A's verdict, and account A's server's 404
// latch, into account B's session on the same process.
//
// Registering the clear in resetService is necessary but NOT sufficient, which
// is why these two live here and not only there.
describe('friendEligibility — identity fence on reset', () => {
  beforeEach(() => {
    clearFriendEligibilityCache();
    mockApiFetch.mockReset();
  });

  it('discards a verdict that settles after logout instead of serving it to the next account', async () => {
    let respond!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        respond = r;
      })
    );
    // Account A opens victim X's profile card; the probe is still in flight.
    const inFlight = fetchEligibility('victim-x');

    // A logs out. resetService clears the cache.
    clearFriendEligibilityCache();
    expect(peekEligibility('victim-x')).toBe('pending');

    // A's probe lands now. X does not accept requests from A.
    respond(ok(false));
    await inFlight;

    // Account B signs in on the same process. It must learn nothing from A.
    expect(peekEligibility('victim-x')).toBe('pending');
  });

  it('does not let a pre-reset 404 latch disable the gate on the next server', async () => {
    let respond!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        respond = r;
      })
    );
    // Account A is on an old self-hosted control-plane with no such route.
    const inFlight = fetchEligibility('someone');

    clearFriendEligibilityCache();

    // The old server's 404 arrives after the reset.
    respond(status(404));
    await inFlight;

    // B connects to a control-plane that DOES implement the route. The gate
    // must be live again, not latched off for the whole session.
    mockApiFetch.mockResolvedValueOnce(ok(false));
    expect(await fetchEligibility('blocks-everyone')).toBe('ineligible');
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});

// ── A 404 means two different things ─────────────────────────────────────────
//
// Found by Gitar on PR #2888. Spec §6.1 has the endpoint answer
// 404 {"error":"User not found"} for an unknown user, and AR-1 records that
// account existence stays deliberately distinguishable. Treating that as
// "this control-plane predates #1240" latched the gate off process-wide, so a
// single stale member-list entry silently disabled the feature for the session.
describe('friendEligibility — 404 disambiguation', () => {
  const notFoundUser = () =>
    ({
      ok: false,
      status: 404,
      json: async () => ({ error: 'User not found' }),
    }) as unknown as Response;
  const noSuchRoute = () =>
    ({
      ok: false,
      status: 404,
      json: async () => ({ message: '404 page not found' }),
    }) as unknown as Response;

  beforeEach(() => {
    clearFriendEligibilityCache();
    mockApiFetch.mockReset();
  });

  it('does NOT latch when the 404 is a modern server saying the user is unknown', async () => {
    mockApiFetch.mockResolvedValueOnce(notFoundUser());
    expect(await fetchEligibility('deleted-account')).toBe('unknown');

    // The gate must still work for everyone else.
    mockApiFetch.mockResolvedValueOnce(ok(false));
    expect(await fetchEligibility('blocks-everyone')).toBe('ineligible');
    mockApiFetch.mockResolvedValueOnce(ok(true));
    expect(await fetchEligibility('accepts-everyone')).toBe('eligible');
    expect(mockApiFetch).toHaveBeenCalledTimes(3);
  });

  it('DOES latch when the 404 does not carry the contractual shape', async () => {
    mockApiFetch.mockResolvedValueOnce(noSuchRoute());
    expect(await fetchEligibility('someone')).toBe('unknown');
    expect(await fetchEligibility('anyone-else')).toBe('unknown');
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it('latches on a 404 with no JSON body at all', async () => {
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 404,
      json: async () => {
        throw new SyntaxError('not json');
      },
    } as unknown as Response);
    expect(await fetchEligibility('someone')).toBe('unknown');
    expect(await fetchEligibility('anyone-else')).toBe('unknown');
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });
});

// ── Nothing may outlive its cause ────────────────────────────────────────────
//
// Found by the Phase-8 security review. Three pieces of module state were
// effectively permanent for a desktop client that runs for days: an unsettled
// probe (no timeout), the route-missing latch, and a settled verdict.
describe('friendEligibility — bounded lifetimes', () => {
  beforeEach(() => {
    clearFriendEligibilityCache();
    mockApiFetch.mockReset();
    vi.useRealTimers();
  });
  afterEach(() => vi.useRealTimers());

  it('passes an abort signal so a stalled probe cannot pin the cache', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(true));
    await fetchEligibility('u1');
    const init = mockApiFetch.mock.calls[0][1] as RequestInit;
    expect(init?.signal).toBeInstanceOf(AbortSignal);
  });

  it('an aborted probe degrades OPEN and is not cached', async () => {
    mockApiFetch.mockRejectedValueOnce(
      Object.assign(new Error('The operation was aborted.'), { name: 'TimeoutError' })
    );
    expect(await fetchEligibility('u1')).toBe('unknown');
    mockApiFetch.mockResolvedValueOnce(ok(false));
    expect(await fetchEligibility('u1')).toBe('ineligible'); // retried, not pinned
  });

  it('the route-missing latch EXPIRES so the gate re-arms after a rolling deploy', async () => {
    vi.useFakeTimers();
    mockApiFetch.mockResolvedValueOnce(status(404)); // non-contractual body -> latch
    expect(await fetchEligibility('u1')).toBe('unknown');
    expect(await fetchEligibility('u2')).toBe('unknown');
    expect(mockApiFetch).toHaveBeenCalledTimes(1); // latched

    vi.advanceTimersByTime(11 * 60_000);

    mockApiFetch.mockResolvedValueOnce(ok(false));
    expect(await fetchEligibility('u3')).toBe('ineligible'); // gate live again
  });

  it('a settled verdict expires, so a hide cannot outlive its cause', async () => {
    vi.useFakeTimers();
    mockApiFetch.mockResolvedValueOnce(ok(false));
    expect(await fetchEligibility('u1')).toBe('ineligible');
    expect(peekEligibility('u1')).toBe('ineligible');

    // The viewer joins a server the target is in; the old verdict must not stick.
    vi.advanceTimersByTime(6 * 60_000);
    expect(peekEligibility('u1')).toBe('pending');

    mockApiFetch.mockResolvedValueOnce(ok(true));
    expect(await fetchEligibility('u1')).toBe('eligible');
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});

// ── The degrade-open matrix, beyond literal true/false ───────────────────────
//
// Every other test in this file builds the body with `ok(eligible: boolean)`,
// so the key was always present and always a boolean. That left the strict
// comparison unevidenced in BOTH directions: `data.eligible === true` could be
// relaxed to `data.eligible`, and the trailing `return 'unknown'` could be
// turned into `'ineligible'`, with the suite still green. A truthiness test is
// the one fail-CLOSED hole in an otherwise fail-open matrix.
describe('friendEligibility — non-boolean 200 bodies degrade OPEN', () => {
  beforeEach(() => {
    clearFriendEligibilityCache();
    mockApiFetch.mockReset();
  });

  const body = (payload: unknown) =>
    ({ ok: true, status: 200, json: async () => payload }) as unknown as Response;

  it.each([
    ['the key absent entirely (a gateway shim or envelope change)', {}],
    ['an explicit null', { eligible: null }],
    ['a truthy non-boolean number', { eligible: 1 }],
    ['a truthy non-boolean string', { eligible: 'true' }],
  ])('%s yields unknown, never a verdict', async (_label, payload) => {
    mockApiFetch.mockResolvedValueOnce(body(payload));
    expect(await fetchEligibility('u1')).toBe('unknown');
  });

  it('a non-boolean body is not cached, so the next mount retries', async () => {
    mockApiFetch.mockResolvedValueOnce(body({ eligible: 'true' }));
    expect(await fetchEligibility('u1')).toBe('unknown');
    expect(peekEligibility('u1')).toBe('pending');

    mockApiFetch.mockResolvedValueOnce(body({ eligible: false }));
    expect(await fetchEligibility('u1')).toBe('ineligible');
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  // The latch short-circuit in peekEligibility is separate from the one in
  // fetchEligibility, and deleting it survived: the synchronous seed would read
  // `pending`, which on an inline surface means HIDDEN — a wrongly hidden
  // affordance on a server that never had the route.
  it('peekEligibility reports unknown, not pending, while the route-missing latch is set', async () => {
    mockApiFetch.mockResolvedValueOnce(status(404)); // non-contractual body -> latch
    expect(await fetchEligibility('u1')).toBe('unknown');

    expect(peekEligibility('u1')).toBe('unknown');
    expect(peekEligibility('never-probed')).toBe('unknown');
  });
});
