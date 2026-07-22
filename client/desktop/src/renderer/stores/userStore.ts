import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { useAuthStore } from './authStore';
import { apiFetch } from '../services/apiClient';
import { errorMessage } from '../utils/redactError';
import { getWebSocketService } from '../services/websocketService';
import { e2eeService } from '../services/e2eeService';
import { E2EEInitTeardownError } from '../services/e2eeErrors';
import { preferencesSyncService } from '../services/preferencesSync';
import { savedGifsSyncService } from '../services/savedGifsSync';
import { friendOrgSyncService } from '../services/friendOrgSync';
import { presenceOverrideSyncService } from '../services/presenceOverrideSync';
import { stopExpirySweep } from '../services/notificationPrefsService';
import { useNotificationPrefsStore } from './notificationPrefsStore';
import { usePresenceOverrideStore } from './presenceOverrideStore';
import { fetchBlobRowForRotation } from '../services/e2eeBlobTransport';
import { setSyncSuppressed } from './colorSyncSuppression';
import {
  deriveKeyFromPassword,
  deriveKeyArgon2id,
  derivePreferencesKeyArgon2id,
  encryptBlob,
  unwrapPrivateKey,
  wrapPrivateKey,
  generateSalt,
  base64ToArrayBuffer,
  arrayBufferToBase64,
  type KeyDerivationAlgorithm,
} from '../utils/crypto';
import { parsePresenceOverrides } from '../utils/presenceOverrides';
import {
  isHydrationLifecycleCurrent,
  resetPostLoginHydrationLifecycle,
  type HydrationLifecycleGuard,
} from '../services/postLoginHydrationLifecycle';

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

  fetchUser: (guard?: HydrationLifecycleGuard) => Promise<void>;
  setUser: (user: UserProfile) => void;
  clearUser: () => void;
  logout: () => Promise<void>;
  updateProfile: (updates: UpdateProfileData) => Promise<void>;
  changePassword: (currentPassword: string, newPassword: string) => Promise<PasswordChangeResult>;
}

interface PasswordChangeResult {
  success: boolean;
  error?: string;
}

interface PasswordChangeLifecycle {
  controller: AbortController;
  userId: string;
  unsubscribeAuth: () => void;
}

interface UserFetchLifecycle {
  controller: AbortController;
  guard?: HydrationLifecycleGuard;
  unsubscribeAuth: () => void;
  detachGuardAbort: () => void;
}

const PASSWORD_CHANGE_CANCELLED = 'Password change was cancelled'; // pragma: allowlist secret
// Distinct from PASSWORD_CHANGE_CANCELLED: the server-side change ALREADY committed, so
// the old credential is gone and the operation is NOT retryable — the user must
// re-authenticate with the new password. Surfaced via the auth store's loginNotice
// channel (which survives nuclearReset — clearAccessToken preserves it) so the login
// screen can explain the sign-out (#2333 salvage).
const REAUTH_REQUIRED_NOTICE =
  'Your password was changed, but this session could not be re-secured. Please sign in again with your new password.';
let activePasswordChange: PasswordChangeLifecycle | null = null;
let activeUserFetch: UserFetchLifecycle | null = null;

function cancelActiveUserFetch(): void {
  const lifecycle = activeUserFetch;
  if (lifecycle === null) return;
  activeUserFetch = null;
  lifecycle.unsubscribeAuth();
  lifecycle.detachGuardAbort();
  lifecycle.controller.abort();
}

function beginUserFetch(guard?: HydrationLifecycleGuard): UserFetchLifecycle {
  cancelActiveUserFetch();
  const { accessToken, sessionId } = useAuthStore.getState();
  const controller = new AbortController();
  const lifecycle: UserFetchLifecycle = {
    controller,
    guard,
    unsubscribeAuth: () => undefined,
    detachGuardAbort: () => undefined,
  };
  activeUserFetch = lifecycle;
  lifecycle.unsubscribeAuth = useAuthStore.subscribe((auth) => {
    if (activeUserFetch !== lifecycle) return;
    const sessionChanged =
      sessionId === null ? auth.accessToken !== accessToken : auth.sessionId !== sessionId;
    if (auth.accessToken === null || sessionChanged) cancelActiveUserFetch();
  });
  if (guard !== undefined) {
    const cancelForGuard = (): void => {
      if (activeUserFetch === lifecycle) cancelActiveUserFetch();
    };
    guard.signal.addEventListener('abort', cancelForGuard, { once: true });
    lifecycle.detachGuardAbort = () => guard.signal.removeEventListener('abort', cancelForGuard);
  }
  return lifecycle;
}

function isUserFetchCurrent(lifecycle: UserFetchLifecycle): boolean {
  return (
    activeUserFetch === lifecycle &&
    !lifecycle.controller.signal.aborted &&
    isHydrationLifecycleCurrent(lifecycle.guard)
  );
}

function finishUserFetch(lifecycle: UserFetchLifecycle): void {
  if (activeUserFetch !== lifecycle) return;
  activeUserFetch = null;
  lifecycle.unsubscribeAuth();
  lifecycle.detachGuardAbort();
}

function cancelActivePasswordChange(): void {
  const lifecycle = activePasswordChange;
  if (lifecycle === null) return;
  activePasswordChange = null;
  lifecycle.unsubscribeAuth();
  lifecycle.controller.abort();
}

function cancelPasswordChangeForDifferentAccount(nextUserId: string): void {
  if (activePasswordChange?.userId !== nextUserId) cancelActivePasswordChange();
}

function beginPasswordChange(userId: string): PasswordChangeLifecycle {
  cancelActivePasswordChange();
  const { accessToken, sessionId } = useAuthStore.getState();
  const lifecycle: PasswordChangeLifecycle = {
    controller: new AbortController(),
    userId,
    unsubscribeAuth: () => undefined,
  };
  activePasswordChange = lifecycle;
  lifecycle.unsubscribeAuth = useAuthStore.subscribe((auth) => {
    if (activePasswordChange !== lifecycle) return;
    const sessionChanged =
      sessionId === null ? auth.accessToken !== accessToken : auth.sessionId !== sessionId;
    if (auth.accessToken === null || sessionChanged) {
      cancelActivePasswordChange();
    }
  });
  return lifecycle;
}

function finishPasswordChange(lifecycle: PasswordChangeLifecycle): void {
  if (activePasswordChange !== lifecycle) return;
  activePasswordChange = null;
  lifecycle.unsubscribeAuth();
}

function assertPasswordChangeCurrent(isCurrent: () => boolean): void {
  if (!isCurrent()) throw new Error(PASSWORD_CHANGE_CANCELLED);
}

function passwordChangeFailure(error: unknown, isCurrent: () => boolean): PasswordChangeResult {
  if (!isCurrent()) return { success: false, error: PASSWORD_CHANGE_CANCELLED };
  return {
    success: false,
    error: error instanceof Error ? error.message : 'Failed to change password',
  };
}

/**
 * Re-init E2EE after a committed password change, failing closed if the new keyset was torn
 * down mid-derivation. Returns `null` on success (the caller continues) or a
 * `PasswordChangeResult` when it failed closed. Extracted (with its own `try/catch`) so
 * `changePassword` stays under the S3776 cognitive-complexity ceiling.
 *
 * A teardown during the derivation surfaces as `E2EEInitTeardownError` (#2337). Because the
 * password POST already committed (credential rotated, refresh tokens revoked), this is NOT
 * a retryable cancellation — the old password is gone. The re-auth message goes in the auth
 * store's `loginNotice` channel (which `clearAccessToken` — and therefore `nuclearReset` —
 * preserves, and which the login screen renders once) so it survives the teardown that
 * redirects the user to the login screen (the returned error can't render — the settings
 * view unmounts on the auth transition).
 *
 * `nuclearReset` is passed in pre-resolved (loaded before the POST) so this path has NO
 * await, letting the lifecycle be inspected synchronously and precisely (coderabbit):
 * - still the current lifecycle → force the teardown (backstop for the rare non-token-
 *   clearing throw) AND stage the notice;
 * - superseded, but the session is already unauthenticated (a self-teardown already cleared
 *   the token and is redirecting to login) → stage the notice only;
 * - superseded by a DIFFERENT authenticated session (a newer login became current) →
 *   neither: do not clobber the new session's state or notice.
 * Any non-teardown error propagates unchanged.
 */
async function reinitE2EEOrFailClosed(
  newPassword: string,
  wrappedPrivateKeyBase64: string,
  saltBase64: string,
  lifecycleGuard: HydrationLifecycleGuard,
  isCurrent: () => boolean,
  nuclearReset: () => void
): Promise<PasswordChangeResult | null> {
  try {
    await e2eeService.initialize(
      newPassword,
      wrappedPrivateKeyBase64,
      saltBase64,
      'argon2id',
      lifecycleGuard
    );
    return null;
  } catch (initError) {
    if (!(initError instanceof E2EEInitTeardownError)) throw initError;
    // No await below — nuclearReset is pre-resolved — so the lifecycle is inspected
    // synchronously with no window for a newer login to slip in (coderabbit).
    if (isCurrent()) {
      nuclearReset();
      useAuthStore.getState().setLoginNotice(REAUTH_REQUIRED_NOTICE);
    } else if (useAuthStore.getState().accessToken === null) {
      // Superseded, but already unauthenticated: a self-teardown cleared the token and is
      // redirecting to login — stage the notice. NOT when a different authenticated session
      // is current (that would clobber the new session's notice).
      useAuthStore.getState().setLoginNotice(REAUTH_REQUIRED_NOTICE);
    }
    return { success: false, error: REAUTH_REQUIRED_NOTICE };
  }
}

const SYNC_DOMAIN_ENDPOINTS = [
  { name: 'preferences', endpoint: '/api/v1/users/me/preferences', responseKey: 'preferences' },
  { name: 'saved_gifs', endpoint: '/api/v1/users/me/saved-gifs', responseKey: 'saved_gifs' },
  {
    name: 'friend_organization',
    endpoint: '/api/v1/users/me/friend-organization',
    responseKey: 'friend_organization',
  },
] as const;

type SyncDomainRotation = { encrypted_data?: string; expected_version: number };
type SyncDomainName = (typeof SYNC_DOMAIN_ENDPOINTS)[number]['name'];
type SyncDomainsPayload = Record<SyncDomainName, SyncDomainRotation>;
type SyncDomainPlaintexts = Partial<Record<SyncDomainName, unknown>>;

/** Narrow an untyped JSON body to a record; anything else yields an empty one. */
function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

/**
 * Fetch each password-derived domain's authoritative server row, decrypt with
 * the HELD (old-password) key, and re-encrypt with the NEW preferences key
 * (#2200). Returns null when any fetch fails — the password change must not
 * proceed on partial knowledge. Absence comes only from the server's explicit
 * null; local stores are never consulted. An undecryptable present row becomes
 * a verify+preserve marker (version only), never an overwrite.
 */
async function assembleSyncDomainRotations(
  newPreferencesKey: CryptoKey,
  signal: AbortSignal,
  isCurrent: () => boolean
): Promise<{ payload: SyncDomainsPayload; plaintexts: SyncDomainPlaintexts } | null> {
  const payload = {} as Record<string, SyncDomainRotation>;
  const plaintexts: SyncDomainPlaintexts = {};
  for (const domain of SYNC_DOMAIN_ENDPOINTS) {
    const row = await fetchBlobRowForRotation(domain.endpoint, domain.responseKey, signal);
    assertPasswordChangeCurrent(isCurrent);
    if (row.kind === 'error') return null;
    if (row.kind === 'absent') {
      payload[domain.name] = { expected_version: 0 };
    } else if (row.kind === 'undecryptable') {
      payload[domain.name] = { expected_version: row.version };
    } else {
      const encrypted = await encryptBlob(row.plaintext, newPreferencesKey);
      assertPasswordChangeCurrent(isCurrent);
      payload[domain.name] = { encrypted_data: encrypted, expected_version: row.version };
      // Kept for the version-skew fallback: an old server that ignored
      // sync_domains leaves these rows unrotated; the plaintext lets the
      // caller re-push them under the new key (#2200 review).
      plaintexts[domain.name] = row.plaintext;
    }
  }
  return { payload: payload as SyncDomainsPayload, plaintexts };
}

/**
 * Ambiguous-commit reconciliation (#2200): a transport failure on the password
 * POST proves nothing. The submitted KDF salt is unique per attempt, so
 * comparing the server's persisted salt decides commit-vs-not before reporting
 * success or retryability.
 */
async function reconcileAmbiguousPasswordCommit(
  submittedSaltBase64: string,
  signal: AbortSignal
): Promise<'committed' | 'not-committed' | 'unknown'> {
  try {
    const res = await apiFetch('/api/v1/users/me/keys', { signal });
    if (!res.ok) return 'unknown';
    const raw: unknown = await res.json();
    const persistedSalt = asRecord(asRecord(raw).e2ee_keys).key_derivation_salt;
    if (typeof persistedSalt !== 'string') return 'unknown';
    return persistedSalt === submittedSaltBase64 ? 'committed' : 'not-committed';
  } catch {
    return 'unknown';
  }
}

type PasswordSubmitOutcome =
  | { kind: 'committed'; presenceOverrideVersion: number }
  // A version-skewed (pre-#2200) server committed the password change but
  // ignored sync_domains; the caller re-pushes these plaintexts under the
  // new key as the best-effort fallback (#2200 review, Codex P1).
  | { kind: 'committed-skew'; presenceOverrideVersion: number; plaintexts: SyncDomainPlaintexts }
  // No response body exists, so the server's sync-domain handling is unknown
  // — a skewed server may have ignored sync_domains entirely. The caller runs
  // the same new-key repair (harmless double-write against a current server).
  | { kind: 'committed-ambiguous'; plaintexts: SyncDomainPlaintexts }
  | { kind: 'terminal'; result: PasswordChangeResult };

type PasswordResponseClassification =
  | { kind: 'retry' }
  | { kind: 'committed'; presenceOverrideVersion: number; skew: boolean }
  | { kind: 'terminal'; result: PasswordChangeResult };

interface PasswordSubmitContext {
  currentPassword: string;
  newPassword: string;
  newWrappedKeyBase64: string;
  newSaltBase64: string;
  presenceOverrideEncryptedData: string;
  presenceOverrideExpectedVersion: number;
  newPreferencesKey: CryptoKey;
  signal: AbortSignal;
  isCurrent: () => boolean;
  onSessionExpired: () => void;
}

/** Map a transport failure on the password POST through salt reconciliation (#2200). */
async function reconcileAfterTransportFailure(
  ctx: PasswordSubmitContext,
  plaintexts: SyncDomainPlaintexts
): Promise<PasswordSubmitOutcome> {
  // A lifecycle abort surfaces as !isCurrent and rethrows as the cancelled
  // path; anything else is a genuinely ambiguous commit.
  assertPasswordChangeCurrent(ctx.isCurrent);
  const outcome = await reconcileAmbiguousPasswordCommit(ctx.newSaltBase64, ctx.signal);
  assertPasswordChangeCurrent(ctx.isCurrent);
  if (outcome === 'committed') return { kind: 'committed-ambiguous', plaintexts };
  if (outcome === 'not-committed') {
    return {
      kind: 'terminal',
      result: { success: false, error: 'Password change did not complete. Please retry.' },
    };
  }
  return {
    kind: 'terminal',
    result: {
      success: false,
      error:
        'Password change outcome is unknown. Try logging in with your new password before retrying.',
    },
  };
}

/** Classify a password-change HTTP response into commit / retry / terminal. */
async function classifyPasswordChangeResponse(
  response: Response,
  finalAttempt: boolean,
  isCurrent: () => boolean,
  onSessionExpired: () => void
): Promise<PasswordResponseClassification> {
  if (response.status === 401) {
    const raw: unknown = await response.json();
    assertPasswordChangeCurrent(isCurrent);
    const data = asRecord(raw);
    if (data.error === 'Current password is incorrect') {
      return { kind: 'terminal', result: { success: false, error: data.error } };
    }
    // Disk-token fate already decided by handleRefreshFailure
    // (rememberMe-aware); do NOT wipe persisted tokens here (#1768).
    onSessionExpired();
    return { kind: 'terminal', result: { success: false, error: 'Session expired' } };
  }

  const raw: unknown = await response.json();
  assertPasswordChangeCurrent(isCurrent);
  const data = asRecord(raw);

  if (response.status === 409 && data.code === 'presence_override_version_conflict') {
    await presenceOverrideSyncService.fetchAndApply();
    assertPasswordChangeCurrent(isCurrent);
    return {
      kind: 'terminal',
      result: {
        success: false,
        error: 'Presence exceptions changed on another device. Please retry password change.',
      },
    };
  }

  if (response.status === 409 && data.code === 'sync_domain_version_conflict') {
    if (finalAttempt) {
      return {
        kind: 'terminal',
        result: {
          success: false,
          error: 'Encrypted settings changed on another device. Please retry password change.',
        },
      };
    }
    return { kind: 'retry' };
  }

  if (!response.ok) {
    const message = typeof data.error === 'string' ? data.error : 'Failed to change password';
    return {
      kind: 'terminal',
      result: { success: false, error: message },
    };
  }

  const version = data.presence_override_version;
  if (!Number.isInteger(version) || (version as number) < 0) {
    throw new Error('Server returned an invalid presence override version');
  }
  // This client ALWAYS submits sync_domains, so a success response missing
  // sync_domain_versions means a version-skewed server ignored the field and
  // committed the password change without rotating the domains (#2200 review).
  // A present-but-malformed value is a server contract violation and throws.
  const skew = !validateSyncDomainVersions(data.sync_domain_versions);
  return { kind: 'committed', presenceOverrideVersion: version as number, skew };
}

/**
 * Assemble every password-derived domain from its authoritative server row and
 * submit atomically with the password change (#2200). One retry on a
 * domain-version conflict (with fresh fetches); a transport failure on the
 * POST reconciles against persisted key material before reporting.
 */
async function submitAtomicPasswordChange(
  ctx: PasswordSubmitContext
): Promise<PasswordSubmitOutcome> {
  for (let attempt = 1; attempt <= 2; attempt++) {
    const assembled = await assembleSyncDomainRotations(
      ctx.newPreferencesKey,
      ctx.signal,
      ctx.isCurrent
    );
    assertPasswordChangeCurrent(ctx.isCurrent);
    if (assembled === null) {
      return {
        kind: 'terminal',
        result: {
          success: false,
          error:
            'Could not fetch encrypted settings for rotation. Check your connection and retry.',
        },
      };
    }

    let response: Response;
    try {
      response = await apiFetch('/api/v1/users/me/password', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          current_password: ctx.currentPassword,
          new_password: ctx.newPassword,
          wrapped_private_key: ctx.newWrappedKeyBase64,
          key_derivation_salt: ctx.newSaltBase64,
          key_derivation_alg: 'argon2id',
          presence_override: {
            encrypted_data: ctx.presenceOverrideEncryptedData,
            expected_version: ctx.presenceOverrideExpectedVersion,
          },
          sync_domains: assembled.payload,
        }),
        signal: ctx.signal,
      });
    } catch {
      return reconcileAfterTransportFailure(ctx, assembled.plaintexts);
    }
    assertPasswordChangeCurrent(ctx.isCurrent);

    const classified = await classifyPasswordChangeResponse(
      response,
      attempt === 2,
      ctx.isCurrent,
      ctx.onSessionExpired
    );
    if (classified.kind === 'retry') continue;
    if (classified.kind === 'committed' && classified.skew) {
      return {
        kind: 'committed-skew',
        presenceOverrideVersion: classified.presenceOverrideVersion,
        plaintexts: assembled.plaintexts,
      };
    }
    if (classified.kind === 'committed') {
      return { kind: 'committed', presenceOverrideVersion: classified.presenceOverrideVersion };
    }
    return classified;
  }
  // Unreachable: the final attempt never classifies as retry.
  return { kind: 'terminal', result: { success: false, error: 'Failed to change password' } };
}

/**
 * Strict best-effort repair for servers that ignored sync_domains: re-encrypt
 * each fetched plaintext under the LIVE (new) preferences key and PUT it,
 * REPORTING failures instead of swallowing them (#2200 review, Codex —
 * pushEncryptedBlob's fire-and-forget contract is wrong for a repair whose
 * silent failure strands the domain under the old key). Returns the domain
 * names whose repair did not land.
 */
async function repushDomainsUnderLiveKey(
  plaintexts: SyncDomainPlaintexts,
  signal: AbortSignal,
  isCurrent: () => boolean
): Promise<string[]> {
  const failed: string[] = [];
  for (const domain of SYNC_DOMAIN_ENDPOINTS) {
    const plaintext = plaintexts[domain.name];
    if (plaintext === undefined) continue;
    try {
      const encrypted = await e2eeService.encryptPreferences(plaintext);
      assertPasswordChangeCurrent(isCurrent);
      const res = await apiFetch(domain.endpoint, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ encrypted_data: encrypted }),
        signal,
      });
      assertPasswordChangeCurrent(isCurrent);
      if (!res.ok) failed.push(domain.name);
    } catch (error) {
      if (!isCurrent()) throw error;
      failed.push(domain.name);
    }
  }
  return failed;
}

/**
 * Returns true when the response carries a well-formed sync_domain_versions
 * object; false when it is ABSENT (the version-skew signal — an old server
 * ignored the submitted sync_domains); throws on present-but-malformed.
 */
function validateSyncDomainVersions(value: unknown): boolean {
  if (value === undefined) return false;
  if (typeof value !== 'object' || value === null || Array.isArray(value)) {
    throw new Error('Server returned invalid sync domain versions');
  }
  for (const domain of ['preferences', 'saved_gifs', 'friend_organization']) {
    const version = (value as Record<string, unknown>)[domain];
    if (!Number.isInteger(version) || (version as number) < 0) {
      throw new Error('Server returned invalid sync domain versions');
    }
  }
  return true;
}

export const useUserStore = wrapStore(
  create<UserState>()(
    devtools(
      (set, get) => ({
        user: null,
        isLoading: false,
        error: null,

        fetchUser: async (guard) => {
          if (!isHydrationLifecycleCurrent(guard)) return;
          const lifecycle = beginUserFetch(guard);
          const isCurrent = (): boolean => isUserFetchCurrent(lifecycle);
          set({ isLoading: true, error: null });

          try {
            const response = await apiFetch('/api/v1/users/me', {
              signal: lifecycle.controller.signal,
            });
            if (!isCurrent()) return;

            if (response.status === 401) {
              // Token expired and refresh failed. apiClient.handleRefreshFailure
              // (inside apiFetch) already made the rememberMe-aware disk decision
              // — do NOT also call electron.clearTokens() here, which would wipe a
              // "Remember Me" session unconditionally on a transient 401 (#1768).
              useAuthStore.getState().clearAccessToken();
              set({ user: null, isLoading: false, error: null });
              return;
            }

            if (!response.ok) {
              const data = await response.json();
              if (!isCurrent()) return;
              throw new Error(data.error || 'Failed to fetch user');
            }

            const data = await response.json();
            if (!isCurrent()) return;
            if (typeof data.user?.id === 'string') {
              cancelPasswordChangeForDifferentAccount(data.user.id);
            }
            set({ user: data.user, isLoading: false });

            // Sync email_verified to authStore so route guards reflect DB state
            if (data.user && typeof data.user.email_verified === 'boolean') {
              useAuthStore.getState().setEmailVerified(data.user.email_verified);
            }
          } catch (error) {
            if (!isCurrent()) return;
            set({
              error: error instanceof Error ? error.message : 'Failed to fetch user',
              isLoading: false,
            });
          } finally {
            finishUserFetch(lifecycle);
          }
        },

        setUser: (user: UserProfile) => {
          cancelActiveUserFetch();
          cancelPasswordChangeForDifferentAccount(user.id);
          set({ user, isLoading: false, error: null });
        },

        clearUser: () => {
          cancelActiveUserFetch();
          cancelActivePasswordChange();
          set({ user: null, isLoading: false, error: null });
        },

        logout: async () => {
          resetPostLoginHydrationLifecycle();
          cancelActiveUserFetch();
          cancelActivePasswordChange();
          // Stop syncing preferences and saved GIFs before tearing down
          preferencesSyncService.stopWatching();
          savedGifsSyncService.stopWatching();
          friendOrgSyncService.stopWatching();
          presenceOverrideSyncService.reset();
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
            // Disk-token fate already decided by handleRefreshFailure
            // (rememberMe-aware); do NOT wipe persisted tokens here (#1768).
            useAuthStore.getState().clearAccessToken();
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
          const initiatingUserId = get().user?.id;
          if (initiatingUserId === undefined || useAuthStore.getState().accessToken === null) {
            return { success: false, error: 'Session expired' };
          }

          const lifecycle = beginPasswordChange(initiatingUserId);
          const isCurrent = (): boolean =>
            activePasswordChange === lifecycle &&
            !lifecycle.controller.signal.aborted &&
            get().user?.id === initiatingUserId;
          const lifecycleGuard = {
            signal: lifecycle.controller.signal,
            isCurrent,
          };
          try {
            // Step 1: Fetch current E2EE keys from the server
            const keysRes = await apiFetch('/api/v1/users/me/keys', {
              signal: lifecycle.controller.signal,
            });
            assertPasswordChangeCurrent(isCurrent);

            if (!keysRes.ok) {
              const keysData = await keysRes.json();
              assertPasswordChangeCurrent(isCurrent);
              return { success: false, error: keysData.error || 'Failed to fetch encryption keys' };
            }

            const keysData = await keysRes.json();
            assertPasswordChangeCurrent(isCurrent);
            const { wrapped_private_key, key_derivation_salt } = keysData.e2ee_keys;
            const currentAlg: KeyDerivationAlgorithm =
              keysData.e2ee_keys.key_derivation_alg || 'pbkdf2';

            // Step 2: Unwrap private key with current password (using stored algorithm)
            const currentSalt = new Uint8Array(base64ToArrayBuffer(key_derivation_salt));
            const currentWrappingKey =
              currentAlg === 'argon2id'
                ? await deriveKeyArgon2id(currentPassword, currentSalt)
                : await deriveKeyFromPassword(currentPassword, currentSalt);
            assertPasswordChangeCurrent(isCurrent);
            const wrappedKeyBuffer = base64ToArrayBuffer(wrapped_private_key);
            const privateKey = await unwrapPrivateKey(wrappedKeyBuffer, currentWrappingKey);
            assertPasswordChangeCurrent(isCurrent);

            // Step 3: Re-wrap private key with new password (always Argon2id)
            const newSalt = generateSalt();
            const newWrappingKey = await deriveKeyArgon2id(newPassword, newSalt);
            assertPasswordChangeCurrent(isCurrent);
            const newWrappedKey = await wrapPrivateKey(privateKey, newWrappingKey);
            assertPasswordChangeCurrent(isCurrent);
            // Build the replacement override ciphertext with the NEW
            // password-derived preferences key without mutating the live E2EE
            // service. The server CASes this document in the same transaction
            // as the password/key rotation, so success can never strand
            // old-key ciphertext.
            const overrideState = usePresenceOverrideStore.getState();
            const presenceOverrideSnapshot = parsePresenceOverrides({
              v: 1,
              excludedUserIds: [...overrideState.excludedUserIds],
            });
            const newPreferencesKey = await derivePreferencesKeyArgon2id(newPassword, newSalt);
            assertPasswordChangeCurrent(isCurrent);
            const presenceOverrideEncryptedData = await encryptBlob(
              presenceOverrideSnapshot,
              newPreferencesKey
            );
            assertPasswordChangeCurrent(isCurrent);

            // Pre-load the reset primitive BEFORE the irreversible password POST so the
            // fail-closed path (reinitE2EEOrFailClosed) has NO await: a chunk-load failure
            // surfaces here — before the change commits — instead of leaving a committed
            // change unable to fail closed (CWE-693), and the teardown can check the
            // lifecycle synchronously with no window for a newer login to slip in (which
            // would otherwise get its notice clobbered). The dynamic import is required —
            // resetService imports userStore, so a static import would cycle (coderabbit).
            const { nuclearReset } = await import('../services/resetService');

            const newWrappedKeyBase64 = arrayBufferToBase64(newWrappedKey);
            const newSaltBase64 = arrayBufferToBase64(newSalt.buffer as ArrayBuffer);

            // Quiesce the domain sync watchers for the rotation window (#2200
            // review, Codex P2): a debounced old-key push landing after the
            // server commit would overwrite an atomically rotated row. The
            // finally block restarts them only while this session is still
            // current (account teardown owns them otherwise). startWatching is
            // idempotent (it stops first), so early-return paths are safe.
            // Accepted residual: a local edit inside its ≤3s debounce at this
            // instant stays local-only until the user's next edit of that
            // domain (no data loss — the store keeps it; the rotation pushes
            // the authoritative SERVER state). The alternative — letting the
            // debounce fire — is the old-key-overwrite race this quiesce
            // exists to close.
            preferencesSyncService.stopWatching();
            savedGifsSyncService.stopWatching();
            friendOrgSyncService.stopWatching();

            // Step 4: atomic submission with retry-once + ambiguity
            // reconciliation lives in submitAtomicPasswordChange (#2200).
            const submit = await submitAtomicPasswordChange({
              currentPassword,
              newPassword,
              newWrappedKeyBase64,
              newSaltBase64,
              presenceOverrideEncryptedData,
              presenceOverrideExpectedVersion: overrideState.appliedVersion,
              newPreferencesKey,
              signal: lifecycle.controller.signal,
              isCurrent,
              onSessionExpired: () => {
                useAuthStore.getState().clearAccessToken();
                set({ user: null, isLoading: false, error: null });
              },
            });
            if (submit.kind === 'terminal') return submit.result;

            // Re-initialize E2EE with the new password. The password POST above already
            // committed, so a teardown during the derivation (E2EEInitTeardownError, #2337)
            // is NOT retryable — reinitE2EEOrFailClosed fails closed with an honest re-auth
            // notice rather than the misleading "Password change was cancelled" (#2333
            // salvage). Returns non-null only on that fail-closed path.
            const failClosed = await reinitE2EEOrFailClosed(
              newPassword,
              newWrappedKeyBase64,
              newSaltBase64,
              lifecycleGuard,
              isCurrent,
              nuclearReset
            );
            if (failClosed) return failClosed;
            assertPasswordChangeCurrent(isCurrent);

            if (submit.kind === 'committed' || submit.kind === 'committed-skew') {
              usePresenceOverrideStore
                .getState()
                .apply(presenceOverrideSnapshot.excludedUserIds, submit.presenceOverrideVersion);
            } else {
              // Ambiguous-commit path: no response body — refetch the
              // authoritative presence exception state under the new key.
              await presenceOverrideSyncService.fetchAndApply();
              assertPasswordChangeCurrent(isCurrent);
            }

            if (submit.kind === 'committed-skew' || submit.kind === 'committed-ambiguous') {
              // The server may have ignored sync_domains (skewed response, or
              // an ambiguous commit whose server version is unknowable), so
              // restore the pre-#2200 best-effort coverage: the live
              // e2eeService now holds the NEW key (reinit above); re-push each
              // decryptable domain's plaintext under it. Against a current
              // server this is a harmless same-plaintext double-write. Repair
              // failures SURFACE — the password did change, but the user must
              // know their synced settings need attention (#2200 review).
              console.warn(
                'Password change committed without confirmed sync-domain rotation; re-pushing domains under the new key'
              );
              const failedRepairs = await repushDomainsUnderLiveKey(
                submit.plaintexts,
                lifecycle.controller.signal,
                isCurrent
              );
              assertPasswordChangeCurrent(isCurrent);
              if (failedRepairs.length > 0) {
                return {
                  success: false,
                  error:
                    'Your password WAS changed, but re-encrypting some synced settings failed ' +
                    `(${failedRepairs.join(', ')}). Log in with your new password; they will repair on the next edit.`,
                };
              }
            }

            return { success: true };
          } catch (error) {
            return passwordChangeFailure(error, isCurrent);
          } finally {
            // Restart the quiesced watchers only for the still-current session
            // (checked BEFORE finishPasswordChange nulls the lifecycle); a
            // superseded/aborted change leaves them to account teardown.
            if (isCurrent()) {
              preferencesSyncService.startWatching();
              savedGifsSyncService.startWatching();
              friendOrgSyncService.startWatching();
            }
            finishPasswordChange(lifecycle);
          }
        },
      }),
      { name: 'UserStore' }
    )
  )
);
