import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import { apiFetch } from '../../services/system/apiClient';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  isAbortError,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { classifyStepUpRefusal, isStepUpFactorRefusal } from '../../services/system/stepUpRefusal';

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
  // An older control-plane omits this field and still defaults to Everyone.
  if (value == null) return 'everyone';
  return FRIEND_REQUEST_MODES.has(value as FriendRequestPrivacyMode)
    ? (value as FriendRequestPrivacyMode)
    : 'mutual_servers';
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

/**
 * What `disablePurgeFence` answers with. It never throws. `aborted` means the
 * request was never sent: `apiFetch`'s pre-dispatch fence refused it because
 * the account or server changed since `context` was captured.
 */
export type PurgeFenceDisableResult =
  { kind: 'accepted' } | { kind: 'aborted' } | PrivacyUpdateRefusal;

/**
 * Maps a non-2xx privacy PATCH onto {@link PrivacyUpdateRefusal}.
 *
 * A 403 is read by the shared step-up classifier
 * (`services/system/stepUpRefusal.ts`), the same one `purgeApi.ts` and the MFA
 * settings actions use, so the clients of the `internal/stepup` seam cannot
 * drift and degrade identically if a seam string is ever reworded. Every other
 * status — including the purge fence's 429 and its budget-outage 503 — stays a
 * `refused` carrying the server's own text, which is the copy written for it.
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
    // `mfa_required` arrives AFTER a correct password (the seam checks the
    // password first), so it is never a signal to drop the password field —
    // PurgeFenceStepUpDialog keeps it up for exactly that reason (#2792).
    const refusal = classifyStepUpRefusal(status, body);
    // E8: the account holds no factor any input could supply, so it is the
    // dead end `stepUpImpossible` already names. As `refused` it would leave
    // the credentials up and invite a retry no input can complete. The
    // privacy PATCH does not send it today; the server's text is the copy.
    if (refusal.kind === 'enrollmentRequired') return { kind: 'stepUpImpossible', message };
    return isStepUpFactorRefusal(refusal) ? { ...refusal, message } : { kind: 'refused', message };
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
 * Requests retain an identity ticket before awaiting. Full reads are ordered
 * by that ticket; mutations own only their specified fields, so independent
 * writes cannot discard or overwrite each other's confirmed values.
 */
let privacyIdentity = 0;
let privacyIssued = 0;
let privacySettled = 0;
type PrivacyField = keyof PrivacySettings;
const PRIVACY_FIELDS = Object.keys(PRIVACY_WIRE_FIELDS) as PrivacyField[];

interface PrivacyFieldOwner {
  readonly identity: number;
}

let privacyFieldOwners: Partial<Record<PrivacyField, PrivacyFieldOwner>> = {};
let privacyFieldConfirmations: Partial<Record<PrivacyField, number>> = {};

interface PrivacyMutation {
  ticket: PrivacyWriteTicket;
  context: ApiRequestContext;
  fields: PrivacyField[];
  owners: Partial<Record<PrivacyField, PrivacyFieldOwner>>;
  confirmations: Partial<Record<PrivacyField, number>>;
}

let privacyMutationFailure: { mutation: PrivacyMutation; fields: PrivacyField[] } | null = null;

// These two queues belong to the account, not the mounted Settings pane.
// Replacing a selection supersedes only its own field, including an older
// request whose response arrives during the replacement's debounce window.
interface PrivacyTierSelection {
  identity: number;
  context: ApiRequestContext;
  owner: PrivacyFieldOwner;
}

let dmPrivacySelection: PrivacyTierSelection | null = null;
let friendRequestSelection: PrivacyTierSelection | null = null;
let dmPrivacyTimer: ReturnType<typeof setTimeout> | null = null;
let friendRequestTimer: ReturnType<typeof setTimeout> | null = null;

// A current direct write to a tier is a successor selection too. Bind a
// queued request to its owner, or retire that field's queued predecessor.
// An expired context owns no current queue and must leave it untouched.
function bindDMPrivacyWrite(
  level: DMPrivacyLevel | undefined,
  context: ApiRequestContext | undefined
): PrivacyTierSelection | null {
  if (level === undefined) return null;
  const selection = dmPrivacySelection;
  if (selection?.context === context) return selection;
  if (!selection || (context && !apiRequestContextIsCurrent(context))) return null;
  dmPrivacySelection = null;
  if (dmPrivacyTimer !== null) clearTimeout(dmPrivacyTimer);
  dmPrivacyTimer = null;
  usePrivacyStore.setState({ pendingDmPrivacyLevel: null, dmSaveError: null });
  return null;
}

function bindFriendRequestPrivacyWrite(
  mode: FriendRequestPrivacyMode | undefined,
  context: ApiRequestContext | undefined
): PrivacyTierSelection | null {
  if (mode === undefined) return null;
  const selection = friendRequestSelection;
  if (selection?.context === context) return selection;
  if (!selection || (context && !apiRequestContextIsCurrent(context))) return null;
  friendRequestSelection = null;
  if (friendRequestTimer !== null) clearTimeout(friendRequestTimer);
  friendRequestTimer = null;
  usePrivacyStore.setState({ pendingFriendRequestMode: null, friendRequestError: null });
  return null;
}

interface PrivacyWriteTicket {
  identity: number;
  seq: number;
}

function beginPrivacyWrite(): PrivacyWriteTicket {
  privacyIssued += 1;
  return { identity: privacyIdentity, seq: privacyIssued };
}

/**
 * Orders full reads and the first full confirmation for the current account.
 * Loaded mutations instead settle their own fields: a newer unrelated write
 * cannot retire an older field's accepted result or refusal.
 */
function maySettle(ticket: PrivacyWriteTicket): boolean {
  if (ticket.identity !== privacyIdentity) return false;
  if (ticket.seq <= privacySettled) return false;
  privacySettled = ticket.seq;
  return true;
}

function beginPrivacyMutation(
  updates: Partial<PrivacySettings>,
  context: ApiRequestContext | undefined
): PrivacyMutation {
  const requestContext = context ?? captureApiRequestContext();
  // An expired explicit context must not displace a live selection, even
  // while apiFetch's own asynchronous admission has not run yet.
  if (!apiRequestContextIsCurrent(requestContext)) {
    throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
  }
  const ticket = beginPrivacyWrite();
  const confirmations = { ...privacyFieldConfirmations };
  const fields = PRIVACY_FIELDS.filter((field) => updates[field] !== undefined);
  const tierOwners: Partial<Record<PrivacyField, PrivacyFieldOwner>> = {
    dmPrivacyLevel: dmPrivacySelection?.context === context ? dmPrivacySelection?.owner : undefined,
    allowFriendRequestsFrom:
      friendRequestSelection?.context === context ? friendRequestSelection?.owner : undefined,
  };
  const owners: Partial<Record<PrivacyField, PrivacyFieldOwner>> = {};
  for (const field of fields) {
    const owner = tierOwners[field] ?? ticket;
    owners[field] = owner;
    privacyFieldOwners[field] = owner;
  }
  // Claim before notifications: a subscriber's successor action must retain
  // ownership rather than being overwritten when this older call resumes.
  if (privacyFieldOwners.dmPrivacyLevel === owners.dmPrivacyLevel) {
    bindDMPrivacyWrite(updates.dmPrivacyLevel, context);
  }
  if (privacyFieldOwners.allowFriendRequestsFrom === owners.allowFriendRequestsFrom) {
    bindFriendRequestPrivacyWrite(updates.allowFriendRequestsFrom, context);
  }
  if (ticket.identity !== privacyIdentity || !apiRequestContextIsCurrent(requestContext)) {
    throw new DOMException('Request lifecycle changed before dispatch', 'AbortError');
  }
  return { ticket, context: requestContext, fields, owners, confirmations };
}

function ownedPrivacyMutationFields(mutation: PrivacyMutation): PrivacyField[] {
  if (mutation.ticket.identity !== privacyIdentity || !apiRequestContextIsCurrent(mutation.context))
    return [];
  return mutation.fields.filter((field) => mutation.owners[field] === privacyFieldOwners[field]);
}

function mergePrivacyFields(
  current: PrivacySettings,
  incoming: PrivacySettings,
  fields: PrivacyField[]
): PrivacySettings {
  let merged = current;
  for (const field of fields) merged = { ...merged, [field]: incoming[field] };
  return merged;
}

function privacyMutationFailureIsCurrent(): boolean {
  const failure = privacyMutationFailure;
  if (!failure) return false;
  const owned = ownedPrivacyMutationFields(failure.mutation);
  return failure.fields.some((field) => owned.includes(field));
}

function publishPrivacyMutation(mutation: PrivacyMutation, incoming: PrivacySettings): void {
  const fields = ownedPrivacyMutationFields(mutation);
  if (fields.length === 0) return;
  const current = usePrivacyStore.getState();
  const hydrated = !current.loaded && maySettle(mutation.ticket);
  let settings = current.settings;
  if (hydrated) {
    const protectedFields = PRIVACY_FIELDS.filter(
      (field) => mutation.confirmations[field] !== privacyFieldConfirmations[field]
    );
    settings = mergePrivacyFields(incoming, settings, protectedFields);
  }
  settings = mergePrivacyFields(settings, incoming, fields);
  for (const field of fields) {
    privacyFieldConfirmations[field] = (privacyFieldConfirmations[field] ?? 0) + 1;
  }
  const keepFailure = privacyMutationFailureIsCurrent();
  if (!keepFailure) privacyMutationFailure = null;
  usePrivacyStore.setState({
    settings,
    loaded: current.loaded || hydrated,
    error: keepFailure ? current.error : null,
  });
}

function publishPrivacyMutationFailure(mutation: PrivacyMutation, message: string): void {
  const fields = ownedPrivacyMutationFields(mutation);
  if (fields.length === 0) return;
  privacyMutationFailure = { mutation, fields };
  usePrivacyStore.setState({ error: message });
}

const defaultSettings: PrivacySettings = {
  messagesFriendsOnly: true,
  messagesServerMembers: true,
  dmPrivacyLevel: 2,
  // New-account placeholder only. A server response without this field is
  // from an older version whose Friends-of-Friends default was off.
  dmFriendsOfFriends: true,
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
  // #1241: transient pre-fetch placeholder; server-confirmed settings replace it.
  // New accounts default to Mutual Servers in the control plane.
  allowFriendRequestsFrom: 'mutual_servers',
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
  pendingDmPrivacyLevel: DMPrivacyLevel | null;
  pendingFriendRequestMode: FriendRequestPrivacyMode | null;
  dmSaveError: string | null;
  friendRequestError: string | null;

  fetchPrivacy: () => Promise<void>;
  scheduleDMPrivacy: (level: DMPrivacyLevel) => void;
  scheduleFriendRequestPrivacy: (mode: FriendRequestPrivacyMode) => void;
  updatePrivacy: (
    updates: Partial<PrivacySettings>,
    credentials?: PrivacyStepUpCredentials,
    context?: ApiRequestContext
  ) => Promise<void>;
  /**
   * #2765: the gated OFF transition. Resolves with the classified outcome
   * instead of throwing, so the step-up dialog can place a rejected factor on
   * the field that owns it.
   */
  disablePurgeFence: (
    credentials: PrivacyStepUpCredentials,
    context?: ApiRequestContext
  ) => Promise<PurgeFenceDisableResult>;
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
        pendingDmPrivacyLevel: null,
        pendingFriendRequestMode: null,
        dmSaveError: null,
        friendRequestError: null,

        scheduleDMPrivacy: (level) => {
          if (dmPrivacyTimer !== null) clearTimeout(dmPrivacyTimer);
          const selection = {
            identity: privacyIdentity,
            context: captureApiRequestContext(),
            owner: { identity: privacyIdentity },
          };
          dmPrivacySelection = selection;
          privacyFieldOwners.dmPrivacyLevel = selection.owner;
          dmPrivacyTimer = setTimeout(() => {
            if (dmPrivacySelection !== selection || selection.identity !== privacyIdentity) return;
            dmPrivacyTimer = null;
            // Check before updatePrivacy issues a write ticket. A cancelled
            // selection must neither dispatch nor supersede a current write.
            if (!apiRequestContextIsCurrent(selection.context)) {
              dmPrivacySelection = null;
              set({ pendingDmPrivacyLevel: null });
              return;
            }
            void get()
              .updatePrivacy({ dmPrivacyLevel: level }, undefined, selection.context)
              .then(() => {
                if (dmPrivacySelection !== selection || selection.identity !== privacyIdentity)
                  return;
                dmPrivacySelection = null;
                set({ pendingDmPrivacyLevel: null });
              })
              .catch((error: unknown) => {
                if (dmPrivacySelection !== selection || selection.identity !== privacyIdentity)
                  return;
                dmPrivacySelection = null;
                const message = error instanceof Error ? error.message : GENERIC_UPDATE_FAILURE;
                set({
                  pendingDmPrivacyLevel: null,
                  dmSaveError: apiRequestContextIsCurrent(selection.context) ? message : null,
                });
              });
          }, 300);
          set({ pendingDmPrivacyLevel: level, dmSaveError: null });
        },

        scheduleFriendRequestPrivacy: (mode) => {
          if (friendRequestTimer !== null) clearTimeout(friendRequestTimer);
          const selection = {
            identity: privacyIdentity,
            context: captureApiRequestContext(),
            owner: { identity: privacyIdentity },
          };
          friendRequestSelection = selection;
          privacyFieldOwners.allowFriendRequestsFrom = selection.owner;
          friendRequestTimer = setTimeout(() => {
            if (friendRequestSelection !== selection || selection.identity !== privacyIdentity)
              return;
            friendRequestTimer = null;
            if (!apiRequestContextIsCurrent(selection.context)) {
              friendRequestSelection = null;
              set({ pendingFriendRequestMode: null });
              return;
            }
            void get()
              .updatePrivacy({ allowFriendRequestsFrom: mode }, undefined, selection.context)
              .then(() => {
                if (friendRequestSelection !== selection || selection.identity !== privacyIdentity)
                  return;
                friendRequestSelection = null;
                set({ pendingFriendRequestMode: null });
              })
              .catch((error: unknown) => {
                if (friendRequestSelection !== selection || selection.identity !== privacyIdentity)
                  return;
                friendRequestSelection = null;
                const message = error instanceof Error ? error.message : GENERIC_UPDATE_FAILURE;
                set({
                  pendingFriendRequestMode: null,
                  friendRequestError: apiRequestContextIsCurrent(selection.context)
                    ? message
                    : null,
                });
              });
          }, 300);
          set({ pendingFriendRequestMode: mode, friendRequestError: null });
        },

        fetchPrivacy: async () => {
          const ticket = beginPrivacyWrite();
          const confirmations = { ...privacyFieldConfirmations };
          const context = captureApiRequestContext();
          privacyMutationFailure = null;
          set({ isLoading: true, error: null });
          try {
            const response = await apiFetch('/api/v1/users/me/privacy');
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to load privacy settings');
            }
            const data = await response.json();
            const p = data.privacy;
            if (!apiRequestContextIsCurrent(context) || !maySettle(ticket)) return;
            const incoming: PrivacySettings = {
              messagesFriendsOnly: p.messages_friends_only ?? true,
              messagesServerMembers: p.messages_server_members ?? true,
              dmPrivacyLevel: (p.dm_privacy_level ?? 2) as DMPrivacyLevel,
              // Older control planes omit this field and default it off.
              // Current servers send the saved value, including the new on default.
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
              // #1241: an absent key is a pre-#1240 server with the older
              // Everyone default. New servers send their actual setting.
              allowFriendRequestsFrom: asFriendRequestMode(p.allow_friend_requests_from),
            };
            const protectedFields = PRIVACY_FIELDS.filter(
              (field) => confirmations[field] !== privacyFieldConfirmations[field]
            );
            set((state) => ({
              settings: mergePrivacyFields(incoming, state.settings, protectedFields),
              isLoading: false,
              // Only here. The catch below deliberately leaves it as-is: a
              // failed fetch means the settings on screen are still defaults.
              loaded: true,
            }));
          } catch (error) {
            if (!apiRequestContextIsCurrent(context) || !maySettle(ticket)) return;
            privacyMutationFailure = null;
            set({
              error: error instanceof Error ? error.message : 'Failed to load privacy settings',
              isLoading: false,
            });
          }
        },

        updatePrivacy: async (
          updates: Partial<PrivacySettings>,
          credentials?: PrivacyStepUpCredentials,
          context?: ApiRequestContext
        ) => {
          const mutation = beginPrivacyMutation(updates, context);
          const body: Record<string, boolean | number | string> = {};
          for (const [field, wireField] of Object.entries(PRIVACY_WIRE_FIELDS)) {
            const value = updates[field as keyof PrivacySettings];
            if (value !== undefined) body[wireField] = value;
          }
          // Factors remain request arguments, never state or persistence.
          if (credentials?.currentPassword) body.current_password = credentials.currentPassword;
          if (credentials?.mfaCode) body.mfa_code = credentials.mfaCode;

          try {
            const response = await apiFetchInContext(
              '/api/v1/users/me/privacy',
              {
                method: 'PATCH',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body),
              },
              context
            );
            if (!response.ok) {
              // Proxy HTML or an empty failure still uses the classified,
              // user-facing refusal rather than exposing a JSON SyntaxError.
              const data: PrivacyErrorBody = await response.json().catch(() => ({}));
              const refusal = classifyPrivacyRefusal(
                response.status,
                data,
                Object.keys(updates) as (keyof PrivacySettings)[]
              );
              throw new PrivacyUpdateError(refusal);
            }
            const data = await response.json();
            const p = data.privacy;
            const incoming: PrivacySettings = {
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
              // Missing on an older server: retain the fail-closed purge floor.
              requireAuthBeforePurge: p.require_auth_before_purge ?? true,
              allowFriendRequestsFrom: asFriendRequestMode(p.allow_friend_requests_from),
            };
            publishPrivacyMutation(mutation, incoming);
          } catch (error) {
            const message = error instanceof Error ? error.message : GENERIC_UPDATE_FAILURE;
            publishPrivacyMutationFailure(mutation, message);
            // Awaiting callers retain the original refusal/AbortError contract,
            // even when this operation no longer owns any visible state.
            throw error;
          }
        },

        disablePurgeFence: async (
          credentials: PrivacyStepUpCredentials,
          context?: ApiRequestContext
        ) => {
          try {
            await get().updatePrivacy({ requireAuthBeforePurge: false }, credentials, context);
            return { kind: 'accepted' };
          } catch (error) {
            if (error instanceof PrivacyUpdateError) return error.refusal;
            // apiFetch's pre-dispatch fence: nothing was sent, so nothing is
            // reported and the credentials are still the user's to send.
            if (isAbortError(error)) return { kind: 'aborted' };
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
          privacyFieldOwners = {};
          privacyFieldConfirmations = {};
          privacyMutationFailure = null;
          dmPrivacySelection = null;
          friendRequestSelection = null;
          if (dmPrivacyTimer !== null) clearTimeout(dmPrivacyTimer);
          if (friendRequestTimer !== null) clearTimeout(friendRequestTimer);
          dmPrivacyTimer = null;
          friendRequestTimer = null;
          set({
            settings: { ...defaultSettings },
            loaded: false,
            error: null,
            pendingDmPrivacyLevel: null,
            pendingFriendRequestMode: null,
            dmSaveError: null,
            friendRequestError: null,
          });
        },
      }),
      { name: 'PrivacyStore' }
    )
  )
);
