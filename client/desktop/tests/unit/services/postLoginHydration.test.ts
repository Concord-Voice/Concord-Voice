import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock every collaborator module before importing the unit under test. The
// repo convention (see subscriptionStore.test.ts) is a hoisted vi.mock factory
// driven via vi.mocked(...), because the unit imports these as ESM named
// bindings that vi.spyOn cannot reassign at the namespace level.
vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    init: vi.fn(),
    startWatching: vi.fn(),
    stopWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    startWatching: vi.fn(),
    stopWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: {
    startWatching: vi.fn(),
    stopWatching: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(true),
  },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: {
    fetchAndApply: vi.fn().mockResolvedValue(true),
    reset: vi.fn(),
  },
}));

vi.mock('@/renderer/services/notificationPrefsService', () => ({
  tryHydrateNotificationPrefs: vi.fn().mockResolvedValue(undefined),
  stopExpirySweep: vi.fn(),
}));

vi.mock('@/renderer/stores/subscriptionStore', () => {
  const hydrate = vi.fn().mockResolvedValue(undefined);
  const reset = vi.fn();
  return {
    useSubscriptionStore: {
      getState: vi.fn(() => ({ hydrate, reset })),
    },
  };
});

import { hydratePostLogin, buildPreferencesSyncDeps } from '@/renderer/services/postLoginHydration';
import { gracefulReset } from '@/renderer/services/resetService';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import { tryHydrateNotificationPrefs } from '@/renderer/services/notificationPrefsService';
import { useSubscriptionStore } from '@/renderer/stores/subscriptionStore';
import { useLayoutStore, type SidebarProfiles } from '@/renderer/stores/layoutStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('hydratePostLogin', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    vi.mocked(preferencesSyncService.fetchAndApply).mockResolvedValue(undefined);
    vi.mocked(savedGifsSyncService.fetchAndApply).mockResolvedValue(undefined);
    vi.mocked(friendOrgSyncService.fetchAndApply).mockResolvedValue(true);
    vi.mocked(presenceOverrideSyncService.fetchAndApply).mockResolvedValue(true);
  });

  it('runs the full hydration cluster in one call', async () => {
    await hydratePostLogin();

    expect(preferencesSyncService.init).toHaveBeenCalledTimes(1);
    expect(preferencesSyncService.startWatching).toHaveBeenCalledTimes(1);
    expect(preferencesSyncService.fetchAndApply).toHaveBeenCalledTimes(1);
    expect(savedGifsSyncService.startWatching).toHaveBeenCalledTimes(1);
    expect(savedGifsSyncService.fetchAndApply).toHaveBeenCalledTimes(1);
    expect(friendOrgSyncService.startWatching).toHaveBeenCalledTimes(1);
    expect(friendOrgSyncService.fetchAndApply).toHaveBeenCalledTimes(1);
    expect(presenceOverrideSyncService.fetchAndApply).toHaveBeenCalledTimes(1);
    expect(tryHydrateNotificationPrefs).toHaveBeenCalledTimes(1);
    // useSubscriptionStore.getState().hydrate() is the entitlement hydration.
    expect(useSubscriptionStore.getState).toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).toHaveBeenCalledTimes(1);
  });

  it('keeps the authenticated guard current until the lifecycle is reset', async () => {
    await hydratePostLogin();
    const guard = vi.mocked(preferencesSyncService.fetchAndApply).mock.calls[0]?.[0];

    expect(guard?.isCurrent()).toBe(true);

    gracefulReset();

    expect(guard?.isCurrent()).toBe(false);
  });

  it('fully hydrates friend organization before fetching presence overrides', async () => {
    let releaseFriendOrg: (() => void) | undefined;
    vi.mocked(friendOrgSyncService.fetchAndApply).mockImplementationOnce(
      () =>
        new Promise<boolean>((resolve) => {
          releaseFriendOrg = () => resolve(true);
        })
    );

    const hydration = hydratePostLogin();
    await vi.waitFor(() => expect(friendOrgSyncService.fetchAndApply).toHaveBeenCalledOnce());

    expect(friendOrgSyncService.startWatching).toHaveBeenCalledOnce();
    expect(presenceOverrideSyncService.fetchAndApply).not.toHaveBeenCalled();

    releaseFriendOrg?.();
    await hydration;

    expect(presenceOverrideSyncService.fetchAndApply).toHaveBeenCalledOnce();
  });

  it('stops a stale hydration chain before fetching presence overrides', async () => {
    vi.mocked(friendOrgSyncService.fetchAndApply).mockResolvedValueOnce(false);

    await hydratePostLogin();

    expect(presenceOverrideSyncService.fetchAndApply).not.toHaveBeenCalled();
    expect(tryHydrateNotificationPrefs).not.toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).not.toHaveBeenCalled();
  });

  it('does not resume with saved-GIF or encrypted-social hydration after reset during preferences', async () => {
    let releasePreferences: (() => void) | undefined;
    vi.mocked(preferencesSyncService.fetchAndApply).mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          releasePreferences = resolve;
        })
    );

    const hydration = hydratePostLogin();
    await vi.waitFor(() => expect(preferencesSyncService.fetchAndApply).toHaveBeenCalledOnce());

    gracefulReset();
    releasePreferences?.();
    await hydration;

    expect(savedGifsSyncService.startWatching).not.toHaveBeenCalled();
    expect(savedGifsSyncService.fetchAndApply).not.toHaveBeenCalled();
    expect(friendOrgSyncService.startWatching).not.toHaveBeenCalled();
    expect(friendOrgSyncService.fetchAndApply).not.toHaveBeenCalled();
    expect(presenceOverrideSyncService.fetchAndApply).not.toHaveBeenCalled();
    expect(tryHydrateNotificationPrefs).not.toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).not.toHaveBeenCalled();
  });

  it('does not start a fresh friend-org generation after reset during saved-GIF hydration', async () => {
    let releaseSavedGifs: (() => void) | undefined;
    vi.mocked(savedGifsSyncService.fetchAndApply).mockImplementationOnce(
      () =>
        new Promise<void>((resolve) => {
          releaseSavedGifs = resolve;
        })
    );

    const hydration = hydratePostLogin();
    await vi.waitFor(() => expect(savedGifsSyncService.fetchAndApply).toHaveBeenCalledOnce());

    gracefulReset();
    releaseSavedGifs?.();
    await hydration;

    expect(friendOrgSyncService.startWatching).not.toHaveBeenCalled();
    expect(friendOrgSyncService.fetchAndApply).not.toHaveBeenCalled();
    expect(presenceOverrideSyncService.fetchAndApply).not.toHaveBeenCalled();
    expect(tryHydrateNotificationPrefs).not.toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).not.toHaveBeenCalled();
  });

  it('stops a hydration chain when presence override hydration is reset in flight', async () => {
    let resolvePresenceOverrides: ((isCurrent: boolean) => void) | undefined;
    vi.mocked(presenceOverrideSyncService.fetchAndApply).mockImplementationOnce(
      () =>
        new Promise<boolean>((resolve) => {
          resolvePresenceOverrides = resolve;
        })
    );

    const hydration = hydratePostLogin();
    await vi.waitFor(() =>
      expect(presenceOverrideSyncService.fetchAndApply).toHaveBeenCalledOnce()
    );

    expect(tryHydrateNotificationPrefs).not.toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).not.toHaveBeenCalled();

    resolvePresenceOverrides?.(false);
    await hydration;

    expect(tryHydrateNotificationPrefs).not.toHaveBeenCalled();
    expect(useSubscriptionStore.getState().hydrate).not.toHaveBeenCalled();
  });

  it('initializes preferencesSync with a dependency bag', async () => {
    await hydratePostLogin();
    // init() is called with the deps built by buildPreferencesSyncDeps — a
    // bag exposing the five injection points.
    const deps = vi.mocked(preferencesSyncService.init).mock.calls[0]?.[0];
    expect(deps).toMatchObject({
      getAppearance: expect.any(Function),
      setAppearance: expect.any(Function),
      getLayout: expect.any(Function),
      setLayout: expect.any(Function),
      applySidebarPreferences: expect.any(Function),
    });
  });
});

describe('buildPreferencesSyncDeps', () => {
  beforeEach(() => {
    resetAllStores();
    useLayoutStore.setState({ interfaceLocked: false, sidebarLayoutsDecoupled: false });
  });

  it('returns the five-function dependency bag', () => {
    const deps = buildPreferencesSyncDeps();
    expect(deps).toMatchObject({
      getAppearance: expect.any(Function),
      setAppearance: expect.any(Function),
      getLayout: expect.any(Function),
      setLayout: expect.any(Function),
      applySidebarPreferences: expect.any(Function),
    });
  });

  it('returns both retained profiles and the decouple flag from getLayout', () => {
    const profiles: SidebarProfiles = {
      dm: {
        left: { width: 280, pinned: true },
        right: { width: 175, pinned: false },
      },
      server: {
        left: { width: 320, pinned: false },
        right: { width: 300, pinned: true },
      },
    };
    useLayoutStore.setState({
      sidebarProfiles: profiles,
      sidebarLayoutsDecoupled: true,
    });

    const layout = buildPreferencesSyncDeps().getLayout();

    expect(layout.sidebarProfiles).toEqual(profiles);
    expect(layout.sidebarLayoutsDecoupled).toBe(true);
    expect(layout).not.toHaveProperty('channelPanelPinned');
    expect(layout).not.toHaveProperty('channelPanelWidth');
    expect(layout).not.toHaveProperty('memberPanelMode');
    expect(layout).not.toHaveProperty('memberPanelWidth');
    expect(layout).not.toHaveProperty('friendsPanelMode');
  });

  it('applies remote sidebar state through the Interface Lock guarded action', () => {
    const retained = useLayoutStore.getState().sidebarProfiles;
    const remote: SidebarProfiles = {
      dm: {
        left: { width: 300, pinned: false },
        right: { width: 280, pinned: false },
      },
      server: {
        left: { width: 310, pinned: false },
        right: { width: 290, pinned: false },
      },
    };
    useLayoutStore.setState({ interfaceLocked: true });

    buildPreferencesSyncDeps().applySidebarPreferences(remote, true);

    expect(useLayoutStore.getState().sidebarProfiles).toEqual(retained);
    expect(useLayoutStore.getState().sidebarLayoutsDecoupled).toBe(false);
  });
});
