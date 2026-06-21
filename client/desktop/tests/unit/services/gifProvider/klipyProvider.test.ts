import { klipyProvider } from '@/renderer/services/gifProvider/klipyProvider';
import type { KlipyGifItem } from '@/renderer/services/gifProvider/klipyClient';
import { API_BASE } from '@/renderer/config';

// Mock the low-level klipyClient so we can verify klipyProvider's translation logic
// without making any real HTTP calls.
const trendingMock = vi.fn();
const searchMock = vi.fn();
const recentMock = vi.fn();
const categoriesMock = vi.fn();
const getBySlugMock = vi.fn();
const notifySharedMock = vi.fn();
const reportMock = vi.fn();
const setPersonalizationEnabledMock = vi.fn();

vi.mock('@/renderer/services/gifProvider/klipyClient', async (importOriginal) => {
  // Keep the real rewriteMediaUrl so URL-rewrite assertions stay accurate.
  const original =
    await importOriginal<typeof import('@/renderer/services/gifProvider/klipyClient')>();
  return {
    ...original,
    klipyClient: {
      trending: (...args: unknown[]) => trendingMock(...args),
      search: (...args: unknown[]) => searchMock(...args),
      recent: (...args: unknown[]) => recentMock(...args),
      categories: (...args: unknown[]) => categoriesMock(...args),
      getBySlug: (...args: unknown[]) => getBySlugMock(...args),
      notifyShared: (...args: unknown[]) => notifySharedMock(...args),
      report: (...args: unknown[]) => reportMock(...args),
      setPersonalizationEnabled: (...args: unknown[]) => setPersonalizationEnabledMock(...args),
    },
  };
});

const mockMp4Item: KlipyGifItem = {
  slug: 'mp4-only',
  width: 320,
  height: 240,
  file: {
    mp4: { url: 'https://media.klipy.com/m.mp4', width: 320, height: 240 },
  },
  still: { url: 'https://media.klipy.com/m.jpg' },
};

const mockGifItem: KlipyGifItem = {
  slug: 'gif-only',
  width: 200,
  height: 200,
  file: {
    gif: { url: 'https://media.klipy.com/g.gif', width: 200, height: 200 },
  },
};

const mockWebpItem: KlipyGifItem = {
  slug: 'webp-only',
  width: 200,
  height: 200,
  file: {
    webp: { url: 'https://media.klipy.com/w.webp' },
  },
};

const mockMixedItem: KlipyGifItem = {
  slug: 'all-three',
  width: 480,
  height: 270,
  file: {
    mp4: { url: 'https://media.klipy.com/all.mp4' },
    webp: { url: 'https://media.klipy.com/all.webp' },
    gif: { url: 'https://media.klipy.com/all.gif' },
  },
  still: { url: 'https://media.klipy.com/all.jpg' },
};

describe('klipyProvider', () => {
  beforeEach(() => {
    trendingMock.mockReset();
    searchMock.mockReset();
    recentMock.mockReset();
    categoriesMock.mockReset();
    getBySlugMock.mockReset();
    notifySharedMock.mockReset();
    reportMock.mockReset();
    setPersonalizationEnabledMock.mockReset();
  });

  describe('static metadata', () => {
    it('exposes the correct vendor name and attribution strings', () => {
      expect(klipyProvider.name).toBe('KLIPY');
      expect(klipyProvider.searchPlaceholder).toBe('Search KLIPY');
      expect(klipyProvider.poweredByText).toBe('Powered by KLIPY');
    });

    it('declares support for recent and categories', () => {
      expect(klipyProvider.supportsRecent).toBe(true);
      expect(klipyProvider.supportsCategories).toBe(true);
    });

    it('exposes light + dark logo asset paths', () => {
      expect(klipyProvider.logoAssetLight).toBe('./branding/KLIPY/klipy-logo-light.svg');
      expect(klipyProvider.logoAssetDark).toBe('./branding/KLIPY/klipy-logo-dark.svg');
    });

    it('includes the independence disclaimer required by the ToS', () => {
      expect(klipyProvider.independenceDisclaimer).toMatch(/independently developed/i);
      expect(klipyProvider.independenceDisclaimer).toMatch(
        /not affiliated with or endorsed by KLIPY/i
      );
    });
  });

  describe('rendition selection', () => {
    it('prefers MP4 (animatedKind=video) when available', async () => {
      getBySlugMock.mockResolvedValue(mockMp4Item);
      const r = await klipyProvider.getBySlug('mp4-only');
      expect(r.animatedUrl).toBe(
        `${API_BASE}/api/v1/klipy/media?url=` + encodeURIComponent('https://media.klipy.com/m.mp4')
      );
      expect(r.animatedKind).toBe('video');
    });

    it('falls back to GIF (animatedKind=image) when no MP4 is available', async () => {
      getBySlugMock.mockResolvedValue(mockGifItem);
      const r = await klipyProvider.getBySlug('gif-only');
      expect(r.animatedUrl).toBe(
        `${API_BASE}/api/v1/klipy/media?url=` + encodeURIComponent('https://media.klipy.com/g.gif')
      );
      expect(r.animatedKind).toBe('image');
    });

    it('falls back to WEBP when no MP4 or GIF is available', async () => {
      getBySlugMock.mockResolvedValue(mockWebpItem);
      const r = await klipyProvider.getBySlug('webp-only');
      expect(r.animatedUrl).toBe(
        `${API_BASE}/api/v1/klipy/media?url=` + encodeURIComponent('https://media.klipy.com/w.webp')
      );
      expect(r.animatedKind).toBe('image');
    });

    it('prefers MP4 over WEBP and GIF when all three are present', async () => {
      getBySlugMock.mockResolvedValue(mockMixedItem);
      const r = await klipyProvider.getBySlug('all-three');
      expect(r.animatedUrl).toBe(
        `${API_BASE}/api/v1/klipy/media?url=` +
          encodeURIComponent('https://media.klipy.com/all.mp4')
      );
      expect(r.animatedKind).toBe('video');
    });

    it('uses the explicit still URL when present', async () => {
      getBySlugMock.mockResolvedValue(mockMp4Item);
      const r = await klipyProvider.getBySlug('mp4-only');
      expect(r.stillUrl).toBe(
        `${API_BASE}/api/v1/klipy/media?url=` + encodeURIComponent('https://media.klipy.com/m.jpg')
      );
    });

    it('falls back to the animated URL as still when no still is provided', async () => {
      getBySlugMock.mockResolvedValue(mockGifItem);
      const r = await klipyProvider.getBySlug('gif-only');
      expect(r.stillUrl).toBe(
        `${API_BASE}/api/v1/klipy/media?url=` + encodeURIComponent('https://media.klipy.com/g.gif')
      );
    });

    it('captures width and height from the MP4 rendition', async () => {
      getBySlugMock.mockResolvedValue(mockMp4Item);
      const r = await klipyProvider.getBySlug('mp4-only');
      expect(r.width).toBe(320);
      expect(r.height).toBe(240);
    });

    it('falls back to GIF dimensions when MP4 dimensions are missing', async () => {
      getBySlugMock.mockResolvedValue(mockGifItem);
      const r = await klipyProvider.getBySlug('gif-only');
      expect(r.width).toBe(200);
      expect(r.height).toBe(200);
    });

    it('throws when getBySlug returns null', async () => {
      getBySlugMock.mockResolvedValue(null);
      await expect(klipyProvider.getBySlug('missing')).rejects.toThrow(/not found/i);
    });

    it('throws when the item has no usable rendition', async () => {
      getBySlugMock.mockResolvedValue({ slug: 'no-renditions' } as KlipyGifItem);
      await expect(klipyProvider.getBySlug('no-renditions')).rejects.toThrow(
        /no usable rendition/i
      );
    });
  });

  describe('list endpoints', () => {
    it('trending returns mapped items', async () => {
      trendingMock.mockResolvedValue({
        data: [mockMp4Item, mockGifItem],
        has_more: true,
      });
      const r = await klipyProvider.trending({ offset: 0, limit: 25 });
      expect(r.items).toHaveLength(2);
      expect(r.items[0].slug).toBe('mp4-only');
      expect(r.items[1].slug).toBe('gif-only');
      expect(r.hasMore).toBe(true);
    });

    it('trending computes the page number from offset/limit', async () => {
      trendingMock.mockResolvedValue({ data: [], has_more: false });
      await klipyProvider.trending({ offset: 50, limit: 25 });
      expect(trendingMock).toHaveBeenCalledWith(3, 25, undefined); // page 3 = offset 50
    });

    it('trending forwards locale when provided', async () => {
      trendingMock.mockResolvedValue({ data: [], has_more: false });
      await klipyProvider.trending({ offset: 0, limit: 25, locale: 'de' });
      expect(trendingMock).toHaveBeenCalledWith(1, 25, 'de');
    });

    it('search returns mapped items and forwards the query', async () => {
      searchMock.mockResolvedValue({ data: [mockMp4Item], has_more: false });
      const r = await klipyProvider.search({ q: 'cats', offset: 0, limit: 25 });
      expect(r.items).toHaveLength(1);
      expect(searchMock).toHaveBeenCalledWith('cats', 1, 25, undefined);
    });

    it('recent returns mapped items', async () => {
      recentMock.mockResolvedValue({ data: [mockGifItem], has_more: false });
      const r = await klipyProvider.recent({ offset: 0, limit: 25 });
      expect(r.items).toHaveLength(1);
      expect(r.items[0].slug).toBe('gif-only');
    });

    it('handles result envelope variant ({result: ...} instead of {data: ...})', async () => {
      trendingMock.mockResolvedValue({ result: [mockMp4Item], has_more: false });
      const r = await klipyProvider.trending({ offset: 0, limit: 25 });
      expect(r.items).toHaveLength(1);
    });

    it('filters out items that have no usable rendition', async () => {
      const noRendition: KlipyGifItem = { slug: 'broken' };
      trendingMock.mockResolvedValue({ data: [mockMp4Item, noRendition], has_more: false });
      const r = await klipyProvider.trending({ offset: 0, limit: 25 });
      // The broken item is silently dropped
      expect(r.items).toHaveLength(1);
      expect(r.items[0].slug).toBe('mp4-only');
    });

    it('infers hasMore from page size when not in the response', async () => {
      // Mock a full page worth of items with no has_more flag
      const fullPage = Array.from({ length: 25 }, (_, i) => ({
        ...mockMp4Item,
        slug: `gif-${i}`,
      }));
      trendingMock.mockResolvedValue({ data: fullPage });
      const r = await klipyProvider.trending({ offset: 0, limit: 25 });
      expect(r.hasMore).toBe(true);
    });
  });

  describe('categories', () => {
    it('returns categories with mapped previews', async () => {
      categoriesMock.mockResolvedValue({
        data: [
          { name: 'Reactions', preview: mockMp4Item },
          { name: 'Animals', preview: mockGifItem },
        ],
      });
      const cats = await klipyProvider.categories({});
      expect(cats).toHaveLength(2);
      expect(cats[0].name).toBe('Reactions');
      expect(cats[0].preview.animatedKind).toBe('video');
      expect(cats[1].name).toBe('Animals');
      expect(cats[1].preview.animatedKind).toBe('image');
    });

    it('drops categories whose preview has no usable rendition', async () => {
      categoriesMock.mockResolvedValue({
        data: [
          { name: 'Good', preview: mockMp4Item },
          { name: 'Bad', preview: { slug: 'no-rendition' } },
        ],
      });
      const cats = await klipyProvider.categories({});
      expect(cats).toHaveLength(1);
      expect(cats[0].name).toBe('Good');
    });

    it('drops categories with no name', async () => {
      categoriesMock.mockResolvedValue({
        data: [
          { name: '', preview: mockMp4Item },
          { preview: mockMp4Item } as { preview: KlipyGifItem },
          { name: 'Valid', preview: mockMp4Item },
        ],
      });
      const cats = await klipyProvider.categories({});
      expect(cats).toHaveLength(1);
      expect(cats[0].name).toBe('Valid');
    });
  });

  describe('personalization toggle', () => {
    it('forwards setPersonalizationEnabled to the underlying client', () => {
      klipyProvider.setPersonalizationEnabled(false);
      expect(setPersonalizationEnabledMock).toHaveBeenCalledWith(false);
      klipyProvider.setPersonalizationEnabled(true);
      expect(setPersonalizationEnabledMock).toHaveBeenCalledWith(true);
    });
  });

  describe('share trigger and report', () => {
    it('notifyShared forwards to the client', async () => {
      notifySharedMock.mockResolvedValue(undefined);
      await klipyProvider.notifyShared!('test-slug');
      expect(notifySharedMock).toHaveBeenCalledWith('test-slug');
    });

    it('report forwards to the client', async () => {
      reportMock.mockResolvedValue(undefined);
      await klipyProvider.report!('bad-slug');
      expect(reportMock).toHaveBeenCalledWith('bad-slug');
    });
  });
});
