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
}

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
          if (updates.messagesFriendsOnly !== undefined)
            body.messages_friends_only = updates.messagesFriendsOnly;
          if (updates.messagesServerMembers !== undefined)
            body.messages_server_members = updates.messagesServerMembers;
          if (updates.dmPrivacyLevel !== undefined) body.dm_privacy_level = updates.dmPrivacyLevel;
          if (updates.dmFriendsOfFriends !== undefined)
            body.dm_friends_of_friends = updates.dmFriendsOfFriends;
          if (updates.autoAcceptFriendCodes !== undefined)
            body.auto_accept_friend_codes = updates.autoAcceptFriendCodes;
          if (updates.searchableByUsername !== undefined)
            body.searchable_by_username = updates.searchableByUsername;
          if (updates.searchableByEmail !== undefined)
            body.searchable_by_email = updates.searchableByEmail;
          if (updates.searchableByPhone !== undefined)
            body.searchable_by_phone = updates.searchableByPhone;
          if (updates.allowEmbeddedContent !== undefined)
            body.allow_embedded_content = updates.allowEmbeddedContent;
          if (updates.loadGifsAutomatically !== undefined)
            body.load_gifs_automatically = updates.loadGifsAutomatically;
          if (updates.sharePersonalizationWithGifProvider !== undefined)
            body.share_personalization_with_gif_provider =
              updates.sharePersonalizationWithGifProvider;

          const response = await apiFetch('/api/v1/users/me/privacy', {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
          });

          if (!response.ok) {
            const data = await response.json();
            throw new Error(data.error || 'Failed to update privacy settings');
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
            },
          });
        },

        clearPrivacy: () => set({ settings: { ...defaultSettings }, error: null }),
      }),
      { name: 'PrivacyStore' }
    )
  )
);
