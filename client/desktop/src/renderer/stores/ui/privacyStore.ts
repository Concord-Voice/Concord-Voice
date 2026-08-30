import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/createStore';
import { apiFetch } from '../../services/apiClient';

// DM Privacy Levels:
// 0 = Off (no DMs at all)
// 1 = Friends Only
// 2 = Friends + Server Members (default)
// 3 = Allow All
export type DMPrivacyLevel = 0 | 1 | 2 | 3;

/**
 * #1241: who may send this user a friend request. Mirrors the CHECK constraint
 * on `privacy_settings.allow_friend_requests_from`.
 */
export type FriendRequestPrivacyMode = 'everyone' | 'mutual_servers' | 'nobody';

const FRIEND_REQUEST_MODES: ReadonlySet<FriendRequestPrivacyMode> = new Set([
  'everyone',
  'mutual_servers',
  'nobody',
]);

/**
 * Narrow a wire value to the enum. A bare `as` cast would let an unexpected
 * string reach the UI, where `indexOf` returns -1 and the control renders a
 * thumb clamped to one mode while its accessible name announces another — the
 * display and the announced value disagreeing on a privacy setting.
 */
function asFriendRequestMode(value: unknown): FriendRequestPrivacyMode {
  return FRIEND_REQUEST_MODES.has(value as FriendRequestPrivacyMode)
    ? (value as FriendRequestPrivacyMode)
    : 'everyone';
}

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
  // #1241: who may send this user a friend request. The server gate is
  // authoritative; the client gate is presentation only.
  allowFriendRequestsFrom: FriendRequestPrivacyMode;
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
  allowFriendRequestsFrom: 'allow_friend_requests_from',
};

/**
 * #1354: a PATCH that carries only `require_auth_before_purge` is rejected with
 * this exact 400 by a control-plane that predates the field. Surfaced verbatim
 * from the copy deck — the user flipped a switch and must learn it did not take.
 */
export const PURGE_AUTH_SKEW_MESSAGE =
  "This version of the server doesn't support this setting yet.";

/**
 * #1241: same copy as the purge fence — the user's problem is identical (the
 * server is too old for this setting), only the field differs.
 */
export const FRIEND_REQUEST_SKEW_MESSAGE = PURGE_AUTH_SKEW_MESSAGE;

/**
 * Fields whose absence from an older control-plane produces the empty-body 400.
 * A field listed here gets version-skew copy instead of the raw server string.
 *
 * A table rather than a per-field boolean argument: two booleans is a smell and
 * three is a bug, and every entry here shares one wire signature.
 */
const SKEW_MESSAGES: Partial<Record<keyof PrivacySettings, string>> = {
  requireAuthBeforePurge: PURGE_AUTH_SKEW_MESSAGE,
  allowFriendRequestsFrom: FRIEND_REQUEST_SKEW_MESSAGE,
};

/**
 * The version-skew copy for the first skew-bearing field in this update, if any.
 *
 * Extracted rather than inlined into classifyPrivacyRefusal: the loop pushed
 * that function's cognitive complexity to 18 against a limit of 15, and the
 * lookup is a self-contained question anyway.
 */
function skewMessageFor(updatedFields: (keyof PrivacySettings)[]): string | undefined {
  for (const field of updatedFields) {
    const skew = SKEW_MESSAGES[field];
    if (skew) return skew;
  }
  return undefined;
}

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
  updatedFields: (keyof PrivacySettings)[]
): PrivacyUpdateRefusal {
  const message = body.error || GENERIC_UPDATE_FAILURE;
  const touchesPurgeFence = updatedFields.includes('requireAuthBeforePurge');

  // #1354: an old control-plane rejects a body it can map no columns from as
  // empty. Checked ahead of every #2765 shape so version skew keeps its own
  // copy, and per-field so each setting names itself.
  if (status === 400 && body.error === 'No fields to update') {
    const skew = skewMessageFor(updatedFields);
    if (skew) return { kind: 'refused', message: skew };
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

/**
 * Identity + write-ordering fence for every response that writes `settings`.
 *
 * Two hazards, one mechanism, both proven by an adversarial pass on PR #2888:
 *
 * 1. IDENTITY. `clearPrivacy()` (called synchronously by `gracefulReset()` on
 *    logout) is synchronous; an in-flight GET/PATCH on `/users/me/privacy` is
 *    not. Its continuation ran AFTER the clear and wrote the PREVIOUS account's
 *    privacy posture back into the store — stamped `loaded: true`, so the next
 *    account on the device was shown account A's searchability, DM posture and
 *    `allow_friend_requests_from` as SERVER-CONFIRMED. The `loaded` flag, added
 *    to guarantee nothing is presented as the user's own choice until the
 *    server confirms it, is precisely what forged the claim.
 *
 * 2. ORDERING. Writes were last-RESPONSE-wins, not last-REQUEST-wins. A slow
 *    earlier PATCH could overwrite a newer one, and a GET issued before a PATCH
 *    committed could roll the display back afterwards — reachable without racy
 *    clicking, since the access-token effect refires `fetchPrivacy` on every
 *    token refresh.
 *
 * This is the same remedy as the generation fence in
 * `services/friendEligibility.ts`. Any handler that writes `settings` must take
 * a ticket BEFORE its await and gate the write on it.
 */
let privacyIdentity = 0;
let privacyIssued = 0;
let privacySettled = 0;

interface PrivacyWriteTicket {
  identity: number;
  seq: number;
}

function beginPrivacyWrite(): PrivacyWriteTicket {
  privacyIssued += 1;
  return { identity: privacyIdentity, seq: privacyIssued };
}

/**
 * True only for the newest request of the CURRENT account to settle. Records
 * the advance, so it is a one-shot per request.
 *
 * Success and refusal are both outcomes and share the one watermark: whichever
 * request settles last is the one the store describes. A superseded refusal is
 * stale by definition — the user's newer choice already landed — so surfacing
 * it would report a failure about a selection they have since replaced (#2903
 * AC-2). Zustand's own `persist` middleware fences its rehydrate error path the
 * same way, on the same counter it uses for the success path.
 *
 * This gates only what is WRITTEN to the store. `updatePrivacy` still throws
 * for the caller awaiting it whether or not its refusal was recorded, so a
 * component that wants to react to its own superseded failure still can.
 */
function maySettle(ticket: PrivacyWriteTicket): boolean {
  if (ticket.identity !== privacyIdentity) return false;
  if (ticket.seq <= privacySettled) return false;
  privacySettled = ticket.seq;
  return true;
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
  // #1241: matches the column default. Permissive is correct here — a
  // restrictive placeholder would hide the affordance for every user whose
  // settings have not loaded yet, on a value the server has not yet spoken to.
  allowFriendRequestsFrom: 'everyone',
};

interface PrivacyState {
  settings: PrivacySettings;
  isLoading: boolean;
  /**
   * True once the server has confirmed these settings at least once.
   *
   * NOT a member of PrivacySettings: INVARIANT #2765 requires every field there
   * to be a real `privacy_settings` column, and this is client session state.
   *
   * Security-adjacent controls must not present `defaultSettings` as if it were
   * the user's own choice. `isLoading` alone is insufficient — it returns to
   * false when a fetch FAILS, which would leave the permissive defaults on
   * screen indefinitely, telling a user who chose `nobody` that anyone may
   * contact them.
   */
  loaded: boolean;
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
        loaded: false,
        error: null,

        fetchPrivacy: async () => {
          const ticket = beginPrivacyWrite();
          set({ isLoading: true, error: null });
          try {
            const response = await apiFetch('/api/v1/users/me/privacy');
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to load privacy settings');
            }
            const data = await response.json();
            const p = data.privacy;
            if (!maySettle(ticket)) return;
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
                // #1241: an absent key is a pre-#1240 server, not a
                // restrictive setting. Default open — the server still enforces.
                allowFriendRequestsFrom: asFriendRequestMode(p.allow_friend_requests_from),
              },
              isLoading: false,
              // Only here. The catch below deliberately leaves it as-is: a
              // failed fetch means the settings on screen are still defaults.
              loaded: true,
            });
          } catch (error) {
            if (!maySettle(ticket)) return;
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
          const ticket = beginPrivacyWrite();
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
              Object.keys(updates) as (keyof PrivacySettings)[]
            );
            // Still throws for the awaiting caller, but a previous account's
            // failure — or one a newer request has already superseded — must
            // not be recorded on the store.
            if (maySettle(ticket)) set({ error: refusal.message });
            throw new PrivacyUpdateError(refusal);
          }

          const data = await response.json();
          const p = data.privacy;
          if (!maySettle(ticket)) return;
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
              // #1241: an absent key is a pre-#1240 server, not a
              // restrictive setting. Default open — the server still enforces.
              allowFriendRequestsFrom: asFriendRequestMode(p.allow_friend_requests_from),
            },
            // A successful PATCH echoes the full authoritative object, so it
            // confirms the settings just as a GET does. Without this, a failed
            // initial fetch followed by any successful write left the control
            // disabled on "Loading your setting…" indefinitely while the store
            // already held real server data.
            loaded: true,
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

        clearPrivacy: () => {
          // Bump identity FIRST, and retire every issued ticket, so nothing
          // still in flight can write after this point.
          privacyIdentity += 1;
          privacySettled = privacyIssued;
          set({ settings: { ...defaultSettings }, loaded: false, error: null });
        },
      }),
      { name: 'PrivacyStore' }
    )
  )
);
