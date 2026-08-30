import { vi, describe, it, expect, beforeEach } from 'vitest';

const resetOrder = vi.hoisted(() => [] as string[]);

// Mock apiClient to prevent real HTTP calls
vi.mock('@/renderer/services/apiClient', () => ({
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    stopWatching: vi.fn(() => resetOrder.push('preferences-sync-stop')),
    pushPreferences: vi.fn(),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    stopWatching: vi.fn(() => resetOrder.push('saved-gifs-sync-stop')),
  },
}));

vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: {
    stopWatching: vi.fn(() => resetOrder.push('friend-sync-stop')),
  },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: {
    reset: vi.fn(),
  },
}));

vi.mock('@/renderer/services/notificationPrefsService', () => ({
  stopExpirySweep: vi.fn(() => resetOrder.push('notification-sweep-stop')),
}));

// #1241: the module-scope eligibility cache lives outside every store, so the
// only thing that clears it on an account transition is resetService's explicit
// call. Spy on it so the REGISTRATION is verified here — the clear function's
// own behaviour is covered in friendEligibility.test.ts.
vi.mock('@/renderer/services/friendEligibility', () => ({
  clearFriendEligibilityCache: vi.fn(),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    fencePendingOperations: vi.fn(),
    clearKeys: vi.fn(),
    // channelStore.clearChannels() purges per-channel access state for each seeded
    // channel, which routes through e2eeService.revokeChannelAccess. Suites that
    // seed channels reach it via resetAllStores() in beforeEach.
    revokeChannelAccess: vi.fn(),
  },
}));

import { gracefulReset, nuclearReset, recoveryReset } from '@/renderer/services/resetService';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { useFriendStore } from '@/renderer/stores/chat/friendStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useRichPresenceStore } from '@/renderer/stores/ui/richPresenceStore';
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { useAudioSettingsStore } from '@/renderer/stores/audio/audioSettingsStore';
import { useFriendOrgStore } from '@/renderer/stores/chat/friendOrgStore';
import { usePresenceOverrideStore } from '@/renderer/stores/ui/presenceOverrideStore';
import { useSavedGifsStore } from '@/renderer/stores/chat/savedGifsStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import { stopExpirySweep } from '@/renderer/services/notificationPrefsService';
import { clearFriendEligibilityCache } from '@/renderer/services/friendEligibility';
import { stopProactiveRefresh } from '@/renderer/services/apiClient';
import { e2eeService } from '@/renderer/services/e2eeService';
import { clearIndex, indexMessage, isIndexed } from '@/renderer/services/searchService';
import { useDraftMessageStore } from '@/renderer/stores/chat/draftMessageStore';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
import { mockServer } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  clearIndex();
  resetOrder.length = 0;
  vi.mocked(friendOrgSyncService.stopWatching).mockClear();
  vi.mocked(preferencesSyncService.stopWatching).mockClear();
  vi.mocked(savedGifsSyncService.stopWatching).mockClear();
  vi.mocked(stopExpirySweep).mockClear();
  vi.mocked(stopProactiveRefresh).mockClear();
  vi.mocked(clearFriendEligibilityCache).mockClear();
  vi.mocked(e2eeService.fencePendingOperations).mockReset();
  vi.mocked(e2eeService.clearKeys).mockReset();
  vi.mocked(e2eeService.revokeChannelAccess).mockReset();
  vi.mocked(presenceOverrideSyncService.reset)
    .mockReset()
    .mockImplementation(() => {
      resetOrder.push('presence-sync-reset');
      usePresenceOverrideStore.getState().reset();
    });
  // Set up some state to verify it gets cleared
  useAuthStore.getState().setAccessToken('test-token');
  useServerStore.getState().addServer(mockServer);
  useUserStore.setState({
    user: { id: 'u1', username: 'test', email: 'test@test.com' },
  } as never);
});

describe('resetService', () => {
  // Only nuclearReset() clears NSFW intent. gracefulReset() must NOT: softRestart()
  // calls it on a same-account Recovery-B reload, and clearing there would revoke the
  // user's opt-in and persist it to disk on any transport blip (#2199).
  it('only nuclear reset clears NSFW intent; recovery and graceful preserve it', () => {
    useSettingsStore.getState().setAllowNsfwContent(true);
    recoveryReset();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(true);
    gracefulReset();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(true);
    nuclearReset();
    expect(useSettingsStore.getState().allowNsfwContent).toBe(false);
  });

  describe('gracefulReset', () => {
    it('cancels encrypted-social sync before clearing either decrypted store', () => {
      useFriendOrgStore.getState()._hydrate({
        v: 1,
        categories: [
          {
            id: 'cat_prior',
            name: 'Prior account',
            emoji: '',
            color: null,
            memberIds: ['prior-user'],
          },
        ],
        sectionOrder: ['cat_prior'],
      });
      usePresenceOverrideStore.getState().apply(['11111111-1111-4111-8111-111111111111'], 7);
      useSavedGifsStore.getState().saveGif('prior-account-gif');
      useNotificationPrefsStore
        .getState()
        .setMute('server', '22222222-2222-4222-8222-222222222222', true, null);

      const unsubscribeFriend = useFriendOrgStore.subscribe((state, previous) => {
        if (previous.categories.length > 0 && state.categories.length === 0) {
          resetOrder.push('friend-store-reset');
        }
      });
      const unsubscribePresence = usePresenceOverrideStore.subscribe((state, previous) => {
        if (previous.excludedUserIds.length > 0 && state.excludedUserIds.length === 0) {
          resetOrder.push('presence-store-reset');
        }
      });

      try {
        gracefulReset();
      } finally {
        unsubscribeFriend();
        unsubscribePresence();
      }

      expect(resetOrder).toEqual([
        'preferences-sync-stop',
        'saved-gifs-sync-stop',
        'friend-sync-stop',
        'notification-sweep-stop',
        'presence-sync-reset',
        'presence-store-reset',
        'friend-store-reset',
      ]);
      expect(useFriendOrgStore.getState().categories).toEqual([]);
      expect(useFriendOrgStore.getState().sectionOrder).toEqual([]);
      expect(useSavedGifsStore.getState().gifs).toEqual([]);
      expect(useNotificationPrefsStore.getState().mutedServers.size).toBe(0);
      expect(usePresenceOverrideStore.getState()).toMatchObject({
        excludedUserIds: [],
        appliedVersion: 0,
        loading: false,
        saving: false,
        conflict: false,
        error: null,
      });
      expect(useAuthStore.getState().accessToken).toBe('test-token');
    });

    it('clears content stores', () => {
      gracefulReset();

      expect(useServerStore.getState().servers).toHaveLength(0);
      expect(useChannelStore.getState().channels).toHaveLength(0);
      expect(useDMStore.getState().conversations).toHaveLength(0);
      expect(useFriendStore.getState().friends).toHaveLength(0);
      expect(useChatStore.getState().messagesByChannel.size).toBe(0);
    });

    it('clears decrypted content from the in-memory search index', () => {
      indexMessage('prior-account-message', 'prior account secret', 'channel-1');
      expect(isIndexed('prior-account-message')).toBe(true);

      gracefulReset();

      expect(isIndexed('prior-account-message')).toBe(false);
    });

    it('clears E2EE keys before clearing content or indexed plaintext', () => {
      // gracefulReset clears renderer key custody (clearKeys fences internally),
      // and must do so BEFORE wiping content stores / the plaintext search index.
      indexMessage('in-flight-message', 'prior account secret', 'channel-1');
      vi.mocked(e2eeService.clearKeys).mockImplementationOnce(() => {
        expect(useServerStore.getState().servers).toHaveLength(1);
        expect(isIndexed('in-flight-message')).toBe(true);
      });

      gracefulReset();

      expect(e2eeService.clearKeys).toHaveBeenCalledOnce();
      expect(useServerStore.getState().servers).toHaveLength(0);
      expect(isIndexed('in-flight-message')).toBe(false);
    });

    it('clears the rich-presence custom-text cache (#1233 cross-account leak fix)', () => {
      useRichPresenceStore.getState().setCustomText('other-user', { text: 'busy', emoji: '🔴' });
      useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'mine' });

      gracefulReset();

      expect(Object.keys(useRichPresenceStore.getState().customTextByUser)).toHaveLength(0);
      expect(useRichPresenceStore.getState().self.tier).toBe(0);
      expect(useRichPresenceStore.getState().self.customText).toBeUndefined();
    });

    it('clears the entitlement set to the free floor (#1297 cross-account leak fix)', () => {
      useSubscriptionStore
        .getState()
        .setEntitlement({ ...FREE_ENTITLEMENT, tier: 'premium', allowMusicMode: true });

      gracefulReset();

      expect(useSubscriptionStore.getState().entitlement).toEqual(FREE_ENTITLEMENT);
      expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
      expect(useSubscriptionStore.getState().degraded).toBe(false);
    });

    it('clears the friend-request eligibility cache (#1241 cross-account leak fix)', () => {
      // The verdict cache is module-scope, not a store, so resetAllStores() and
      // every store-level clear leave it untouched. This asserts the
      // REGISTRATION — that gracefulReset actually calls the clear — because
      // that call is the whole guard. Without it, account A's per-user verdicts
      // (and the process-wide `unsupported` latch) are served to account B on a
      // shared device.
      gracefulReset();

      expect(clearFriendEligibilityCache).toHaveBeenCalledTimes(1);
    });

    it('preserves auth tokens', () => {
      gracefulReset();
      expect(useAuthStore.getState().accessToken).toBe('test-token');
    });

    it('preserves proactive token refresh for a same-account reset', () => {
      gracefulReset();

      expect(stopProactiveRefresh).not.toHaveBeenCalled();
    });

    it('removes specific localStorage keys', () => {
      localStorage.setItem('concord:dm-store', 'data');
      localStorage.setItem('concord-servers', 'data');
      localStorage.setItem('concord-channels', 'data');
      localStorage.setItem('concord:audio-advanced', 'should-stay');

      gracefulReset();

      expect(localStorage.getItem('concord:dm-store')).toBeNull();
      expect(localStorage.getItem('concord-servers')).toBeNull();
      expect(localStorage.getItem('concord-channels')).toBeNull();
      // Settings stores should NOT be touched
      expect(localStorage.getItem('concord:audio-advanced')).toBe('should-stay');
    });
  });

  describe('nuclearReset', () => {
    it('clears freshly reseeded NSFW content intent', () => {
      useSettingsStore.getState().setAllowNsfwContent(true);

      nuclearReset();

      expect(useSettingsStore.getState().allowNsfwContent).toBe(false);
    });

    it('inherits encrypted-social cancellation before clearing auth and tokens', () => {
      useFriendOrgStore.getState()._hydrate({
        v: 1,
        categories: [
          { id: 'cat_prior', name: 'Prior', emoji: '', color: null, memberIds: ['prior-user'] },
        ],
        sectionOrder: ['cat_prior'],
      });
      usePresenceOverrideStore.getState().apply(['11111111-1111-4111-8111-111111111111'], 3);
      const clearTokens = vi.fn(() => resetOrder.push('token-clear'));
      window.electron.clearTokens = clearTokens;

      nuclearReset();

      expect(resetOrder.slice(0, 5)).toEqual([
        'preferences-sync-stop',
        'saved-gifs-sync-stop',
        'friend-sync-stop',
        'notification-sweep-stop',
        'presence-sync-reset',
      ]);
      expect(resetOrder.at(-1)).toBe('token-clear');
      expect(useFriendOrgStore.getState().categories).toEqual([]);
      expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual([]);
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

    it('clears everything including auth', () => {
      nuclearReset();

      expect(useAuthStore.getState().accessToken).toBeNull();
      expect(useServerStore.getState().servers).toHaveLength(0);
    });

    it('preserves device-settings localStorage keys (#1603)', () => {
      // Device-local preferences survive logout-class resets; the old
      // wipe-everything contract reverted theme + A/V settings on every
      // logout. Detailed per-store coverage lives in
      // resetService.deviceSettings.test.ts.
      localStorage.setItem('concord-settings', 'device-pref');
      localStorage.setItem('concord:audio-advanced', 'device-pref');
      localStorage.setItem('concord:video-settings', 'device-pref');
      localStorage.setItem('concord:tts-settings', 'device-pref');

      nuclearReset();

      // The settings and audio keys are re-serialized by their reset write-backs,
      // so assert presence, not raw-string equality.
      expect(localStorage.getItem('concord-settings')).not.toBeNull();
      expect(localStorage.getItem('concord:audio-advanced')).not.toBeNull();
      expect(localStorage.getItem('concord:video-settings')).toBe('device-pref');
      expect(localStorage.getItem('concord:tts-settings')).toBe('device-pref');
    });

    it('clears user-scoped per-participant volume overrides (#1233 discipline)', () => {
      useAudioSettingsStore.getState().setParticipantVolume('other-user-id', 40);

      nuclearReset();

      expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({});
    });

    it('inherits the friend-request eligibility clear (#1241)', () => {
      // Every login-screen transition routes through here, so the guard must
      // hold on the widest tier too — it does by delegation to gracefulReset.
      nuclearReset();

      expect(clearFriendEligibilityCache).toHaveBeenCalledTimes(1);
    });

    it('calls electron clearTokens', () => {
      const clearTokens = vi.fn();
      window.electron.clearTokens = clearTokens;

      nuclearReset();

      expect(clearTokens).toHaveBeenCalled();
    });
  });

  describe('recoveryReset (#2199)', () => {
    it('fences in-flight E2EE work', () => {
      recoveryReset();

      expect(e2eeService.fencePendingOperations).toHaveBeenCalledTimes(1);
    });

    it('never clears E2EE key material — handleReconnected gates on isInitialized', () => {
      recoveryReset();

      expect(e2eeService.clearKeys).not.toHaveBeenCalled();
    });

    it('retains client-authored state that no hydrator can restore', () => {
      useDraftMessageStore.getState().setDraft('channel-1', {
        text: 'unsent text',
        updatedAt: 1,
      });

      recoveryReset();

      expect(useDraftMessageStore.getState().getDraft('channel-1')?.text).toBe('unsent text');
    });

    it('retains the active server so the user is not bounced out of their channel', () => {
      useServerStore.getState().setActiveServer(mockServer.id);

      recoveryReset();

      expect(useServerStore.getState().servers).toHaveLength(1);
      expect(useServerStore.getState().activeServerId).toBe(mockServer.id);
    });

    it('retains content stores that gracefulReset clears', () => {
      useChannelStore.setState({ channels: [{ id: 'c1' }] as never });
      useDMStore.setState({ conversations: [{ id: 'd1' }] as never });
      useFriendStore.setState({ friends: [{ id: 'f1' }] as never });

      recoveryReset();

      expect(useChannelStore.getState().channels).toHaveLength(1);
      expect(useDMStore.getState().conversations).toHaveLength(1);
      expect(useFriendStore.getState().friends).toHaveLength(1);
    });

    it('retains the authenticated user — the account did not change', () => {
      recoveryReset();

      expect(useUserStore.getState().user).not.toBeNull();
      expect(useAuthStore.getState().accessToken).toBe('test-token');
    });

    it('leaves E2EE readiness agreeing with the service (AC #3)', () => {
      useE2EEStore.getState().setReady(true);

      recoveryReset();

      expect(useE2EEStore.getState().ready).toBe(true);
    });

    it('does not stop proactive refresh — the token is unchanged (AC #5)', () => {
      recoveryReset();

      expect(stopProactiveRefresh).not.toHaveBeenCalled();
    });

    it('keeps the eligibility cache — the account did not change (#1241)', () => {
      recoveryReset();

      expect(clearFriendEligibilityCache).not.toHaveBeenCalled();
    });

    it('does not stop account-bound watchers — hydratePostLogin re-arms them idempotently', () => {
      recoveryReset();

      expect(preferencesSyncService.stopWatching).not.toHaveBeenCalled();
      expect(savedGifsSyncService.stopWatching).not.toHaveBeenCalled();
      expect(friendOrgSyncService.stopWatching).not.toHaveBeenCalled();
      expect(stopExpirySweep).not.toHaveBeenCalled();
    });
  });

  describe('nuclearReset E2EE key custody (#2199, CWE-212)', () => {
    it('clears renderer key material — the login screen is about to appear', () => {
      nuclearReset();

      expect(e2eeService.clearKeys).toHaveBeenCalledTimes(1);
    });

    it('clears keys without depending on userStore.logout as the caller', () => {
      // apiClient's rememberMe=false 401 path calls nuclearReset() directly and
      // lands on the login screen without passing through userStore.logout, which
      // was the only clearKeys() caller before #2199.
      nuclearReset();

      expect(e2eeService.clearKeys).toHaveBeenCalled();
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

    it('gracefulReset ALSO clears keys — all its callers reach the login screen or a reload', () => {
      // Codex #2327 review: the CWE-212 fix was incomplete when clearKeys lived only
      // in nuclearReset — apiClient's rememberMe=true refresh-failure path uses
      // gracefulReset and still lands on the login screen. Post-recoveryReset-split,
      // no gracefulReset caller is a live same-session continuation, so it clears
      // renderer keys too. (recoveryReset — the same-session path — still does NOT;
      // see its suite above.)
      gracefulReset();

      expect(e2eeService.clearKeys).toHaveBeenCalledTimes(1);
    });
  });
});

// #2363. App replays a held deep link when `accessToken && emailVerified` next
// become true, and that predicate is account-BLIND — so user A's invite, held
// across a rememberMe refresh failure, would open for whoever signs in next.
// gracefulReset is the renderer's own session teardown, so it announces the end
// and App drops anything held for the session that just closed. This does NOT
// touch click-invite-then-sign-in: that flow has no prior session, so this reset
// never runs for it.
describe('gracefulReset — deep-link session teardown (#2363)', () => {
  it('announces the session end so a held invite cannot cross to the next sign-in', async () => {
    const { gracefulReset } = await import('@/renderer/services/resetService');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    const seen: string[] = [];
    const handler = (): void => {
      seen.push('deep-link-session-ended');
    };
    globalThis.addEventListener('deep-link-session-ended', handler);
    try {
      gracefulReset();
      expect(
        seen,
        'without this the held code outlives its session and opens for the next account'
      ).toEqual(['deep-link-session-ended']);
    } finally {
      globalThis.removeEventListener('deep-link-session-ended', handler);
    }
  });

  // The other half, and a P1 the first version got wrong. A cold start whose
  // restoreSession() fails calls gracefulReset, and so does the
  // ownerless-credential path — both with no access token, because nobody signed
  // in. Announcing there erases the invite the user launched the app to accept.
  it('stays silent when there was no session — the cold-start invite must survive', async () => {
    const { gracefulReset } = await import('@/renderer/services/resetService');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    useAuthStore.setState({ accessToken: null });
    const forgetDeepLinks = vi.fn();
    window.electron.forgetDeepLinks = forgetDeepLinks;
    const seen: string[] = [];
    const handler = (): void => {
      seen.push('deep-link-session-ended');
    };
    globalThis.addEventListener('deep-link-session-ended', handler);
    try {
      gracefulReset();
      expect(
        seen,
        'a login-side reset must not erase the invite the user is signing in to accept'
      ).toEqual([]);
      expect(
        forgetDeepLinks,
        'and it must not tell main to forget one either'
      ).not.toHaveBeenCalled();
    } finally {
      globalThis.removeEventListener('deep-link-session-ended', handler);
    }
  });

  // Codex P2 on PR #2967. Recovery B is a same-account RENDERER restart on a
  // still-valid session: preflight reports tokenValid === 'ok' with an unstable
  // renderer, and useConnectionRecovery calls softRestart() -> gracefulReset() ->
  // location.reload(). The token is live there, so `accessToken !== null` answers
  // "did a session exist" correctly and "is this session ending" WRONGLY.
  //
  // Before the forget, this path still worked: the event cleared App's copy, but
  // main kept its own and resetDeepLinkDelivery re-queued it for the successor
  // renderer. Forgetting removes the copy that recovery was relying on, so a
  // transport blip silently eats a pending invite. #2199 recorded this exact
  // shape for NSFW intent (see the top of this file); the deep-link fence walked
  // into it, so the intent is now explicit at the call site rather than inferred.
  it('softRestart preserves deep links — Recovery B restarts a renderer, it does not end a session', async () => {
    const { softRestart } = await import('@/renderer/services/resetService');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    const forgetDeepLinks = vi.fn();
    window.electron.forgetDeepLinks = forgetDeepLinks;
    const seen: string[] = [];
    const handler = (): void => {
      seen.push('deep-link-session-ended');
    };
    globalThis.addEventListener('deep-link-session-ended', handler);
    try {
      softRestart();
      expect(
        forgetDeepLinks,
        'a transport blip must not eat the invite the user is holding'
      ).not.toHaveBeenCalled();
      expect(seen, 'and it must not announce an ending that is not happening').toEqual([]);
    } finally {
      globalThis.removeEventListener('deep-link-session-ended', handler);
    }
  });

  // The renderer event clears App's copy; MAIN holds its own, and on a rememberMe
  // teardown nothing else tells it the session ended — clearTokens is not called
  // there by design. Without this call a swap inside the carry window replayed
  // user A's invite into user B's renderer (#2363).
  it('tells main to forget too — the renderer fence alone is not sufficient', async () => {
    const { gracefulReset } = await import('@/renderer/services/resetService');
    const { useAuthStore } = await import('@/renderer/stores/auth/authStore');
    useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
    const forgetDeepLinks = vi.fn();
    window.electron.forgetDeepLinks = forgetDeepLinks;

    gracefulReset();

    expect(
      forgetDeepLinks,
      "main keeps its own copy; clearing only the renderer's leaves it replayable"
    ).toHaveBeenCalled();
  });
});
