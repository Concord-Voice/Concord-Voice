import {
  klipyClient,
  rewriteMediaUrl,
  isValidGifSlug,
  isValidCustomerId,
  KlipyIdentityError,
} from '@/renderer/services/messaging/gifProvider/klipyClient';
import { API_BASE } from '@/renderer/config';
import {
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';

// All KLIPY traffic now goes through the control-plane proxy via apiFetch.
// There is no longer a "direct mode" — the renderer never speaks to api.klipy.com.
const apiFetchMock = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => apiFetchMock(...args),
}));

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

describe('klipyClient', () => {
  beforeEach(() => {
    resetAllStores();
    apiFetchMock.mockReset();
    resetRuntimeServerBase();
    // Fully reset the singleton client (clears cached customer_id between tests)
    (klipyClient as unknown as { _resetForTesting: () => void })._resetForTesting();
    localStorage.clear();
  });

  afterEach(() => {
    resetRuntimeServerBase();
  });

  describe('proxy routing', () => {
    it('uses apiFetch and routes through /api/v1/klipy/* for all requests', async () => {
      klipyClient.setPersonalizationEnabled(false); // skip customer_id fetch
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      expect(apiFetchMock).toHaveBeenCalled();
      const url = apiFetchMock.mock.calls[0][0] as string;
      expect(url).toContain('/api/v1/klipy/gifs/trending');
      // The URL must NEVER reference api.klipy.com directly
      expect(url).not.toContain('api.klipy.com');
    });
  });

  describe('customer_id personalization', () => {
    it('sends an ephemeral customer_id when personalization is OFF', async () => {
      klipyClient.setPersonalizationEnabled(false);
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      const url = apiFetchMock.mock.calls[0][0] as string;
      // Ephemeral UUID is always sent (KLIPY requires customer_id).
      // It must be a valid UUID v4 and NOT fetched from the control-plane.
      expect(url).toContain('customer_id=');
      expect(apiFetchMock).toHaveBeenCalledTimes(1); // no /customer-id network call
    });

    it('fetches and caches customer_id from /customer-id when personalization is ON', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock
        .mockResolvedValueOnce(jsonResponse({ customer_id: 'cust-abc-123' }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      expect(apiFetchMock).toHaveBeenCalledTimes(2);
      const firstCallUrl = apiFetchMock.mock.calls[0][0] as string;
      const firstCallInit = apiFetchMock.mock.calls[0][1] as RequestInit;
      expect(firstCallUrl).toBe('/api/v1/klipy/customer-id');
      expect(firstCallInit?.method).toBe('POST');
      expect(apiFetchMock.mock.calls[1][0]).toContain('customer_id=cust-abc-123');
    });

    it('persists customer_id to localStorage and reuses it on subsequent calls', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock
        .mockResolvedValueOnce(jsonResponse({ customer_id: 'cust-xyz' }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      await klipyClient.trending(2, 25);
      const idCalls = apiFetchMock.mock.calls.filter((call) =>
        (call[0] as string).includes('/customer-id')
      );
      expect(idCalls).toHaveLength(1);
      expect(localStorage.getItem('concord:klipy-customer-id')).toBe('cust-xyz');
    });

    it('reads cached customer_id from localStorage on first use', async () => {
      localStorage.setItem('concord:klipy-customer-id', 'pre-cached-id');
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      const idCalls = apiFetchMock.mock.calls.filter((call) =>
        (call[0] as string).includes('/customer-id')
      );
      expect(idCalls).toHaveLength(0);
      const trendingUrl = apiFetchMock.mock.calls[0][0] as string;
      expect(trendingUrl).toContain('customer_id=pre-cached-id');
    });

    it('does not carry a customer_id FAILURE across a runtime server change', async () => {
      // A 60s backoff recorded against the server the user just left would be
      // inherited by the one they joined, so recent() throws KlipyIdentityError
      // there even though that server's endpoint is healthy.
      klipyClient.setPersonalizationEnabled(true);
      let release!: (r: Response) => void;
      apiFetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            release = resolve;
          })
      );

      const pending = klipyClient.getCustomerID();
      setRuntimeServerBase('https://other-instance.example.com');
      release(jsonResponse({ error: 'nope' }, 500));
      await expect(pending).resolves.toBeNull();

      // The new server must be free to try: a fresh request is issued rather
      // than short-circuited by the previous server's backoff.
      apiFetchMock.mockResolvedValueOnce(jsonResponse({ customer_id: 'fresh-id' }));
      await expect(klipyClient.getCustomerID()).resolves.toBe('fresh-id');
    });

    it.each([
      [400, 'client-bug'],
      [429, 'backpressure'],
      [500, 'upstream'],
      [503, 'upstream'],
    ])('classifies a %i share rejection as %s', async (status, expected) => {
      // The comment above this classification named three classes while the
      // code collapsed 429 into `upstream`, so the two drifted apart unnoticed.
      // 429 is backpressure we deliberately do not retry; an operator
      // aggregating on failureClass cannot tell that from a vendor outage
      // unless the class says so.
      klipyClient.setPersonalizationEnabled(false);
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      apiFetchMock.mockResolvedValueOnce(jsonResponse({ error: 'no' }, status));

      await klipyClient.notifyShared('good-slug');

      // LAST matching call, not the first: console.warn accumulates across the
      // it.each cases, so `find` can read a NEIGHBOURING case's call and assert
      // against the wrong status. Caught when falsifying — collapsing the 429
      // branch failed the 500 and 503 cases too, because they were reading the
      // 429 case's entry rather than their own.
      const calls = warn.mock.calls.filter((c) => String(c[0]).includes('upstream rejected'));
      expect(calls.length).toBeGreaterThan(0);
      expect(calls[calls.length - 1][1]).toMatchObject({ status, failureClass: expected });
      warn.mockRestore();
    });

    it('does not send a share to a server joined mid-flight', async () => {
      // Rejecting the stale identifier is not enough: the SHARE itself — the
      // slug and the search term in ctx.q — would still be POSTed to whichever
      // server is current after the await. Unlike the earlier mid-flight test
      // that had to be deleted for vacuity, this one asserts on the REQUEST
      // COUNT, which discriminates regardless of what the continuation returns.
      klipyClient.setPersonalizationEnabled(true);
      let release!: (r: Response) => void;
      apiFetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            release = resolve;
          })
      );

      const pending = klipyClient.notifyShared('good-slug', { q: 'private search term' });
      setRuntimeServerBase('https://other-instance.example.com');
      release(jsonResponse({ customer_id: 'minted-by-the-previous-server' }));
      await pending;

      const shareCalls = apiFetchMock.mock.calls.filter((c) =>
        String(c[0]).includes('/gifs/share/')
      );
      expect(shareCalls).toHaveLength(0);
    });

    it('does not hand a caller on the new server the promise begun on the old one', async () => {
      // The in-flight handle exists so concurrent callers share ONE request.
      // It is only shareable within the selection it started under: once the
      // stale-selection guard makes the old promise resolve null by design, a
      // caller on the NEW server that joins it inherits that null — recent()
      // reports an identity error and shares go unattributed on a server whose
      // endpoint is perfectly healthy.
      klipyClient.setPersonalizationEnabled(true);
      let releaseA!: (r: Response) => void;
      apiFetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            releaseA = resolve;
          })
      );

      const onA = klipyClient.getCustomerID();
      setRuntimeServerBase('https://server-b.example.com');

      apiFetchMock.mockResolvedValueOnce(jsonResponse({ customer_id: 'minted-by-b' }));
      const onB = klipyClient.getCustomerID();

      releaseA(jsonResponse({ customer_id: 'minted-by-a' }));

      await expect(onA).resolves.toBeNull();
      await expect(onB).resolves.toBe('minted-by-b');
    });

    it('discards an in-flight customer_id when the runtime server changes', async () => {
      // The state check that selects the server happens BEFORE the await; it
      // says nothing about the world after it. Without the fence, a value
      // minted by the server the user just left is cached and persisted
      // against the server they just joined.
      klipyClient.setPersonalizationEnabled(true);
      let release!: (r: Response) => void;
      apiFetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            release = resolve;
          })
      );

      const pending = klipyClient.getCustomerID();
      setRuntimeServerBase('https://other-instance.example.com');
      release(jsonResponse({ customer_id: 'minted-by-the-previous-server' }));

      await expect(pending).resolves.toBeNull();
      expect(localStorage.getItem('concord:klipy-customer-id')).toBeNull();
    });

    it('returns null and skips customer_id when /customer-id fails', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock
        .mockResolvedValueOnce(jsonResponse({ error: 'oops' }, 500))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      expect(apiFetchMock.mock.calls[1][0]).not.toContain('customer_id=');
    });

    // --- Failure cache + single in-flight promise (#571 item #15) ---

    it('caches /customer-id failure and does NOT retry within the TTL window', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock
        .mockResolvedValueOnce(jsonResponse({ error: 'not found' }, 404))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      await klipyClient.trending(2, 25);
      const idCalls = apiFetchMock.mock.calls.filter((call) =>
        (call[0] as string).includes('/customer-id')
      );
      expect(idCalls).toHaveLength(1);
    });

    it('retries /customer-id after the 60s failure cache expires', async () => {
      vi.useFakeTimers();
      try {
        klipyClient.setPersonalizationEnabled(true);
        apiFetchMock
          .mockResolvedValueOnce(jsonResponse({ error: 'not found' }, 404))
          .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }))
          .mockResolvedValueOnce(jsonResponse({ customer_id: 'later' }))
          .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
        await klipyClient.trending(1, 25);
        vi.setSystemTime(Date.now() + 61_000);
        await klipyClient.trending(2, 25);
        const idCalls = apiFetchMock.mock.calls.filter((call) =>
          (call[0] as string).includes('/customer-id')
        );
        expect(idCalls).toHaveLength(2);
      } finally {
        vi.useRealTimers();
      }
    });

    it('coalesces concurrent /customer-id callers into a single in-flight request', async () => {
      klipyClient.setPersonalizationEnabled(true);
      let resolveCustomerId: (v: Response) => void = () => {};
      const pending = new Promise<Response>((r) => {
        resolveCustomerId = r;
      });
      apiFetchMock
        .mockReturnValueOnce(pending)
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      const p1 = klipyClient.getCustomerID();
      const p2 = klipyClient.getCustomerID();
      resolveCustomerId(jsonResponse({ customer_id: 'shared-id' }));
      const [id1, id2] = await Promise.all([p1, p2]);
      expect(id1).toBe('shared-id');
      expect(id2).toBe('shared-id');
      const idCalls = apiFetchMock.mock.calls.filter((call) =>
        (call[0] as string).includes('/customer-id')
      );
      expect(idCalls).toHaveLength(1);
    });

    it('caches a successful response indefinitely across many subsequent calls', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock
        .mockResolvedValueOnce(jsonResponse({ customer_id: 'sticky' }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      await klipyClient.trending(1, 25);
      await klipyClient.trending(2, 25);
      await klipyClient.trending(3, 25);
      const idCalls = apiFetchMock.mock.calls.filter((call) =>
        (call[0] as string).includes('/customer-id')
      );
      expect(idCalls).toHaveLength(1);
    });
  });

  describe('endpoint methods', () => {
    beforeEach(() => {
      // Skip customer_id for these tests so we have one fetch per call
      klipyClient.setPersonalizationEnabled(false);
    });

    it('search forwards the query, page, per_page, and locale', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [], has_more: false }));
      await klipyClient.search('cats', 2, 10, 'fr');
      const url = apiFetchMock.mock.calls[0][0] as string;
      expect(url).toContain('/api/v1/klipy/gifs/search');
      expect(url).toContain('q=cats');
      expect(url).toContain('page=2');
      expect(url).toContain('per_page=10');
      expect(url).toContain('locale=fr');
      expect(url).toContain('format_filter=mp4%2Cgif%2Cwebp');
    });

    it('search throws on non-OK upstream response', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ error: 'oops' }, 500));
      await expect(klipyClient.search('cats', 1, 25)).rejects.toThrow(/search failed: 500/);
    });

    it('trending throws on non-OK upstream response', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ error: 'oops' }, 500));
      await expect(klipyClient.trending(1, 25)).rejects.toThrow(/trending failed: 500/);
    });

    it('recent returns empty when personalization is OFF (no customer_id)', async () => {
      klipyClient.setPersonalizationEnabled(false);
      const r = await klipyClient.recent(1, 25);
      expect(r.data).toEqual([]);
      expect(r.has_more).toBe(false);
      expect(apiFetchMock).not.toHaveBeenCalled();
    });

    it('recent calls the customer-scoped endpoint when personalization is ON', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock
        .mockResolvedValueOnce(jsonResponse({ customer_id: 'recent-customer' }))
        .mockResolvedValueOnce(jsonResponse({ data: [], has_more: false }));
      await klipyClient.recent(1, 25);
      const recentCall = apiFetchMock.mock.calls[1][0] as string;
      expect(recentCall).toContain('/api/v1/klipy/gifs/recent/recent-customer');
    });

    it('categories optionally forwards locale', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [] }));
      await klipyClient.categories('en');
      const url = apiFetchMock.mock.calls[0][0] as string;
      expect(url).toContain('/gifs/categories?locale=en');
    });

    it('categories without locale omits the query string', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [] }));
      await klipyClient.categories();
      const url = apiFetchMock.mock.calls[0][0] as string;
      expect(url).toContain('/gifs/categories');
      expect(url).not.toContain('?');
    });

    it('getBySlug returns the first item from the items endpoint', async () => {
      apiFetchMock.mockResolvedValue(
        jsonResponse({
          data: [{ slug: 'abc', file: { gif: { url: 'https://media.klipy.com/abc.gif' } } }],
        })
      );
      const item = await klipyClient.getBySlug('abc');
      expect(item).not.toBeNull();
      expect(item?.slug).toBe('abc');
    });

    it('getBySlug returns null on non-OK response', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ error: 'nope' }, 404));
      const item = await klipyClient.getBySlug('missing');
      expect(item).toBeNull();
    });

    it('getBySlug returns null when the response has no items', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({ data: [] }));
      const item = await klipyClient.getBySlug('empty');
      expect(item).toBeNull();
    });

    it('notifyShared sends a POST to /gifs/share/{slug}', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({}));
      await klipyClient.notifyShared('test-slug');
      expect(apiFetchMock).toHaveBeenCalled();
      const url = apiFetchMock.mock.calls[0][0] as string;
      const init = apiFetchMock.mock.calls[0][1] as RequestInit;
      expect(url).toContain('/gifs/share/test-slug');
      expect(init?.method).toBe('POST');
    });

    it('notifyShared swallows errors', async () => {
      apiFetchMock.mockRejectedValue(new Error('boom'));
      await expect(klipyClient.notifyShared('test-slug')).resolves.toBeUndefined();
    });

    it('report sends a POST to /gifs/report/{slug}', async () => {
      apiFetchMock.mockResolvedValue(jsonResponse({}));
      await klipyClient.report('bad-slug');
      const url = apiFetchMock.mock.calls[0][0] as string;
      const init = apiFetchMock.mock.calls[0][1] as RequestInit;
      expect(url).toContain('/gifs/report/bad-slug');
      expect(init?.method).toBe('POST');
    });

    it('media binaries are not handled by klipyClient — only API calls', async () => {
      // The media proxy is hit directly via <img src> / <video src>, not via klipyClient
      // This test just confirms there's no media-related public method on the client
      // (defensive — if someone adds one in the future, this test breaks and reminds them
      // to update the privacy/proxy reasoning).
      const methods = Object.keys(klipyClient).filter(
        (k) => typeof (klipyClient as Record<string, unknown>)[k] === 'function'
      );
      expect(methods.find((m) => m.toLowerCase().includes('media'))).toBeUndefined();
    });
  });

  describe('report() slug guard (#2580, second sink)', () => {
    it.each([['..'], ['../../users/me'], ['%2e%2e%2f'], ['a/b'], ['']])(
      'issues ZERO requests for %s',
      async (slug) => {
        klipyClient.setPersonalizationEnabled(false);
        apiFetchMock.mockResolvedValue(jsonResponse({}));
        await expect(klipyClient.report(slug)).rejects.toThrow();
        expect(apiFetchMock).not.toHaveBeenCalled();
      }
    );

    it('still issues exactly one request for a valid slug', async () => {
      klipyClient.setPersonalizationEnabled(false);
      apiFetchMock.mockResolvedValue(jsonResponse({}));
      await klipyClient.report('good-slug');
      expect(apiFetchMock).toHaveBeenCalledTimes(1);
    });
  });

  describe('notifyShared (A2 + #2580)', () => {
    it('sends customer_id when personalization is ON', async () => {
      klipyClient.setPersonalizationEnabled(true);
      localStorage.setItem('concord:klipy-customer-id', 'abc-123');
      apiFetchMock.mockResolvedValue(jsonResponse({ result: true }));

      await klipyClient.notifyShared('good-slug');

      expect(apiFetchMock).toHaveBeenCalledTimes(1);
      const path = apiFetchMock.mock.calls[0][0] as string;
      expect(path).toContain('/gifs/share/good-slug');
      expect(path).toContain('customer_id=abc-123');
    });

    it('OMITS customer_id when personalization is OFF', async () => {
      klipyClient.setPersonalizationEnabled(false);
      apiFetchMock.mockResolvedValue(jsonResponse({ result: true }));

      await klipyClient.notifyShared('good-slug');

      expect(apiFetchMock).toHaveBeenCalledTimes(1);
      expect(apiFetchMock.mock.calls[0][0] as string).not.toContain('customer_id');
    });

    it('forwards q when the send originated from a search', async () => {
      klipyClient.setPersonalizationEnabled(false);
      apiFetchMock.mockResolvedValue(jsonResponse({ result: true }));

      await klipyClient.notifyShared('good-slug', { q: 'happy dance' });

      expect(apiFetchMock.mock.calls[0][0] as string).toContain('q=happy+dance');
    });

    it.each([
      ['..'],
      ['../../users/me'],
      ['%2e%2e%2f'],
      ['a/b'],
      ['a\\b'],
      ['a?b'],
      ['a#b'],
      [''],
      ['a'.repeat(101)],
    ])('issues ZERO requests for the invalid slug %s', async (slug) => {
      // Personalization ON is the strict case: an implementation that resolves
      // customer_id BEFORE validating would issue one customer-id request here
      // and fail AC9 on a technicality a PoC will find.
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock.mockResolvedValue(jsonResponse({ result: true }));

      await klipyClient.notifyShared(slug);

      expect(apiFetchMock).not.toHaveBeenCalled();
    });

    it('never rejects, and emits a signal, when the upstream fails', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      klipyClient.setPersonalizationEnabled(false);
      apiFetchMock.mockResolvedValue(jsonResponse({ error: 'nope' }, 429));

      await expect(klipyClient.notifyShared('good-slug')).resolves.toBeUndefined();
      expect(warn).toHaveBeenCalled();
      // The slug must never appear in the log line (observability symmetry).
      expect(JSON.stringify(warn.mock.calls)).not.toContain('good-slug');
      warn.mockRestore();
    });

    it('never rejects when the transport itself throws', async () => {
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      klipyClient.setPersonalizationEnabled(false);
      apiFetchMock.mockRejectedValue(new Error('offline'));

      await expect(klipyClient.notifyShared('good-slug')).resolves.toBeUndefined();
      expect(warn).toHaveBeenCalled();
      warn.mockRestore();
    });
  });

  describe('recent() identity failures (A3)', () => {
    it('THROWS when personalization is ON and the customer id cannot be resolved', async () => {
      // Previously returned a fabricated { data: [], has_more: false } — a
      // success shape indistinguishable from "you have no recents", which would
      // mask the A2 fix during verification.
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock.mockResolvedValue(jsonResponse({ error: 'unauthorized' }, 401));

      await expect(klipyClient.recent(1, 25)).rejects.toBeInstanceOf(KlipyIdentityError);
    });

    it('returns a legitimate empty when personalization is OFF', async () => {
      // Not an error: the Recent tab is hidden in this mode.
      klipyClient.setPersonalizationEnabled(false);
      await expect(klipyClient.recent(1, 25)).resolves.toEqual({ data: [], has_more: false });
      expect(apiFetchMock).not.toHaveBeenCalled();
    });

    it('force skips the customer-id failure backoff', async () => {
      klipyClient.setPersonalizationEnabled(true);
      apiFetchMock.mockResolvedValue(jsonResponse({ error: 'nope' }, 500));
      await expect(klipyClient.recent(1, 25)).rejects.toBeInstanceOf(KlipyIdentityError);
      const callsAfterFirst = apiFetchMock.mock.calls.length;

      // Without force, the 60s TTL short-circuits and issues no new request.
      await expect(klipyClient.recent(1, 25)).rejects.toBeInstanceOf(KlipyIdentityError);
      expect(apiFetchMock.mock.calls.length).toBe(callsAfterFirst);

      // With force, the backoff is cleared and the id fetch is retried.
      await expect(klipyClient.recent(1, 25, { force: true })).rejects.toBeInstanceOf(
        KlipyIdentityError
      );
      expect(apiFetchMock.mock.calls.length).toBeGreaterThan(callsAfterFirst);
    });
  });
});

describe('isValidGifSlug (#2580)', () => {
  it('accepts a well-formed slug', () => {
    expect(isValidGifSlug('funny-cat-123')).toBe(true);
    expect(isValidGifSlug('a')).toBe(true);
    expect(isValidGifSlug('A'.repeat(100))).toBe(true);
  });

  // The empty string is the case where this DELIBERATELY DIVERGES from the Go
  // ValidateSlug, which returns true for "" ("optional attached-slug field is
  // well-formed"). Here the question is "may I interpolate this into a path?"
  // and the answer for "" is no. A naive mirror of the Go function reopens A2.
  it('rejects the empty string, unlike the Go ValidateSlug', () => {
    expect(isValidGifSlug('')).toBe(false);
  });

  it.each([
    ['dot segments', '../../users/me'],
    ['single dot segment', '..'],
    ['encoded dot segments', '%2e%2e%2f'],
    ['forward slash', 'a/b'],
    ['backslash', 'a\\b'],
    ['query introducer', 'a?b=c'],
    ['fragment introducer', 'a#b'],
    ['unicode fraction slash', 'a⁄b'],
    ['unicode fullwidth solidus', 'a／b'],
    ['newline', 'a\nb'],
    ['space', 'a b'],
    ['underscore', 'a_b'],
    ['oversized (101 chars)', 'a'.repeat(101)],
  ])('rejects %s', (_label, slug) => {
    expect(isValidGifSlug(slug)).toBe(false);
  });
});

describe('rewriteMediaUrl', () => {
  beforeEach(() => {
    resetRuntimeServerBase();
  });

  afterEach(() => {
    resetRuntimeServerBase();
  });

  // The rewritten URL MUST be absolute — see the regression-guard test below
  // and the comment on rewriteMediaUrl in klipyClient.ts for context.
  const proxy = (u: string) => `${API_BASE}/api/v1/klipy/media?url=${encodeURIComponent(u)}`;

  // Regression guard for the bundled-mode regression introduced by PR #830.
  // The rewritten URL is consumed directly by <img src> / <video src>, which
  // resolves relative URLs against the renderer origin (`app://concord/` in
  // bundled mode after #830). A relative URL therefore dead-ends at the asar
  // protocol handler with net::ERR_UNEXPECTED. The URL MUST be absolute AND
  // MUST target the configured API host — binding to API_BASE explicitly
  // closes the "wrong absolute host" gap that a loose `^https?://` regex
  // would miss.
  it('returns an absolute URL targeting API_BASE so <img src> resolves against the API host', () => {
    const result = rewriteMediaUrl('https://media.klipy.com/a.mp4');
    expect(result?.startsWith(API_BASE)).toBe(true);
    expect(result).toContain('/api/v1/klipy/media?url=');
  });

  it('rewrites klipy.com subdomains', () => {
    expect(rewriteMediaUrl('https://media.klipy.com/a.mp4')).toBe(
      proxy('https://media.klipy.com/a.mp4')
    );
  });

  it('rewrites through the active runtime API base', () => {
    setRuntimeServerBase('https://homelab.lan:8443');

    expect(rewriteMediaUrl('https://media.klipy.com/a.mp4')).toBe(
      `https://homelab.lan:8443/api/v1/klipy/media?url=${encodeURIComponent('https://media.klipy.com/a.mp4')}`
    );
  });

  it('rewrites klipy.io subdomains', () => {
    expect(rewriteMediaUrl('https://cdn.klipy.io/a.gif')).toBe(proxy('https://cdn.klipy.io/a.gif'));
  });

  it('rewrites apex klipy.com', () => {
    expect(rewriteMediaUrl('https://klipy.com/a.gif')).toBe(proxy('https://klipy.com/a.gif'));
  });

  it('does not rewrite non-KLIPY URLs', () => {
    const url = 'https://example.com/image.gif';
    expect(rewriteMediaUrl(url)).toBe(url);
  });

  // Defense in depth — explicit guard that non-KLIPY URLs aren't accidentally
  // proxied through the API. The strict-equality test above already covers this
  // implicitly, but a named invariant makes the intent visible to future readers
  // and prevents a regression where `API_BASE` leaks into the passthrough path.
  it('does not prefix API_BASE onto non-KLIPY URLs', () => {
    const url = 'https://example.com/image.gif';
    const result = rewriteMediaUrl(url);
    expect(result?.startsWith(API_BASE)).toBe(false);
  });

  it('does not rewrite URLs that merely contain "klipy" in path', () => {
    const url = 'https://example.com/klipy/image.gif';
    expect(rewriteMediaUrl(url)).toBe(url);
  });

  it('returns undefined unchanged', () => {
    expect(rewriteMediaUrl(undefined)).toBeUndefined();
  });

  it('returns empty string unchanged', () => {
    expect(rewriteMediaUrl('')).toBe('');
  });

  it('handles URLs with query params', () => {
    const url = 'https://media.klipy.com/a.mp4?token=abc&size=hd';
    expect(rewriteMediaUrl(url)).toBe(proxy(url));
  });
});

describe('customer id is untrusted input (#2371 red-team VULN-002)', () => {
  // The stored id is NOT scoped per server: a hostile self-hosted instance can
  // mint one, it persists, and it is then reused against the official API with
  // the official bearer token. Same class as #2580, different sink.
  const POISON = '../../../users/me/sessions';

  it('rejects a path-traversal customer id', () => {
    expect(isValidCustomerId(POISON)).toBe(false);
    expect(isValidCustomerId('')).toBe(false);
    expect(isValidCustomerId('a/b')).toBe(false);
    expect(isValidCustomerId('7f3c9a10-2b4e-4d6f-9a01-1c2d3e4f5a6b')).toBe(true);
  });

  it('discards a poisoned stored id instead of interpolating it into the path', async () => {
    localStorage.setItem('concord:klipy-customer-id', POISON);
    klipyClient.setPersonalizationEnabled(true);
    // Minting a fresh id must be what happens next, not reuse of the poison.
    apiFetchMock.mockResolvedValue(jsonResponse({ customer_id: 'clean-id-123' }));

    await klipyClient.recent(1, 25).catch(() => undefined);

    const paths = apiFetchMock.mock.calls.map((c) => String(c[0]));
    expect(paths.some((p) => p.includes('users/me/sessions'))).toBe(false);
    expect(paths.some((p) => p.includes('..'))).toBe(false);
    expect(localStorage.getItem('concord:klipy-customer-id')).not.toBe(POISON);
  });

  it('refuses to persist an ill-formed id handed back by the server', async () => {
    klipyClient.setPersonalizationEnabled(true);
    apiFetchMock.mockResolvedValue(jsonResponse({ customer_id: POISON }));

    await klipyClient.recent(1, 25).catch(() => undefined);

    expect(localStorage.getItem('concord:klipy-customer-id')).toBeNull();
    const paths = apiFetchMock.mock.calls.map((c) => String(c[0]));
    expect(paths.some((p) => p.includes('users/me/sessions'))).toBe(false);
  });
});

describe('opting out mid-flight (Codex P1 — async continuation)', () => {
  // A state check made BEFORE an await does not hold after it. The user can
  // disable personalization while /customer-id is still in flight; the
  // continuation must re-read the flag, not trust the value it captured.
  // NOT TESTED HERE, deliberately, and the gap is real: the notifyShared
  // equivalent of the case below cannot be staged in this harness.
  // setPersonalizationEnabled(false) clears customerIdInFlight, so the
  // continuation re-fetches and resolves null whether or not the post-await
  // recheck exists — three staging attempts could not make the assertion
  // discriminate. The recheck in notifyShared is therefore defensive and
  // unverified by test; the recent() case below IS verified and covers the
  // same class. Do not add a green test here without falsifying it first.

  it('recent does not issue a request when personalization is disabled mid-flight', async () => {
    klipyClient.setPersonalizationEnabled(true);
    // Build the deferred BEFORE wiring the mock: assigning `release` inside the
    // implementation makes it exist only once the mock is invoked.
    let release!: (r: Response) => void;
    const idInFlight = new Promise<Response>((res) => {
      release = res;
    });
    apiFetchMock.mockImplementationOnce(() => idInFlight);
    apiFetchMock.mockResolvedValue(jsonResponse({ data: [], has_more: false }));

    const p = klipyClient.recent(1, 25);
    klipyClient.setPersonalizationEnabled(false);
    release(jsonResponse({ customer_id: 'abc-123' }));
    await p.catch(() => undefined);

    const paths = apiFetchMock.mock.calls.map((c) => String(c[0]));
    expect(paths.some((path) => path.includes('/gifs/recent/'))).toBe(false);
  });
});
