import {
  getMutualServers,
  clearMutualServersCache,
} from '@/renderer/services/system/mutualServers';

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));
import { apiFetch } from '@/renderer/services/system/apiClient';
const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;

const ok = (serverIds: readonly unknown[]) =>
  ({ ok: true, status: 200, json: async () => ({ server_ids: serverIds }) }) as unknown as Response;
const body = (payload: unknown) =>
  ({ ok: true, status: 200, json: async () => payload }) as unknown as Response;
const status = (code: number) =>
  ({ ok: false, status: code, json: async () => ({}) }) as unknown as Response;

describe('mutualServers', () => {
  beforeEach(() => {
    clearMutualServersCache();
    mockApiFetch.mockReset();
  });

  it('maps a 200 body to its server_ids', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(['s1', 's2']));
    expect(await getMutualServers('u1')).toEqual(['s1', 's2']);
  });

  it('calls the :user_id route via apiFetch', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    await getMutualServers('u1');
    expect(mockApiFetch).toHaveBeenCalledWith(
      '/api/v1/users/u1/mutual-servers',
      expect.objectContaining({ signal: expect.anything() })
    );
  });

  it('filters non-string entries out of server_ids', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(['s1', 42, null, 's2']));
    expect(await getMutualServers('u1')).toEqual(['s1', 's2']);
  });

  // Load-bearing: the picker asks about every recipient of a group DM in one
  // pass, so `useMutualServersForAll` calls this once per recipient inside the
  // same render. A value-keyed cache cannot dedupe that first burst — only a
  // promise inserted into `inflight` BEFORE the await can. Kills the mutation
  // that moves `inflight.set(userId, probe)` below the await (or deletes it).
  it('dedupes concurrent calls for the same user to ONE request', async () => {
    let resolveFn!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        resolveFn = r;
      })
    );
    const a = getMutualServers('u1');
    const b = getMutualServers('u1');
    resolveFn(ok(['s1']));
    expect(await a).toEqual(['s1']);
    expect(await b).toEqual(['s1']);
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it('caches a resolved result for the session', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    await getMutualServers('u1');
    await getMutualServers('u1');
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it('404 yields null and latches process-wide so no further user is probed', async () => {
    mockApiFetch.mockResolvedValueOnce(status(404));
    expect(await getMutualServers('u1')).toBeNull();
    expect(await getMutualServers('u2')).toBeNull();
    expect(await getMutualServers('u3')).toBeNull();
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it.each([429, 500, 502, 401, 400])('status %i yields null and is NOT cached', async (code) => {
    mockApiFetch.mockResolvedValue(status(code));
    expect(await getMutualServers('u1')).toBeNull();
    expect(await getMutualServers('u1')).toBeNull();
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('a rejected transport yields null and is not cached', async () => {
    mockApiFetch.mockRejectedValueOnce(new Error('offline'));
    expect(await getMutualServers('u1')).toBeNull();
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('u1')).toEqual(['s1']);
  });

  it('clear empties the cache and releases the unsupported latch', async () => {
    mockApiFetch.mockResolvedValueOnce(status(404));
    await getMutualServers('u1');
    clearMutualServersCache();
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('u1')).toEqual(['s1']);
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});

// ── The strictness of the server_ids guard ───────────────────────────────────
//
// `Array.isArray(data.server_ids)` must reject a truthy, non-array value. A
// truthy check alone would let a duck-typed non-array value through to
// `.filter`, silently trusting a shape the endpoint never emits. A body whose
// `server_ids` is absent, null, or a plain non-array primitive is NOT a
// distinguishing case here: `.filter` on those throws, which the surrounding
// try/catch also maps to `null` — so both the correct guard and a truthy-check
// mutant agree, and asserting on those tells us nothing about which guard is
// live. The one case that DOES distinguish them is a truthy, non-array value
// that duck-types a working `.filter` — only the correct `Array.isArray` guard
// rejects it; a truthy check would let it through and return a result.
describe('mutualServers — server_ids strictness', () => {
  beforeEach(() => {
    clearMutualServersCache();
    mockApiFetch.mockReset();
  });

  it('rejects a truthy array-like value that is not a real Array', async () => {
    const arrayLikeButNotArray = {
      filter: (predicate: (id: unknown) => boolean) => ['s1', 's2'].filter(predicate),
    };
    mockApiFetch.mockResolvedValueOnce(body({ server_ids: arrayLikeButNotArray }));
    expect(await getMutualServers('u1')).toBeNull();
  });

  it('a malformed body is not cached, so the next call retries', async () => {
    mockApiFetch.mockResolvedValueOnce(body({}));
    expect(await getMutualServers('u1')).toBeNull();
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('u1')).toEqual(['s1']);
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});

// ── Nothing may outlive its cause ────────────────────────────────────────────
//
// A desktop client runs for days. An unsettled probe (no timeout), a settled
// result, and the route-missing latch must all be bounded, not effectively
// permanent.
describe('mutualServers — bounded lifetimes', () => {
  beforeEach(() => {
    clearMutualServersCache();
    mockApiFetch.mockReset();
    vi.useRealTimers();
  });
  afterEach(() => vi.useRealTimers());

  it('passes an abort signal so a stalled probe cannot pin the cache', async () => {
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    await getMutualServers('u1');
    const init = mockApiFetch.mock.calls[0][1] as RequestInit;
    expect(init?.signal).toBeInstanceOf(AbortSignal);
  });

  it('an aborted probe degrades to null and is not cached', async () => {
    mockApiFetch.mockRejectedValueOnce(
      Object.assign(new Error('The operation was aborted.'), { name: 'TimeoutError' })
    );
    expect(await getMutualServers('u1')).toBeNull();
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('u1')).toEqual(['s1']); // retried, not pinned
  });

  it('a settled result expires, so membership changes are not stuck for the session', async () => {
    vi.useFakeTimers();
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('u1')).toEqual(['s1']);
    expect(mockApiFetch).toHaveBeenCalledTimes(1);

    // The viewer's shared-server membership with u1 changes mid-session.
    vi.advanceTimersByTime(6 * 60_000);

    mockApiFetch.mockResolvedValueOnce(ok(['s2']));
    expect(await getMutualServers('u1')).toEqual(['s2']);
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('the route-missing latch expires so greying re-arms after a rolling restart', async () => {
    vi.useFakeTimers();
    mockApiFetch.mockResolvedValueOnce(status(404));
    expect(await getMutualServers('u1')).toBeNull();
    expect(await getMutualServers('u2')).toBeNull();
    expect(mockApiFetch).toHaveBeenCalledTimes(1); // latched

    vi.advanceTimersByTime(11 * 60_000);

    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('u3')).toEqual(['s1']); // gate live again
  });
});

// ── The identity fence on reset ──────────────────────────────────────────────
//
// Found by the same class of review that caught it in friendEligibility.
// Clearing is synchronous; a probe already in flight is not. Its `.then`
// continuation runs AFTER the clear and, without the generation fence, writes
// straight back into the structures the clear just emptied — carrying account
// A's shared-server list into account B's session on the same process.
describe('mutualServers — identity fence on reset', () => {
  beforeEach(() => {
    clearMutualServersCache();
    mockApiFetch.mockReset();
  });

  it('discards a result that settles after clearMutualServersCache instead of caching it for the next account', async () => {
    let respond!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        respond = r;
      })
    );
    // Account A opens a group-DM picker on victim's profile; the probe is
    // still in flight.
    const inFlight = getMutualServers('victim');

    // A logs out. resetService clears the cache.
    clearMutualServersCache();

    // A's probe lands now, naming server the ATTACKER account shares with
    // victim.
    respond(ok(['attacker-shared-server']));
    await inFlight;

    // Account B signs in on the same process and asks about the same id. If
    // the fence were missing, A's answer would have been cached and this call
    // would be served from it with no second request.
    mockApiFetch.mockResolvedValueOnce(ok(['b-shared-server']));
    expect(await getMutualServers('victim')).toEqual(['b-shared-server']);
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('does not let a pre-reset 404 latch disable greying on the next server', async () => {
    let respond!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        respond = r;
      })
    );
    // Account A is on an old self-hosted control-plane with no such route.
    const inFlight = getMutualServers('someone');

    clearMutualServersCache();

    // The old server's 404 arrives after the reset.
    respond(status(404));
    await inFlight;

    // B connects to a control-plane that DOES implement the route. Greying
    // must be live again, not latched off for the whole session.
    mockApiFetch.mockResolvedValueOnce(ok(['s1']));
    expect(await getMutualServers('someone-else')).toEqual(['s1']);
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });
});
