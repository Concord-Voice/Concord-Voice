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

const GENERIC_UPDATE_FAILURE = 'Failed to update privacy settings';

/**
 * #2765: the step-up factors for the one gated transition — turning
 * `require_auth_before_purge` OFF. These are function arguments and nothing
 * else: they are never written to store state, never persisted, and never
 * logged (`[internal]rules/observability.md` Core principle #1).
 */
export interface PrivacyStepUpCredentials {
  currentPassword?: string;
  mfaCode?: string;
}

/** The error shapes the privacy PATCH can answer with. */
interface PrivacyErrorBody {
  error?: string;
  password_required?: boolean;
  mfa_required?: boolean;
  methods?: string[];
}

/**
 * A refused privacy update, classified. Every arm carries `message` so the
 * generic surfaces (the section banner, the thrown Error) stay uniform, while
 * the `kind` lets the step-up dialog place the failure on the field that owns
 * it. Copy for the two per-field arms lives with the dialog — this module maps
 * the wire, not the words.
 */
export type PrivacyUpdateRefusal =
  | { kind: 'passwordRequired'; message: string }
  | { kind: 'mfaRequired'; methods: string[]; message: string }
  | { kind: 'invalidPassword'; message: string }
  | { kind: 'invalidMfaCode'; message: string }
  | { kind: 'stepUpImpossible'; message: string }
  | { kind: 'refused'; message: string };

/** What `disablePurgeFence` answers with. It never throws. */
export type PurgeFenceDisableResult = { kind: 'accepted' } | PrivacyUpdateRefusal;

/**
 * Maps a non-2xx privacy PATCH onto {@link PrivacyUpdateRefusal}.
 *
 * The two boolean flags are the intended contract and are matched first. The
 * two string comparisons below are NOT: they match human-readable prose from
 * `internal/stepup`, which carries no machine-readable discriminator — the same
 * known brittleness `services/purgeApi.ts` documents, kept byte-identical on
 * purpose so both clients degrade the same way. Rewording either server string
 * downgrades a per-field error to the generic banner; nothing is misreported.
 */
export function classifyPrivacyRefusal(
  status: number,
  body: PrivacyErrorBody,
  touchesPurgeFence: boolean
): PrivacyUpdateRefusal {
  const message = body.error || GENERIC_UPDATE_FAILURE;

  // #1354: an old control-plane rejects a toggle-only body as empty. Checked
  // ahead of every #2765 shape so version skew keeps its own copy.
  if (status === 400 && body.error === 'No fields to update' && touchesPurgeFence) {
    return { kind: 'refused', message: PURGE_AUTH_SKEW_MESSAGE };
  }

  if (status === 403) {
    if (body.password_required) return { kind: 'passwordRequired', message };
    // An account with MFA but no password is answered with `mfa_required` and
    // no `password_required` — that absence is the signal to drop the password
    // field, because a passwordless SSO account has nothing to type there.
    if (body.mfa_required) return { kind: 'mfaRequired', methods: body.methods ?? [], message };
    if (body.error === 'Invalid password') return { kind: 'invalidPassword', message };
    if (body.error === 'Invalid MFA code') return { kind: 'invalidMfaCode', message };
    return { kind: 'refused', message };
  }

  // A 400 on the gated transition means the account carries neither a password
  // nor MFA: no credential exists that could satisfy the step-up, so the
  // server's message is the whole answer and there is nothing to retry.
  if (status === 400 && touchesPurgeFence) return { kind: 'stepUpImpossible', message };

  return { kind: 'refused', message };
}

/**
 * Carries the classification to a caller that wants it, while keeping
 * `updatePrivacy`'s existing throw-with-a-message contract intact for the
 * twelve callers that only render `err.message`.
 */
export class PrivacyUpdateError extends Error {
  public readonly refusal: PrivacyUpdateRefusal;

  constructor(refusal: PrivacyUpdateRefusal) {
    super(refusal.message);
    this.name = 'PrivacyUpdateError';
    this.refusal = refusal;
  }
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
  updatePrivacy: (
    updates: Partial<PrivacySettings>,
    credentials?: PrivacyStepUpCredentials
  ) => Promise<void>;
  /**
   * #2765: the gated OFF transition. Resolves with the classified outcome
   * instead of throwing, so the step-up dialog can place a rejected factor on
   * the field that owns it.
   */
  disablePurgeFence: (credentials: PrivacyStepUpCredentials) => Promise<PurgeFenceDisableResult>;
  clearPrivacy: () => void;
}

export const usePrivacyStore = wrapStore(
  create<PrivacyState>()(
    devtools(
      (set, get) => ({
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

        updatePrivacy: async (
          updates: Partial<PrivacySettings>,
          credentials?: PrivacyStepUpCredentials
        ) => {
          const body: Record<string, boolean | number | string> = {};
          for (const [field, wireField] of Object.entries(PRIVACY_WIRE_FIELDS)) {
            const value = updates[field as keyof PrivacySettings];
            if (value !== undefined) body[wireField] = value;
          }
          // #2765: the factors travel only when the caller actually holds them.
          // An absent factor must not go out as an empty string — the server
          // reads that as a supplied-and-wrong one.
          if (credentials?.currentPassword) body.current_password = credentials.currentPassword;
          if (credentials?.mfaCode) body.mfa_code = credentials.mfaCode;

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
            const data: PrivacyErrorBody = await response.json().catch(() => ({}));
            // #1354 skew and the #2765 step-up shapes are both decided by the
            // classifier, so the two clients of this endpoint cannot drift.
            // The message is recorded on the store either way, so a caller that
            // only awaits cannot swallow the failure silently.
            const refusal = classifyPrivacyRefusal(
              response.status,
              data,
              updates.requireAuthBeforePurge !== undefined
            );
            set({ error: refusal.message });
            throw new PrivacyUpdateError(refusal);
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

        disablePurgeFence: async (credentials: PrivacyStepUpCredentials) => {
          try {
            await get().updatePrivacy({ requireAuthBeforePurge: false }, credentials);
            return { kind: 'accepted' };
          } catch (error) {
            if (error instanceof PrivacyUpdateError) return error.refusal;
            // apiFetch rejects — rather than resolving with a status — when no
            // response arrives at all. Nothing was changed, so it is a refusal
            // like any other; only the message comes from the transport.
            return {
              kind: 'refused',
              message: error instanceof Error ? error.message : GENERIC_UPDATE_FAILURE,
            };
          }
        },

        clearPrivacy: () => set({ settings: { ...defaultSettings }, error: null }),
      }),
      { name: 'PrivacyStore' }
    )
  )
);
