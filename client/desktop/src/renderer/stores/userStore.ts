import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { useAuthStore } from './authStore';
import { apiFetch } from '../services/apiClient';
import { errorMessage } from '../utils/redactError';
import { getWebSocketService } from '../services/websocketService';
import { e2eeService } from '../services/e2eeService';
import { preferencesSyncService } from '../services/preferencesSync';
import { savedGifsSyncService } from '../services/savedGifsSync';
import { stopExpirySweep } from '../services/notificationPrefsService';
import { useNotificationPrefsStore } from './notificationPrefsStore';
import { setSyncSuppressed } from './colorSyncSuppression';
import {
  deriveKeyFromPassword,
  deriveKeyArgon2id,
  unwrapPrivateKey,
  wrapPrivateKey,
  generateSalt,
  base64ToArrayBuffer,
  arrayBufferToBase64,
  type KeyDerivationAlgorithm,
} from '../utils/crypto';

export interface UserProfile {
  id: string;
  username: string;
  email?: string;
  email_verified?: boolean;
  display_name?: string;
  bio?: string;
  avatar_url?: string;
  header_image_url?: string;
  links?: string[];
  created_at?: string;
  username_changed_at?: string;
  username_change_eligible_at?: string;
}

export interface UpdateProfileData {
  username?: string;
  display_name?: string | null;
  bio?: string | null;
  avatar_url?: string | null;
  header_image_url?: string | null;
  color_scheme?: string | null;
  links?: string[];
}

interface UserState {
  user: UserProfile | null;
  isLoading: boolean;
  error: string | null;

  fetchUser: () => Promise<void>;
  setUser: (user: UserProfile) => void;
  clearUser: () => void;
  logout: () => Promise<void>;
  updateProfile: (updates: UpdateProfileData) => Promise<void>;
  changePassword: (
    currentPassword: string,
    newPassword: string
  ) => Promise<{ success: boolean; error?: string }>;
}

export const useUserStore = wrapStore(
  create<UserState>()(
    devtools(
      (set) => ({
        user: null,
        isLoading: false,
        error: null,

        fetchUser: async () => {
          set({ isLoading: true, error: null });

          try {
            const response = await apiFetch('/api/v1/users/me');

            if (response.status === 401) {
              // Token expired and refresh failed — clear auth
              useAuthStore.getState().clearAccessToken();
              globalThis.electron?.clearTokens?.();
              set({ user: null, isLoading: false, error: null });
              return;
            }

            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to fetch user');
            }

            const data = await response.json();
            set({ user: data.user, isLoading: false });

            // Sync email_verified to authStore so route guards reflect DB state
            if (data.user && typeof data.user.email_verified === 'boolean') {
              useAuthStore.getState().setEmailVerified(data.user.email_verified);
            }
          } catch (error) {
            set({
              error: error instanceof Error ? error.message : 'Failed to fetch user',
              isLoading: false,
            });
          }
        },

        setUser: (user: UserProfile) => {
          set({ user, isLoading: false, error: null });
        },

        clearUser: () => {
          set({ user: null, isLoading: false, error: null });
        },

        logout: async () => {
          // Stop syncing preferences and saved GIFs before tearing down
          preferencesSyncService.stopWatching();
          savedGifsSyncService.stopWatching();
          // Stop the mute-prefs expiry sweep timer and clear any in-memory
          // prefs so the next user's session doesn't inherit the previous
          // user's mute list.
          stopExpirySweep();
          useNotificationPrefsStore.getState().clearAll();
          // Reset sync suppression flag in case Settings was open during logout.
          // STATIC import of the leaf colorSyncSuppression module — do NOT revert
          // to a dynamic `import('./settingsStore')`: that unawaited import raced
          // vitest worker teardown (EnvironmentTeardownError loading overlayColors
          // via settingsStore's static import). See colorSyncSuppression.ts.
          setSyncSuppressed(false);
          e2eeService.clearKeys();

          // Disconnect WebSocket FIRST so the server receives the close frame
          // and broadcasts offline presence while the connection is still healthy.
          getWebSocketService().disconnect();

          try {
            // Main process handles logout API call (it holds the refresh token)
            if (globalThis.electron?.logout) {
              await globalThis.electron.logout({
                accessToken: useAuthStore.getState().accessToken ?? undefined,
              });
            }
          } catch (error) {
            console.error('Logout API error:', errorMessage(error));
          }

          // Nuclear reset: login screen will appear, so wipe everything
          const { nuclearReset } = await import('../services/resetService');
          nuclearReset();
        },

        updateProfile: async (updates: UpdateProfileData) => {
          const response = await apiFetch('/api/v1/users/me', {
            method: 'PATCH',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(updates),
          });

          if (response.status === 401) {
            useAuthStore.getState().clearAccessToken();
            globalThis.electron?.clearTokens?.();
            set({ user: null, isLoading: false, error: null });
            throw new Error('Session expired');
          }

          const text = await response.text();
          let data;
          try {
            data = JSON.parse(text);
          } catch {
            throw new Error(
              'Server returned an unexpected response. Make sure the backend is running the latest version.'
            );
          }

          if (!response.ok) {
            throw new Error(data.error || 'Failed to update profile');
          }

          set({ user: data.user });

          // Notify other clients via WebSocket so their cached user info updates
          getWebSocketService().sendProfileUpdate();
        },

        changePassword: async (currentPassword: string, newPassword: string) => {
          try {
            // Step 1: Fetch current E2EE keys from the server
            const keysRes = await apiFetch('/api/v1/users/me/keys');

            if (!keysRes.ok) {
              const keysData = await keysRes.json();
              return { success: false, error: keysData.error || 'Failed to fetch encryption keys' };
            }

            const keysData = await keysRes.json();
            const { wrapped_private_key, key_derivation_salt } = keysData.e2ee_keys;
            const currentAlg: KeyDerivationAlgorithm =
              keysData.e2ee_keys.key_derivation_alg || 'pbkdf2';

            // Step 2: Unwrap private key with current password (using stored algorithm)
            const currentSalt = new Uint8Array(base64ToArrayBuffer(key_derivation_salt));
            const currentWrappingKey =
              currentAlg === 'argon2id'
                ? await deriveKeyArgon2id(currentPassword, currentSalt)
                : await deriveKeyFromPassword(currentPassword, currentSalt);
            const wrappedKeyBuffer = base64ToArrayBuffer(wrapped_private_key);
            const privateKey = await unwrapPrivateKey(wrappedKeyBuffer, currentWrappingKey);

            // Step 3: Re-wrap private key with new password (always Argon2id)
            const newSalt = generateSalt();
            const newWrappingKey = await deriveKeyArgon2id(newPassword, newSalt);
            const newWrappedKey = await wrapPrivateKey(privateKey, newWrappingKey);

            // Step 4: Send password change with re-wrapped keys
            const response = await apiFetch('/api/v1/users/me/password', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                current_password: currentPassword,
                new_password: newPassword,
                wrapped_private_key: arrayBufferToBase64(newWrappedKey),
                key_derivation_salt: arrayBufferToBase64(newSalt.buffer as ArrayBuffer),
                key_derivation_alg: 'argon2id',
              }),
            });

            if (response.status === 401) {
              const data = await response.json();
              if (data.error === 'Current password is incorrect') {
                return { success: false, error: data.error };
              }
              useAuthStore.getState().clearAccessToken();
              globalThis.electron?.clearTokens?.();
              set({ user: null, isLoading: false, error: null });
              return { success: false, error: 'Session expired' };
            }

            const data = await response.json();

            if (!response.ok) {
              return { success: false, error: data.error || 'Failed to change password' };
            }

            // Re-initialize E2EE service with new password (always Argon2id after change)
            await e2eeService.initialize(
              newPassword,
              arrayBufferToBase64(newWrappedKey),
              arrayBufferToBase64(newSalt.buffer as ArrayBuffer),
              'argon2id'
            );

            // Re-encrypt and push preferences with the new key
            await preferencesSyncService.pushPreferences();

            return { success: true };
          } catch (error) {
            return {
              success: false,
              error: error instanceof Error ? error.message : 'Failed to change password',
            };
          }
        },
      }),
      { name: 'UserStore' }
    )
  )
);
