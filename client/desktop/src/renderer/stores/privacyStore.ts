import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { apiFetch } from '../services/apiClient';

// DM Privacy Levels:
// 0 = Off (no DMs at all)
// 1 = Friends Only
// 2 = Friends + Server Members (default)
// 3 = Allow All
export type DMPrivacyLevel = 0 | 1 | 2 | 3;

export interface PrivacySettings {
  messagesFriendsOnly: boolean;
  messagesServerMembers: boolean;
  dmPrivacyLevel: DMPrivacyLevel;
  dmFriendsOfFriends: boolean;
  autoAcceptFriendCodes: boolean;
  searchableByUsername: boolean;
  searchableByEmail: boolean;
  searchableByPhone: boolean;
  allowEmbeddedContent: boolean;
  // KLIPY GIF integration settings (decoupled from allowEmbeddedContent in v2)
  loadGifsAutomatically: boolean;
  sharePersonalizationWithGifProvider: boolean;
  // #1354: step-up authentication before a DM/group purge.
  requireAuthBeforePurge: boolean;
}

/**
 * Client field → wire field, for the PATCH body.
 *
 * Typed `Record<keyof PrivacySettings, string>` so the mapping is exhaustive: a
 * new PrivacySettings key that is not listed here is a compile error, where the
 * previous per-field `if` chain would simply have dropped it from every update.
 */
const PRIVACY_WIRE_FIELDS: Record<keyof PrivacySettings, string> = {
  messagesFriendsOnly: 'messages_friends_only',
  messagesServerMembers: 'messages_server_members',
  dmPrivacyLevel: 'dm_privacy_level',
  dmFriendsOfFriends: 'dm_friends_of_friends',
  autoAcceptFriendCodes: 'auto_accept_friend_codes',
  searchableByUsername: 'searchable_by_username',
  searchableByEmail: 'searchable_by_email',
  searchableByPhone: 'searchable_by_phone',
  allowEmbeddedContent: 'allow_embedded_content',
  loadGifsAutomatically: 'load_gifs_automatically',
  sharePersonalizationWithGifProvider: 'share_personalization_with_gif_provider',
  requireAuthBeforePurge: 'require_auth_before_purge',
};

/**
 * #1354: a PATCH that carries only `require_auth_before_purge` is rejected with
 * this exact 400 by a control-plane that predates the field. Surfaced verbatim
 * from the copy deck — the user flipped a switch and must learn it did not take.
 */
export const PURGE_AUTH_SKEW_MESSAGE =
  "This version of the server doesn't support this setting yet.";

const defaultSettings: PrivacySettings = {
  messagesFriendsOnly: true,
  messagesServerMembers: true,
  dmPrivacyLevel: 2,
  dmFriendsOfFriends: false,
  autoAcceptFriendCodes: false,
  searchableByUsername: false,
  searchableByEmail: false,
  searchableByPhone: false,
  allowEmbeddedContent: false,
  // #1766: default ON for new users (transient pre-fetch placeholder; the
  // authoritative value is the server's, applied by fetchPrivacy()).
  loadGifsAutomatically: true,
  // Transient pre-fetch placeholder; aligned to the authoritative default
  // (migration 000056 column default + backend no-row fallback are both TRUE —
  // personalization-on degrades nothing when KLIPY traffic is always proxied).
  // Overwritten by fetchPrivacy() with the server value.
  sharePersonalizationWithGifProvider: true,
  // #1354: fail-closed. Mirrors the column default (migration 000090), the
  // backend's no-row fallback, and internal/dm/purge.go's own read — an OFF
  // placeholder would show the toggle disabled while DM purges kept demanding
  // credentials.
  requireAuthBeforePurge: true,
};

interface PrivacyState {
  settings: PrivacySettings;
  isLoading: boolean;
  error: string | null;

  fetchPrivacy: () => Promise<void>;
  updatePrivacy: (updates: Partial<PrivacySettings>) => Promise<void>;
  clearPrivacy: () => void;
}

export const usePrivacyStore = wrapStore(
  create<PrivacyState>()(
    devtools(
      (set) => ({
        settings: { ...defaultSettings },
        isLoading: false,
        error: null,

        fetchPrivacy: async () => {
          set({ isLoading: true, error: null });
          try {
            const response = await apiFetch('/api/v1/users/me/privacy');
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to load privacy settings');
            }
            const data = await response.json();
            const p = data.privacy;
            set({
              settings: {
                messagesFriendsOnly: p.messages_friends_only ?? true,
                messagesServerMembers: p.messages_server_members ?? true,
                dmPrivacyLevel: (p.dm_privacy_level ?? 2) as DMPrivacyLevel,
                dmFriendsOfFriends: p.dm_friends_of_friends ?? false,
                autoAcceptFriendCodes: p.auto_accept_friend_codes ?? false,
                searchableByUsername: p.searchable_by_username ?? false,
                searchableByEmail: p.searchable_by_email ?? false,
                searchableByPhone: p.searchable_by_phone ?? false,
                allowEmbeddedContent: p.allow_embedded_content ?? false,
                loadGifsAutomatically: p.load_gifs_automatically ?? false,
                sharePersonalizationWithGifProvider:
                  p.share_personalization_with_gif_provider ?? true,
                // #1354: a missing key means an old control-plane. Fail closed —
                // the server still requires step-up for DM purges.
                requireAuthBeforePurge: p.require_auth_before_purge ?? true,
              },
              isLoading: false,
            });
          } catch (error) {
            set({
              error: error instanceof Error ? error.message : 'Failed to load privacy settings',
              isLoading: false,
            });
          }
        },

        updatePrivacy: async (updates: Partial<PrivacySettings>) => {
          const body: Record<string, boolean | number> = {};
          for (const [field, wireField] of Object.entries(PRIVACY_WIRE_FIELDS)) {
            const value = updates[field as keyof PrivacySettings];
            if (value !== undefined) body[wireField] = value;
          }

          const response = await apiFetch('/api/v1/users/me/privacy', {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
          });

          if (!response.ok) {
            // A non-JSON failure (proxy HTML 502, empty 400) must not reject
            // with a SyntaxError — the caller renders this message verbatim.
            //
            // Annotate the variable rather than asserting the fallback: `json()`
            // resolves to `any`, so an `as` on the catch value asserts into a
            // position that already accepts it (SonarQube S4325) while leaving
            // `data` untyped either way. The annotation types BOTH branches.
            const data: { error?: string } = await response.json().catch(() => ({}));
            // #1354: an old control-plane rejects a toggle-only body as empty.
            // Translate that one shape so the failure is legible, and record it
            // on the store so the caller cannot swallow it silently.
            const isPurgeAuthSkew =
              response.status === 400 &&
              data.error === 'No fields to update' &&
              updates.requireAuthBeforePurge !== undefined;
            const message = isPurgeAuthSkew
              ? PURGE_AUTH_SKEW_MESSAGE
              : data.error || 'Failed to update privacy settings';
            set({ error: message });
            throw new Error(message);
          }

          const data = await response.json();
          const p = data.privacy;
          set({
            settings: {
              messagesFriendsOnly: p.messages_friends_only,
              messagesServerMembers: p.messages_server_members,
              dmPrivacyLevel: (p.dm_privacy_level ?? 2) as DMPrivacyLevel,
              dmFriendsOfFriends: p.dm_friends_of_friends ?? false,
              autoAcceptFriendCodes: p.auto_accept_friend_codes,
              searchableByUsername: p.searchable_by_username,
              searchableByEmail: p.searchable_by_email,
              searchableByPhone: p.searchable_by_phone,
              allowEmbeddedContent: p.allow_embedded_content ?? false,
              loadGifsAutomatically: p.load_gifs_automatically ?? false,
              sharePersonalizationWithGifProvider:
                p.share_personalization_with_gif_provider ?? true,
              // #1354: same fail-closed read as fetchPrivacy — an echo without
              // the key is an old server, not an OFF setting.
              requireAuthBeforePurge: p.require_auth_before_purge ?? true,
            },
            error: null,
          });
        },

        clearPrivacy: () => set({ settings: { ...defaultSettings }, error: null }),
      }),
      { name: 'PrivacyStore' }
    )
  )
);
