// Mock the underlying provider so we can verify the wiring without doing
// any actual KLIPY work.
const setPersonalizationEnabledMock = vi.fn();

vi.mock('@/renderer/services/messaging/gifProvider/klipyProvider', () => ({
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
    vi.resetModules();
    setPersonalizationEnabledMock.mockReset();
  });

  it('exports the active provider', async () => {
    const { gifProvider } = await import('@/renderer/services/messaging/gifProvider');
    expect(gifProvider).toBeDefined();
    expect(gifProvider.name).toBe('KLIPY');
  });

  it('does not enable personalization from the unconfirmed ON placeholder', async () => {
    const { usePrivacyStore } = await import('@/renderer/stores/ui/privacyStore');
    expect(usePrivacyStore.getState().loaded).toBe(false);
    expect(usePrivacyStore.getState().settings.sharePersonalizationWithGifProvider).toBe(true);

    await import('@/renderer/services/messaging/gifProvider');
    expect(setPersonalizationEnabledMock).toHaveBeenLastCalledWith(false);
  });

  it('applies confirmed privacy settings to the provider on import', async () => {
    const { usePrivacyStore } = await import('@/renderer/stores/ui/privacyStore');
    usePrivacyStore.setState({
      loaded: true,
      settings: {
        ...usePrivacyStore.getState().settings,
        sharePersonalizationWithGifProvider: true,
      },
    });

    await import('@/renderer/services/messaging/gifProvider');
    expect(setPersonalizationEnabledMock).toHaveBeenLastCalledWith(true);
  });

  it('forwards subsequent privacy store updates to the provider', async () => {
    const { usePrivacyStore } = await import('@/renderer/stores/ui/privacyStore');
    await import('@/renderer/services/messaging/gifProvider');
    setPersonalizationEnabledMock.mockClear();

    usePrivacyStore.setState({ loaded: true });
    expect(setPersonalizationEnabledMock).toHaveBeenLastCalledWith(true);

    usePrivacyStore.setState({
      settings: {
        ...usePrivacyStore.getState().settings,
        sharePersonalizationWithGifProvider: false,
      },
    });
    expect(setPersonalizationEnabledMock).toHaveBeenLastCalledWith(false);

    usePrivacyStore.setState({ loaded: false });
    expect(setPersonalizationEnabledMock).toHaveBeenLastCalledWith(false);
  });
});
