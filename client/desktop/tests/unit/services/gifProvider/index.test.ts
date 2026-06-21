import { resetAllStores } from '../../../helpers/store-helpers';
import { usePrivacyStore } from '@/renderer/stores/privacyStore';

// Mock the underlying provider so we can verify the wiring without doing
// any actual KLIPY work.
const setPersonalizationEnabledMock = vi.fn();

vi.mock('@/renderer/services/gifProvider/klipyProvider', () => ({
  klipyProvider: {
    name: 'KLIPY',
    searchPlaceholder: 'Search KLIPY',
    poweredByText: 'Powered by KLIPY',
    supportsRecent: true,
    supportsCategories: true,
    setPersonalizationEnabled: (...args: unknown[]) => setPersonalizationEnabledMock(...args),
    trending: vi.fn(),
    search: vi.fn(),
    recent: vi.fn(),
    categories: vi.fn(),
    getBySlug: vi.fn(),
  },
}));

describe('gifProvider index', () => {
  beforeEach(() => {
    resetAllStores();
    setPersonalizationEnabledMock.mockReset();
  });

  it('exports the active provider', async () => {
    const { gifProvider } = await import('@/renderer/services/gifProvider');
    expect(gifProvider).toBeDefined();
    expect(gifProvider.name).toBe('KLIPY');
  });

  it('applies current privacy settings to the provider on import', async () => {
    // Set non-default privacy values BEFORE the import to verify the
    // applySettings call inside index.ts picks them up
    usePrivacyStore.setState({
      settings: {
        ...usePrivacyStore.getState().settings,
        sharePersonalizationWithGifProvider: false,
      },
    });
    // Re-import to trigger the module-level subscribe
    vi.resetModules();
    await import('@/renderer/services/gifProvider');
    expect(setPersonalizationEnabledMock).toHaveBeenCalledWith(false);
  });

  it('forwards subsequent privacy store updates to the provider', async () => {
    vi.resetModules();
    await import('@/renderer/services/gifProvider');
    setPersonalizationEnabledMock.mockClear();
    // Trigger a store update
    usePrivacyStore.setState({
      settings: {
        ...usePrivacyStore.getState().settings,
        sharePersonalizationWithGifProvider: false,
      },
    });
    expect(setPersonalizationEnabledMock).toHaveBeenCalledWith(false);
  });
});
