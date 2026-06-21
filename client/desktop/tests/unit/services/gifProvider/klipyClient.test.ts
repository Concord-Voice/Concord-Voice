import { klipyClient, rewriteMediaUrl } from '@/renderer/services/gifProvider/klipyClient';
import { API_BASE } from '@/renderer/config';
import { resetAllStores } from '../../../helpers/store-helpers';

// All KLIPY traffic now goes through the control-plane proxy via apiFetch.
// There is no longer a "direct mode" — the renderer never speaks to api.klipy.com.
const apiFetchMock = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
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
    // Fully reset the singleton client (clears cached customer_id between tests)
    (klipyClient as unknown as { _resetForTesting: () => void })._resetForTesting();
    localStorage.clear();
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
});

describe('rewriteMediaUrl', () => {
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
