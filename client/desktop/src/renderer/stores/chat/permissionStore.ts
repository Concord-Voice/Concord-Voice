/**
 * Permission Store — manages RBAC roles, effective permissions, and channel overrides.
 * Integrates with the backend RBAC/SBAC system.
 */

import type { StoreApi } from 'zustand';
import { createStore } from '../../utils/runtime/createStore';
import { apiFetch } from '../../services/system/apiClient';
import {
  apiFetchInContext,
  captureApiRequestContext,
  isAbortError,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { useChannelStore } from './channelStore';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import {
  Role,
  type ReorderOutcome,
  type RoleReorderPayload,
  type RoleViewer,
} from '../../types/server';
import { hasPermission, parseEffectivePermissions } from '../../utils/policy/permissions';

export interface ChannelOverride {
  id: string;
  channel_id: string;
  target_type: 'user' | 'role';
  target_id: string;
  allow: string;
  deny: string;
  created_at: string;
  updated_at: string;
}

export interface UpsertOverrideRequest {
  target_type: 'user' | 'role';
  target_id: string;
  allow: string;
  deny: string;
}

export interface CreateRoleRequest {
  name: string;
  color?: string;
  permissions?: string;
}

export type UpdateRoleRequest = Partial<{
  name: string;
  color: string;
  emoji: string;
  permissions: string;
  display_separately: boolean;
  mentionable: boolean;
}>;

/**
 * What a step-up re-send of a role or override write adds to its first send
 * (#3456 §3.3): the code the dialog proved and the account and server it opened
 * for. The first send has neither.
 */
export interface WriteConfirmation {
  /** Sent as `mfa_code`. Absent when the dialog sends with no factor (the server decides). */
  readonly mfaCode: string | undefined;
  /** The request refuses to dispatch once the account or server is no longer this one. */
  readonly context: ApiRequestContext;
}

/**
 * Why a role or override write did not land, in the shape of `ReorderOutcome`
 * (#3406): `ok` first, then a `kind`.
 *
 * - `refused`: the server answered non-2xx. `body` is its JSON, or null when it
 *   was not JSON. The host reads it (`adaptDangerousActionRefusal`) to decide
 *   whether the answer asks for verification. `context` is the account and
 *   server the request went out as: a refusal that opens the dialog hands it
 *   on as the dialog's `capture`, so the re-send is admitted against the
 *   session the server refused and never against a successor (C82).
 * - `aborted`: nothing was sent, because the account or server changed first.
 * - `network`: the outcome is unknown here: the request failed in transit, or
 *   the account changed while it was out, so this view applied nothing.
 */
export type PermissionWriteFailure =
  | { ok: false; kind: 'refused'; status: number; body: unknown; context: ApiRequestContext }
  | { ok: false; kind: 'aborted' | 'network' };

export type PermissionWriteOutcome = { ok: true } | PermissionWriteFailure;

export type RoleCreateOutcome = { ok: true; role: Role } | PermissionWriteFailure;

/** Shown when a 403 carries no readable `error` string — never a re-worded server reason. */
const REORDER_DENIED_FALLBACK = 'You cannot reorder these roles.';

/** Upper bound on the server-supplied denial text rendered in the reorder banner. */
const MAX_DENIAL_REASON_CHARS = 200;

/**
 * Read the self-scoped `viewer` block from GET /servers/{id}/roles.
 *
 * Absent, malformed, or a non-integer ceiling all collapse to `unknown`, which
 * drives the reorder UI read-only. A control plane older than the `viewer` block
 * is a real self-hosted deployment, so the ceiling is NEVER guessed or derived —
 * guessing a ceiling is what shipped the previous attempt's owner-only bug.
 *
 * Wire is snake_case (`max_role_position`); the client model is camelCase.
 */
function parseRoleViewer(raw: unknown): RoleViewer {
  if (typeof raw !== 'object' || raw === null) return { kind: 'unknown' };
  const viewer = raw as { kind?: unknown; max_role_position?: unknown };
  if (viewer.kind === 'owner') return { kind: 'owner' };
  if (
    viewer.kind === 'bounded' &&
    typeof viewer.max_role_position === 'number' &&
    Number.isInteger(viewer.max_role_position) &&
    // `>= 0` is explicit rather than emergent. A negative ceiling already fails
    // closed — every role reads as above it, the band empties, and the rail goes
    // read-only — but that outcome falls out of downstream arithmetic rather
    // than being stated here, so a later change to `isAboveCeiling` could
    // silently turn it into an open failure. Positions are non-negative by
    // construction; say so at the boundary.
    viewer.max_role_position >= 0
  ) {
    return { kind: 'bounded', maxRolePosition: viewer.max_role_position };
  }
  return { kind: 'unknown' };
}

/**
 * Carry the server's actionable denial text verbatim — the reorder guards return
 * distinct, user-facing strings ("Cannot reorder roles at or above your own
 * position" vs "Reorder would create roles at or above your position") and the
 * banner renders whichever one came back. Body parsing has its own guard so a
 * non-JSON 403 degrades to the fallback instead of surfacing as a network error.
 */
async function readDenialReason(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown };
    if (typeof body.error === 'string' && body.error.trim() !== '') {
      // Bounded because this string is SERVER-CONTROLLED and is rendered into the
      // banner verbatim. React escapes it, so there is no injection path; the
      // residual is a hostile or misconfigured self-hosted control plane emitting
      // a multi-kilobyte or misleading banner. The real guard reasons are all
      // short static literals, so this truncation can only ever fire on a
      // response the genuine server would not send.
      return body.error.slice(0, MAX_DENIAL_REASON_CHARS);
    }
  } catch {
    // Non-JSON or truncated body — fall through to the fallback.
  }
  return REORDER_DENIED_FALLBACK;
}

/** No permission write is in flight for a scope. One shared instance, so a
 *  selector falling back to it returns a stable reference. */
export const NO_PERMISSION_WRITES: readonly number[] = [];

let nextPermissionWriteId = 0;

/**
 * Record a permission write in `permissionWritesInFlight[scopeKey]` from the
 * moment it is called until its request settles (#3406 review, round 6). The
 * record is taken before the first await, so a caller that renders in the same
 * tick already sees it, and it is dropped only on settlement, success or not.
 */
async function trackPermissionWrite<T>(
  set: StoreApi<PermissionState>['setState'],
  scopeKey: string,
  write: () => Promise<T>
): Promise<T> {
  const id = ++nextPermissionWriteId;
  set((state) => ({
    permissionWritesInFlight: {
      ...state.permissionWritesInFlight,
      [scopeKey]: [...(state.permissionWritesInFlight[scopeKey] ?? []), id],
    },
  }));
  try {
    return await write();
  } finally {
    set((state) => {
      const next = { ...state.permissionWritesInFlight };
      const remaining = (next[scopeKey] ?? []).filter((w) => w !== id);
      if (remaining.length > 0) next[scopeKey] = remaining;
      else delete next[scopeKey];
      return { permissionWritesInFlight: next };
    });
  }
}

/**
 * Read sequencing per scope (#3406 review, round 9). A settings modal reads its
 * overrides when it opens, and a save made before that read answers refetches
 * the list; the older response then landed on top of the fresh one, so an added
 * override vanished or a saved mask reverted, and saving the stale row again
 * erased the bits just written. Role writes, which patch the list locally, and
 * `roles_reordered` refetches have the same race.
 *
 * A read takes a ticket when it starts. A write that has CONFIRMED raises the
 * scope's floor to a fresh ticket, so every read begun before that point is
 * known to be older than the write. Keys match `channelOverrides` (a channel id
 * or `category:<id>`) and `rolesScope` for roles. Tickets only ever grow, so
 * correctness never needs a sequence reset: an account change is fenced by the
 * auth lifecycle before a read ever reaches `settleRead`. `reset()` still drops
 * the `permissions:` scopes, because those are keyed by server and channel id
 * and would otherwise outlive the account that read them.
 */
interface ReadSequence {
  next: number;
  floor: number;
  committed: number;
}

const readSequences = new Map<string, ReadSequence>();

/** Prefix of the effective-permission scopes, so `reset()` can drop exactly those. */
const PERMISSIONS_SCOPE_PREFIX = 'permissions:';

function clearPermissionReadSequences(): void {
  for (const scopeKey of readSequences.keys()) {
    if (scopeKey.startsWith(PERMISSIONS_SCOPE_PREFIX)) readSequences.delete(scopeKey);
  }
}

function readSequence(scopeKey: string): ReadSequence {
  let sequence = readSequences.get(scopeKey);
  if (sequence === undefined) {
    sequence = { next: 0, floor: 0, committed: 0 };
    readSequences.set(scopeKey, sequence);
  }
  return sequence;
}

function beginRead(scopeKey: string): number {
  return ++readSequence(scopeKey).next;
}

function markWriteConfirmed(scopeKey: string): void {
  const sequence = readSequence(scopeKey);
  sequence.floor = ++sequence.next;
}

/**
 * What a read that succeeded does with its result:
 * - `commit`: it began after the scope's last confirmed write and after every
 *   read already committed, so it is the freshest view there is.
 * - `stale`: it began before a confirmed write, and no read begun since that
 *   write has landed. Its data predates the write, and the view holds only the
 *   write's local patch, so the caller reads again.
 * - `skip`: the view already holds a read begun after the last confirmed write
 *   and after this one, so it is at least as fresh as this result.
 */
function settleRead(scopeKey: string, ticket: number): 'commit' | 'stale' | 'skip' {
  const sequence = readSequence(scopeKey);
  if (ticket > sequence.floor && ticket > sequence.committed) {
    sequence.committed = ticket;
    return 'commit';
  }
  if (ticket <= sequence.floor && sequence.committed <= sequence.floor) return 'stale';
  return 'skip';
}

/** The read-sequence key of a server's role list, apart from override scopes. */
function rolesScope(serverId: string): string {
  return `roles:${serverId}`;
}

const WRITE_ABORTED: PermissionWriteFailure = { ok: false, kind: 'aborted' };
/**
 * A write whose outcome this view cannot state. Also what a host reads a
 * rejected write as: the store's own actions never reject, but a prop a host is
 * handed might.
 */
export const WRITE_UNKNOWN: PermissionWriteFailure = { ok: false, kind: 'network' };

type WriteSent = { ok: true; response: Response } | PermissionWriteFailure;

/**
 * Sends one role or override write. The first send carries no `confirmation`
 * and is the host's request as it always was; a step-up re-send adds `mfa_code`
 * to the same body and is admitted against the dialog's account and server
 * (#3456 §3.3). A refusal is returned with its status and body rather than
 * worded here: only the host knows whether it asks for verification.
 *
 * Both sends go out against one captured context, which the refusal carries:
 * the first send's own, captured here, or the re-send's, which is the first
 * send's handed back through the dialog. A host never has to capture after
 * the answer lands, when the account or server may already be another (C82).
 */
async function sendWrite(
  path: string,
  method: 'POST' | 'PATCH' | 'PUT' | 'DELETE',
  body: object | undefined,
  confirmation: WriteConfirmation | undefined
): Promise<WriteSent> {
  const code = confirmation?.mfaCode;
  const sent = code === undefined ? body : { ...body, mfa_code: code };
  const init: RequestInit =
    sent === undefined
      ? { method }
      : { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sent) };
  const context = confirmation?.context ?? captureApiRequestContext();
  try {
    const response = await apiFetchInContext(path, init, context);
    if (response.ok) return { ok: true, response };
    return {
      ok: false,
      kind: 'refused',
      status: response.status,
      body: await response.json().catch(() => null),
      context,
    };
  } catch (err) {
    return isAbortError(err) ? WRITE_ABORTED : WRITE_UNKNOWN;
  }
}

/**
 * The read-sequence keys of the two effective-permission reads (#3456, F10).
 * Prefixed, because `channelOverrides` already keys its own sequence on a bare
 * channel id and the two must not share a ticket counter.
 */
function serverPermissionsScope(serverId: string): string {
  return `${PERMISSIONS_SCOPE_PREFIX}server:${serverId}`;
}

function channelPermissionsScope(channelId: string): string {
  return `${PERMISSIONS_SCOPE_PREFIX}channel:${channelId}`;
}

/** Set or clear one server's `mfa_restricted` flag. The flag is stored only when true. */
function withMfaRestricted(
  current: Record<string, true>,
  serverId: string,
  restricted: boolean
): Record<string, true> {
  const { [serverId]: _previous, ...rest } = current;
  return restricted ? { ...rest, [serverId]: true } : rest;
}

/**
 * The store update that drops one override from a scope's list, keyed like
 * channelOverrides. Module-level so the delete actions, which run inside
 * trackPermissionWrite, do not nest a fifth function.
 */
function withoutOverride(
  state: PermissionState,
  scopeKey: string,
  overrideId: string
): Pick<PermissionState, 'channelOverrides'> {
  return {
    channelOverrides: {
      ...state.channelOverrides,
      [scopeKey]: (state.channelOverrides[scopeKey] ?? []).filter((o) => o.id !== overrideId),
    },
  };
}

interface PermissionState {
  // Server roles keyed by server ID
  serverRoles: Record<string, Role[]>;
  // Reorder ceiling of the current user per server, from the roles `viewer` block
  roleViewer: Record<string, RoleViewer>;
  // User's effective permissions per server (BigInt as string for storage)
  serverPermissions: Record<string, bigint>;
  // User's effective permissions per channel
  channelPermissions: Record<string, bigint>;
  // Servers whose last GET /servers/:id/permissions carried `mfa_restricted: true`
  // (#3453/#3456): the viewer's dangerous bits are masked there until they enrol
  // a second factor. Stored only when true, never persisted and never logged, and
  // known only for servers MainView has made active (the route is not fanned out).
  mfaRestrictedByServer: Record<string, true>;
  // Channel overrides keyed by channel ID
  channelOverrides: Record<string, ChannelOverride[]>;
  // Override writes and category syncs whose request has not settled, keyed
  // like channelOverrides: a channel id, or `category:<id>`. A sync is recorded
  // under its channel's key, since it replaces that channel's overrides. The
  // settings modals unmount when closed, which destroys their own write locks
  // while the request can still commit, so a reopened modal reads this record
  // to stay locked until that request settles (#3406 review, round 6).
  permissionWritesInFlight: Record<string, readonly number[]>;

  // --- Permission checks ---
  hasServerPermission: (serverId: string, perm: bigint) => boolean;

  /** Drop every field: all of it is the signed-in account's view (#3406). */
  reset: () => void;

  // --- Role management ---
  fetchRoles: (serverId: string) => Promise<boolean>;
  // The three role writes and the two override upserts answer with a result
  // union, not a bare boolean, so a host can tell a refusal that asks for
  // verification from one that does not (#3456). `confirmation` is set only on
  // the step-up dialog's re-send.
  createRole: (
    serverId: string,
    data: CreateRoleRequest,
    confirmation?: WriteConfirmation
  ) => Promise<RoleCreateOutcome>;
  updateRole: (
    serverId: string,
    roleId: string,
    data: UpdateRoleRequest,
    confirmation?: WriteConfirmation
  ) => Promise<PermissionWriteOutcome>;
  deleteRole: (
    serverId: string,
    roleId: string,
    confirmation?: WriteConfirmation
  ) => Promise<PermissionWriteOutcome>;
  reorderRoles: (serverId: string, payload: RoleReorderPayload) => Promise<ReorderOutcome>;
  assignRole: (serverId: string, userId: string, roleId: string) => Promise<boolean>;
  unassignRole: (serverId: string, userId: string, roleId: string) => Promise<boolean>;

  // --- Server permissions ---
  fetchServerPermissions: (serverId: string) => Promise<void>;
  fetchChannelPermissions: (channelId: string) => Promise<void>;
  /**
   * Drop cached effective permissions for these channels AND discard any read of
   * them already in flight, so a response that began before the eviction cannot
   * put a pre-change answer back (#3456).
   */
  evictChannelPermissions: (channelIds: readonly string[]) => void;
  /**
   * The same for servers: drops the cached effective permissions AND the
   * `mfa_restricted` flag, and discards any read in flight. For servers the
   * viewer is not looking at, whose answer an account-wide change has outdated;
   * the next activation re-reads (#3456).
   */
  evictServerPermissions: (serverIds: readonly string[]) => void;

  // --- Channel overrides (SBAC) ---
  fetchChannelOverrides: (channelId: string) => Promise<void>;
  upsertChannelOverride: (
    channelId: string,
    data: UpsertOverrideRequest,
    confirmation?: WriteConfirmation
  ) => Promise<PermissionWriteOutcome>;
  deleteChannelOverride: (channelId: string, overrideId: string) => Promise<boolean>;

  // --- Category overrides ---
  fetchCategoryOverrides: (categoryId: string) => Promise<void>;
  upsertCategoryOverride: (
    categoryId: string,
    data: UpsertOverrideRequest,
    confirmation?: WriteConfirmation
  ) => Promise<PermissionWriteOutcome>;
  deleteCategoryOverride: (categoryId: string, overrideId: string) => Promise<boolean>;

  // --- Category sync ---
  setCategorySync: (channelId: string, sync: boolean) => Promise<boolean>;
}

export const usePermissionStore = createStore<PermissionState>()((set, get) => ({
  serverRoles: {},
  roleViewer: {},
  serverPermissions: {},
  channelPermissions: {},
  mfaRestrictedByServer: {},
  channelOverrides: {},
  permissionWritesInFlight: {},

  // An account change must not carry the previous account's roles, effective
  // permissions or overrides, nor its writes in flight: a reopened settings
  // modal would treat those as inherited and lock until they settle (#3406
  // review, round 7). A write that settles after this only removes its own id.
  // Reset cannot cancel a request already dispatched, so every action below
  // captures the account before its request and re-checks it before any write
  // that follows an await (round 8): a continuation from the previous account
  // writes nothing.
  reset: () => {
    clearPermissionReadSequences();
    set({
      serverRoles: {},
      roleViewer: {},
      serverPermissions: {},
      channelPermissions: {},
      mfaRestrictedByServer: {},
      channelOverrides: {},
      permissionWritesInFlight: {},
    });
  },

  hasServerPermission: (serverId: string, perm: bigint): boolean => {
    const perms = get().serverPermissions[serverId];
    if (perms === undefined) return false;
    return hasPermission(perms, perm);
  },

  // ─── Role Management ──────────────────────────────────────────────

  // Returns whether THIS call refreshed the view. Callers that need to know
  // whether their own read landed (reorderRoles reporting `reconciled`) must use
  // this return value, NOT an observation of shared state: a per-server revision
  // counter is satisfied by ANY concurrent successful fetch, so a fetch that
  // started before a write and landed after it would mark a failed refetch as
  // reconciled while the view still showed pre-write data. That is one-sided in
  // the unsafe direction, and `roles_reordered` makes concurrent fetches routine.
  // Existing callers that ignore the value keep the previous swallow behaviour.
  //
  // A read begun before a confirmed role write re-reads rather than landing on
  // top of it (round 9); a read overtaken by a newer one reports true, since the
  // view already holds data at least as new as this call's start.
  fetchRoles: async (serverId: string): Promise<boolean> => {
    const lifecycle = captureAuthLifecycle();
    const ticket = beginRead(rolesScope(serverId));
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles`);
      if (!res.ok) return false;
      const data = await res.json();
      if (!isSameAuthLifecycle(lifecycle)) return false;
      const verdict = settleRead(rolesScope(serverId), ticket);
      if (verdict === 'stale') return await get().fetchRoles(serverId);
      if (verdict === 'skip') return true;
      set((state) => ({
        serverRoles: { ...state.serverRoles, [serverId]: data.roles ?? [] },
        roleViewer: { ...state.roleViewer, [serverId]: parseRoleViewer(data.viewer) },
      }));
      return true;
    } catch {
      // Network error — leave existing state
      return false;
    }
  },

  createRole: async (serverId, data, confirmation) => {
    const lifecycle = captureAuthLifecycle();
    const sent = await sendWrite(`/api/v1/servers/${serverId}/roles`, 'POST', data, confirmation);
    if (!sent.ok) return sent;
    try {
      const json = await sent.response.json();
      const role = json.role as Role;
      if (!isSameAuthLifecycle(lifecycle)) return WRITE_UNKNOWN;
      markWriteConfirmed(rolesScope(serverId));
      // Add to local state
      set((state) => ({
        serverRoles: {
          ...state.serverRoles,
          [serverId]: [...(state.serverRoles[serverId] ?? []), role].sort(
            (a, b) => b.position - a.position
          ),
        },
      }));
      return { ok: true, role };
    } catch {
      return WRITE_UNKNOWN;
    }
  },

  updateRole: async (serverId, roleId, data, confirmation) => {
    const lifecycle = captureAuthLifecycle();
    const sent = await sendWrite(
      `/api/v1/servers/${serverId}/roles/${roleId}`,
      'PATCH',
      data,
      confirmation
    );
    if (!sent.ok) return sent;
    try {
      const json = await sent.response.json();
      const serverRole = json.role as Role | undefined;
      if (!isSameAuthLifecycle(lifecycle)) return WRITE_UNKNOWN;
      markWriteConfirmed(rolesScope(serverId));
      set((state) => ({
        serverRoles: {
          ...state.serverRoles,
          [serverId]: (state.serverRoles[serverId] ?? []).map((r) => {
            if (r.id !== roleId) return r;
            // Prefer server-returned role; fall back to optimistic merge from sent data
            if (serverRole) return serverRole;
            return {
              ...r,
              ...(data.name !== undefined && { name: data.name }),
              ...(data.color !== undefined && { color: data.color }),
              ...(data.emoji !== undefined && { emoji: data.emoji }),
              ...(data.permissions !== undefined && { permissions: data.permissions }),
              ...(data.display_separately !== undefined && {
                display_separately: data.display_separately,
              }),
              ...(data.mentionable !== undefined && { mentionable: data.mentionable }),
            };
          }),
        },
      }));
      return { ok: true };
    } catch {
      return WRITE_UNKNOWN;
    }
  },

  deleteRole: async (serverId, roleId, confirmation) => {
    const lifecycle = captureAuthLifecycle();
    const sent = await sendWrite(
      `/api/v1/servers/${serverId}/roles/${roleId}`,
      'DELETE',
      undefined,
      confirmation
    );
    if (!sent.ok) return sent;
    if (!isSameAuthLifecycle(lifecycle)) return WRITE_UNKNOWN;
    markWriteConfirmed(rolesScope(serverId));
    set((state) => ({
      serverRoles: {
        ...state.serverRoles,
        [serverId]: (state.serverRoles[serverId] ?? []).filter((r) => r.id !== roleId),
      },
    }));
    return { ok: true };
  },

  reorderRoles: async (serverId: string, payload: RoleReorderPayload): Promise<ReorderOutcome> => {
    const lifecycle = captureAuthLifecycle();
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles/reorder`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        // The brand is a compile-time custody guard, never part of the wire body.
        body: JSON.stringify({ role_ids: payload.role_ids }),
      });

      if (res.ok) {
        // The write committed either way, but after an account change the
        // re-read would run for the new account: report the view unreconciled.
        if (!isSameAuthLifecycle(lifecycle)) return { ok: true, reconciled: false };
        markWriteConfirmed(rolesScope(serverId));
        // Refetch to pick up the new positions. `reconciled` reports whether THIS
        // read landed: the write is committed either way, but the view may be
        // stale, and the UI says so rather than lying in either direction.
        const reconciled = await get().fetchRoles(serverId);
        return { ok: true, reconciled };
      }

      if (res.status === 403) {
        return { ok: false, kind: 'denied', reason: await readDenialReason(res) };
      }

      // 429 is deliberately NOT folded into `denied`. Apply is a whole-band write
      // against a 5/min/user limit, so a user correcting a mistake reaches it in
      // ordinary use — and `denied` discards the draft, destroying that work over a
      // transient throttle.
      if (res.status === 429) return { ok: false, kind: 'throttled' };

      return { ok: false, kind: 'unexpected' };
    } catch {
      return { ok: false, kind: 'network' };
    }
  },

  assignRole: async (serverId: string, userId: string, roleId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/members/${userId}/roles`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role_id: roleId }),
      });
      return res.ok;
    } catch {
      return false;
    }
  },

  unassignRole: async (serverId: string, userId: string, roleId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/members/${userId}/roles/${roleId}`, {
        method: 'DELETE',
      });
      return res.ok;
    } catch {
      return false;
    }
  },

  // ─── Server Permissions ────────────────────────────────────────────

  // Both effective-permission reads are fenced twice after their await (#3456,
  // F10): by account, because `reset()` cannot cancel a request already sent, and
  // by read order per key, so an older response landing after a newer one cannot
  // put an older answer back. The permission-change events make overlapping reads
  // routine: the event's refetch races the one MainView or MessageInput issued on
  // navigation. A read that loses on either count writes nothing.
  fetchServerPermissions: async (serverId: string) => {
    const lifecycle = captureAuthLifecycle();
    const scope = serverPermissionsScope(serverId);
    const ticket = beginRead(scope);
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/permissions`);
      if (!res.ok) return;
      const data = await res.json();
      if (!isSameAuthLifecycle(lifecycle)) return;
      if (settleRead(scope, ticket) !== 'commit') return;
      set((state) => ({
        serverPermissions: {
          ...state.serverPermissions,
          [serverId]: parseEffectivePermissions(data.permissions),
        },
        mfaRestrictedByServer: withMfaRestricted(
          state.mfaRestrictedByServer,
          serverId,
          data.mfa_restricted === true
        ),
      }));
    } catch {
      // Network error
    }
  },

  fetchChannelPermissions: async (channelId: string) => {
    const lifecycle = captureAuthLifecycle();
    const scope = channelPermissionsScope(channelId);
    const ticket = beginRead(scope);
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/permissions`);
      if (!res.ok) return;
      const data = await res.json();
      if (!isSameAuthLifecycle(lifecycle)) return;
      if (settleRead(scope, ticket) !== 'commit') return;
      set((state) => ({
        channelPermissions: {
          ...state.channelPermissions,
          [channelId]: parseEffectivePermissions(data.permissions),
        },
      }));
    } catch {
      // Network error
    }
  },

  evictChannelPermissions: (channelIds: readonly string[]) => {
    // Raising the floor makes every read begun before now `stale`, and these
    // reads hold no local patch to reconcile, so a stale one simply writes
    // nothing. A read begun after this lands normally.
    for (const channelId of channelIds) markWriteConfirmed(channelPermissionsScope(channelId));
    set((state) => {
      const cached = channelIds.filter((channelId) => channelId in state.channelPermissions);
      if (cached.length === 0) return state;
      const next = { ...state.channelPermissions };
      for (const channelId of cached) delete next[channelId];
      return { channelPermissions: next };
    });
  },

  evictServerPermissions: (serverIds: readonly string[]) => {
    // As for channels: the raised floor makes any read begun before now write
    // nothing, and the entry and its `mfa_restricted` flag go together.
    for (const serverId of serverIds) markWriteConfirmed(serverPermissionsScope(serverId));
    set((state) => {
      const cached = serverIds.filter(
        (serverId) => serverId in state.serverPermissions || serverId in state.mfaRestrictedByServer
      );
      if (cached.length === 0) return state;
      const serverPermissions = { ...state.serverPermissions };
      const mfaRestrictedByServer = { ...state.mfaRestrictedByServer };
      for (const serverId of cached) {
        delete serverPermissions[serverId];
        delete mfaRestrictedByServer[serverId];
      }
      return { serverPermissions, mfaRestrictedByServer };
    });
  },

  // ─── Channel Overrides (SBAC) ──────────────────────────────────────

  fetchChannelOverrides: async (channelId: string) => {
    const lifecycle = captureAuthLifecycle();
    const ticket = beginRead(channelId);
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/overrides`);
      if (!res.ok) return;
      const data = await res.json();
      if (!isSameAuthLifecycle(lifecycle)) return;
      const verdict = settleRead(channelId, ticket);
      if (verdict === 'stale') await get().fetchChannelOverrides(channelId);
      if (verdict !== 'commit') return;
      set((state) => ({
        channelOverrides: {
          ...state.channelOverrides,
          [channelId]: data.overrides ?? [],
        },
      }));
    } catch {
      // Network error
    }
  },

  upsertChannelOverride: (channelId, data, confirmation) =>
    trackPermissionWrite(set, channelId, async () => {
      const lifecycle = captureAuthLifecycle();
      const sent = await sendWrite(
        `/api/v1/channels/${channelId}/overrides`,
        'PUT',
        data,
        confirmation
      );
      if (!sent.ok) return sent;
      if (!isSameAuthLifecycle(lifecycle)) return WRITE_UNKNOWN;
      markWriteConfirmed(channelId);
      // Refetch to get updated list
      await get().fetchChannelOverrides(channelId);
      return { ok: true } as const;
    }),

  deleteChannelOverride: (channelId: string, overrideId: string) =>
    trackPermissionWrite(set, channelId, async () => {
      const lifecycle = captureAuthLifecycle();
      try {
        const res = await apiFetch(`/api/v1/channels/${channelId}/overrides/${overrideId}`, {
          method: 'DELETE',
        });
        if (!res.ok || !isSameAuthLifecycle(lifecycle)) return false;
        markWriteConfirmed(channelId);
        set((state) => withoutOverride(state, channelId, overrideId));
        return true;
      } catch {
        return false;
      }
    }),

  // ─── Category Overrides ────────────────────────────────────────────

  fetchCategoryOverrides: async (categoryId: string) => {
    const lifecycle = captureAuthLifecycle();
    const ticket = beginRead(`category:${categoryId}`);
    try {
      const res = await apiFetch(`/api/v1/categories/${categoryId}/overrides`);
      if (!res.ok) return;
      const data = await res.json();
      if (!isSameAuthLifecycle(lifecycle)) return;
      const verdict = settleRead(`category:${categoryId}`, ticket);
      if (verdict === 'stale') await get().fetchCategoryOverrides(categoryId);
      if (verdict !== 'commit') return;
      set((state) => ({
        channelOverrides: {
          ...state.channelOverrides,
          [`category:${categoryId}`]: data.overrides ?? [],
        },
      }));
    } catch {
      // Network error
    }
  },

  upsertCategoryOverride: (categoryId, data, confirmation) =>
    trackPermissionWrite(set, `category:${categoryId}`, async () => {
      const lifecycle = captureAuthLifecycle();
      const sent = await sendWrite(
        `/api/v1/categories/${categoryId}/overrides`,
        'PUT',
        data,
        confirmation
      );
      if (!sent.ok) return sent;
      if (!isSameAuthLifecycle(lifecycle)) return WRITE_UNKNOWN;
      markWriteConfirmed(`category:${categoryId}`);
      await get().fetchCategoryOverrides(categoryId);
      return { ok: true } as const;
    }),

  deleteCategoryOverride: (categoryId: string, overrideId: string) =>
    trackPermissionWrite(set, `category:${categoryId}`, async () => {
      const lifecycle = captureAuthLifecycle();
      try {
        const res = await apiFetch(`/api/v1/categories/${categoryId}/overrides/${overrideId}`, {
          method: 'DELETE',
        });
        if (!res.ok || !isSameAuthLifecycle(lifecycle)) return false;
        markWriteConfirmed(`category:${categoryId}`);
        set((state) => withoutOverride(state, `category:${categoryId}`, overrideId));
        return true;
      } catch {
        return false;
      }
    }),

  // ─── Category Sync ─────────────────────────────────────────────────

  // The result is recorded here, not by the modal that asked: that modal may
  // have been closed before the request settled, the server sends no channel
  // update for a sync, and a reopened modal starts from a snapshot of the
  // channel (#3406 review, round 8). Turning sync on also replaces the
  // channel's overrides, so they are re-read in the same write.
  setCategorySync: (channelId: string, sync: boolean) =>
    trackPermissionWrite(set, channelId, async () => {
      const lifecycle = captureAuthLifecycle();
      try {
        const res = await apiFetch(`/api/v1/channels/${channelId}/permission-sync`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ sync_permissions: sync }),
        });
        if (!res.ok || !isSameAuthLifecycle(lifecycle)) return false;
        useChannelStore.getState().updateChannel(channelId, { sync_permissions: sync });
        if (sync) {
          markWriteConfirmed(channelId);
          await get().fetchChannelOverrides(channelId);
        }
        return true;
      } catch {
        return false;
      }
    }),
}));
