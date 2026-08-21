export interface Server {
  id: string;
  name: string;
  icon_url?: string;
  banner_url?: string;
  owner_id: string;
  server_tier?: 'groundspeed' | 'mach1' | 'mach2' | 'mach3' | 'selfhost'; // server-axis tier (ADR-0028 ladder); absent on stale cache
  allow_embedded_content: boolean;
  created_at: string;
  updated_at: string;
}

export interface ServerWithRole extends Server {
  role: 'owner' | 'admin' | 'member'; // Legacy — kept for backwards compat during transition
  member_count: number;
  online_count: number;
  permissions?: string; // Stringified bigint of computed server permissions
}

export interface Role {
  id: string;
  server_id: string;
  name: string;
  color?: string;
  emoji?: string;
  position: number;
  permissions: string; // Stringified bigint
  is_default: boolean;
  // `@all` is created is_default AND is_managed (internal/servers/handlers.go:191),
  // so every predicate that means "managed, therefore locked" must carry
  // `&& !is_default` — see utils/roleHierarchy.ts.
  is_managed: boolean;
  display_separately: boolean;
  mentionable: boolean;
  created_at: string;
  updated_at: string;
}

// --- Role hierarchy / reorder types (#2359) ---
//
// Spec: [internal]specs/2026-08-21-2359-role-reorder-ui-design.md.
// The payload invariant itself is documented once, in utils/roleHierarchy.ts,
// which owns the only constructor of RoleReorderPayload.

/**
 * The calling actor's own standing in a server's role hierarchy, published by
 * `GET /servers/{id}/roles` (`rbac.RoleListViewer`).
 *
 * Three states rather than `number | null`: the owner branch has to be visible
 * to the type system, and therefore to exhaustiveness checking and to the test
 * matrix. An invisible owner branch is the root cause of PR #2839 — the owner
 * bypasses the two guards that refuse every other actor, so every path
 * exercised during development passed.
 *
 * `unknown` is a real state, not a theoretical one: Concord is self-hostable,
 * and a control plane older than this change omits the block entirely. Fail
 * closed to read-only. NEVER guess a ceiling.
 *
 * The ceiling is advisory and may be stale. It may only SHRINK the draggable
 * band; it must never be used to permit anything, and a 403 remains a
 * first-class expected outcome.
 */
export type RoleViewer =
  { kind: 'owner' } | { kind: 'bounded'; maxRolePosition: number } | { kind: 'unknown' };

/**
 * The body of `PATCH /servers/{id}/roles/reorder`.
 *
 * The array is highest-authority-FIRST: the server assigns
 * `position = cardinality(role_ids) - ord + offset` (`ord` 1-based), so index 0
 * receives the highest position number.
 *
 * The brand exists because that direction COINCIDES with display order, and the
 * coincidence is what made #2839's docstring — the display order "is also the
 * exact role_ids payload shape" — plausible enough to ship. A `string[]` cannot
 * inhabit this type, so handing the display list to the store is a compile
 * error rather than a review question.
 *
 * `buildReorderPayload` (utils/roleHierarchy.ts) is the ONLY constructor. Build
 * it at Apply from the reconciled hierarchy and never store it: a payload that
 * cannot be held cannot go stale.
 *
 * `__brand` is a compile-time marker that happens to exist at runtime — send
 * `{ role_ids: payload.role_ids }`, not the object itself.
 */
export type RoleReorderPayload = {
  readonly role_ids: readonly string[]; // highest authority FIRST
  readonly __brand: 'RoleReorderPayload';
};

/**
 * The outcome of a reorder round-trip, replacing a `boolean` that collapsed
 * three distinct outcomes into one bit and discarded the server's actionable
 * 403 reason.
 *
 * `reconciled: false` means the PATCH landed but the refetch did not: the write
 * is durable and the view may be stale. Say exactly that; do not lie in either
 * direction.
 *
 * `throttled` is a separate variant on purpose. The endpoint is rate-limited
 * 5/min/user and Apply is a whole-band write, so a user correcting a mistake
 * reaches the limit in ordinary use. `denied` triggers a full revert of the
 * draft, so folding 429 into it would destroy a careful arrangement over a
 * transient throttle.
 */
export type ReorderOutcome =
  | { ok: true; reconciled: boolean }
  | { ok: false; kind: 'denied'; reason: string }
  | { ok: false; kind: 'throttled' }
  | { ok: false; kind: 'network' | 'unexpected' };

/**
 * The four disjoint groups a server's roles fall into for one viewer, each
 * sorted by `position` DESCENDING (highest authority first). Produced only by
 * `partitionRoleHierarchy`.
 *
 * Only `band` is draggable, and the payload projects EXACTLY `band`. Rendering
 * from a partition rather than a flat sorted list is what makes an order the
 * server would refuse unrepresentable, instead of a bug to be caught later.
 */
export type RoleHierarchy = {
  aboveCeiling: Role[];
  band: Role[];
  managed: Role[];
  pinned: Role[];
};

/**
 * Whether a reorder may be OFFERED at all, evaluated before any drag surface is
 * rendered. Every non-`ok` variant is a read-only mode with its own honest
 * notice — the point is to never hand the user a gesture that is guaranteed to
 * 403 or to do nothing.
 */
export type ReorderPrecondition =
  | { kind: 'ok' }
  | { kind: 'unknown-viewer' }
  | { kind: 'band-exceeds-ceiling' }
  | { kind: 'managed-gap' }
  | { kind: 'nothing-to-move' };

export interface MemberRoleInfo {
  role_id: string;
  role_name: string;
  role_color?: string;
  role_emoji?: string;
  position: number;
  display_separately?: boolean;
}

export interface CreateServerRequest {
  name: string;
  icon_url?: string;
}

export interface CreateServerResponse {
  server: Server;
  role: string;
}

export interface ListServersResponse {
  servers: ServerWithRole[];
}

// --- Invite Types ---

export interface ServerInvite {
  id: string;
  server_id: string;
  code: string;
  created_by: string;
  max_uses: number | null;
  use_count: number;
  expires_at: string | null;
  is_revoked: boolean;
  created_at: string;
}

export interface ServerInviteWithCreator extends ServerInvite {
  creator_username: string;
}

export interface CreateInviteRequest {
  max_uses?: number;
  expires_in?: number; // seconds
}

export interface JoinServerRequest {
  code: string;
}

export interface JoinServerResponse {
  server: Server;
  role: string;
}

export interface InviteInfoResponse {
  server_name: string;
  server_icon: string | null;
  server_banner: string | null;
  member_count: number;
  valid: boolean;
}
