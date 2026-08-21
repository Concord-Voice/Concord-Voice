/**
 * Pure role-hierarchy model for the role reorder rail (#2359). No DOM, no store
 * imports, no React — callable from a bare unit test.
 *
 * This module is the CUSTODY of the reorder payload contract. The contract
 * lives in a pure module behind a branded constructor rather than in a
 * docstring, because prose cannot be type-checked and PR #2839 was killed by a
 * docstring that was simply wrong.
 *
 * THE INVARIANT (spec §2.1):
 *
 *   The payload is EXACTLY the set of roles whose current position is strictly
 *   below the actor's ceiling, minus the default role, minus non-default
 *   managed roles — in the chosen order, highest authority first.
 *   NOTHING MORE, NOTHING LESS.
 *
 * Both halves are load-bearing, and each is a distinct nearly-shipped bug:
 *
 * - NOTHING MORE. #2839 sent the whole display list. That is a loud 403 for
 *   every non-owner (guard 4: no named role at `position >= actorMaxPosition`).
 *   The owner short-circuits guards 4 and 5, so every path exercised during
 *   development passed.
 *
 * - NOTHING LESS. The obvious correction — sending only the roles the user
 *   dragged — is worse. `applyRolePositions` renumbers ONLY the named ids
 *   (`internal/rbac/handlers.go`); omitted roles keep their existing positions.
 *   Proven by an executed probe against real PostgreSQL:
 *
 *       BEFORE: @all=0  A=1  B=2  C=3  D=4
 *       subset reorder [D, C]  ->  HTTP 200
 *       AFTER : @all=0  A=1  B=2  C=1  D=2
 *       COLLISION at position 2: [B D]   COLLISION at position 1: [A C]
 *
 *   `RowsAffected == len(role_ids)`, so the server's own consistency guard
 *   passes and the transaction commits. 200 OK, corrupted hierarchy, no error
 *   anywhere. Over-sending fails loudly; under-sending fails SILENTLY.
 *
 * Server contract read from `services/control-plane/internal/rbac/handlers.go`
 * (`evaluateReorderGuards`, `applyRolePositions`).
 */

import type {
  ReorderPrecondition,
  Role,
  RoleHierarchy,
  RoleReorderPayload,
  RoleViewer,
} from '../types/server';

/**
 * The default role (`@all`). Always rendered last, never draggable, and never
 * named in the payload — omitting it is what makes the server reserve
 * position 0 for it (`offset = 1`) instead of renumbering it into the band.
 */
function isPinnedRole(role: Role): boolean {
  return role.is_default;
}

/**
 * A managed role that is NOT the default role — refused by the server for
 * everyone, owner included.
 *
 * `&& !role.is_default` is MANDATORY, not defensive: `@all` is created
 * `is_default = TRUE, is_managed = TRUE` (`internal/servers/handlers.go:191`),
 * so without the clause the default role lands in the wrong group and the rail
 * stops pinning it last. This mirrors the server's own write-side filter
 * (`internal/rbac/handlers.go`: `NOT (is_managed AND NOT is_default)`).
 */
function isManagedLockedRole(role: Role): boolean {
  return role.is_managed && !role.is_default;
}

/**
 * Whether a role sits at or above the viewer's ceiling and is therefore
 * untouchable by them.
 *
 * Exhaustive switch with no `default` on purpose — a fourth viewer state must
 * be a compile error here, not a silent fall-through to "draggable".
 */
function isAboveCeiling(role: Role, viewer: RoleViewer): boolean {
  switch (viewer.kind) {
    case 'owner':
      // Guards 4 and 5 are bypassed for the owner, so nothing is above them.
      return false;
    case 'bounded':
      // Guard 4 refuses any named role at `position >= actorMaxPosition`.
      return role.position >= viewer.maxRolePosition;
    case 'unknown':
      // Fail closed. With no ceiling we cannot say which roles are safe to
      // name, so every role is locked and the band is empty — the payload
      // constructor then cannot produce anything at all.
      return true;
  }
}

/** Highest authority first. `Array.prototype.sort` is stable, so roles sharing
 * a position (only reachable through the server-side subset-collision defect)
 * keep their incoming relative order rather than flickering. */
function byPositionDescending(a: Role, b: Role): number {
  return b.position - a.position;
}

/**
 * Split a server's roles into the four disjoint groups the rail renders, from
 * the viewer's point of view. Does not mutate `roles`.
 *
 * Order of the tests is load-bearing: pinned wins over managed (see
 * `isManagedLockedRole`), and both win over the ceiling test, because a locked
 * role's position is irrelevant once it can never be named.
 */
export function partitionRoleHierarchy(roles: readonly Role[], viewer: RoleViewer): RoleHierarchy {
  const aboveCeiling: Role[] = [];
  const band: Role[] = [];
  const managed: Role[] = [];
  const pinned: Role[] = [];

  for (const role of roles) {
    if (isPinnedRole(role)) {
      pinned.push(role);
    } else if (isManagedLockedRole(role)) {
      managed.push(role);
    } else if (isAboveCeiling(role, viewer)) {
      aboveCeiling.push(role);
    } else {
      band.push(role);
    }
  }

  // Sorted as statements rather than inline in the return: `Array.sort` mutates
  // AND returns, so using it as an expression reads as if it were pure. These
  // four arrays are local accumulators, so mutating them is safe — but the
  // expression form is the habit that makes it unsafe somewhere else later.
  // (`toSorted` would be the direct fix; it is ES2023 and this project targets
  // ES2022.)
  aboveCeiling.sort(byPositionDescending);
  band.sort(byPositionDescending);
  managed.sort(byPositionDescending);
  pinned.sort(byPositionDescending);

  return { aboveCeiling, band, managed, pinned };
}

/**
 * Whether a reorder may be offered at all. Evaluated before any drag surface
 * renders, so the UI can go read-only with an honest reason instead of handing
 * the user a gesture that is guaranteed to fail.
 *
 * The order of the tests is the reporting priority, not an accident.
 */
export function evaluateReorderPrecondition(
  hierarchy: RoleHierarchy,
  viewer: RoleViewer
): ReorderPrecondition {
  // No ceiling means no safe band. Never guess one.
  if (viewer.kind === 'unknown') {
    return { kind: 'unknown-viewer' };
  }

  // PRECEDENCE: this check sits ABOVE the managed-role one deliberately, and
  // the two can be true at once. When they are, this is the honest notice,
  // because it is the condition under which NO constructible order succeeds —
  // the band already excludes managed roles, so removing them would not help,
  // and telling the user about integration roles would point them at something
  // that is not the binding constraint. Pinned by scenario s5 of the shared
  // contract fixture (testdata/role_reorder_contract.json).
  //
  // Guard 5 inverted: the server refuses when
  // `len(role_ids) - 1 + offset >= actorMaxPosition`, and `offset` is always 1
  // here because we never name the default role. So a band larger than
  // `ceiling - 1` has NO order the actor can construct that is not a 403 —
  // read-only is the only honest surface.
  //
  // The identity `len(band) <= ceiling - 1` holds automatically under dense,
  // unique positions, which is exactly why it will never be hit in
  // development. It is emergent, not a law: break density once and this fires.
  if (viewer.kind === 'bounded' && hierarchy.band.length > viewer.maxRolePosition - 1) {
    return { kind: 'band-exceeds-ceiling' };
  }

  // Conservative, and today unreachable in production: `@all` is the only
  // `is_managed` row any production path creates, so this group is always
  // empty until bots/integrations land (#247). It matters because an omitted
  // managed role KEEPS its position while the band is renumbered around it,
  // which is the silent-collision shape above. Refuse rather than collide.
  if (hierarchy.managed.length > 0) {
    return { kind: 'managed-gap' };
  }

  // Not a failure state — nothing to reorder renders silently.
  if (hierarchy.band.length < 2) {
    return { kind: 'nothing-to-move' };
  }

  return { kind: 'ok' };
}

/**
 * Move one band entry, returning a NEW array. Never mutates `bandIds`.
 *
 * `to` is the index the moved id occupies in the RETURNED array — not an
 * insertion index into the input. That makes the two keyboard mechanisms plain
 * arithmetic (`Alt+↑` is `to = from - 1`, `Alt+↓` is `to = from + 1`, the
 * toolbar pair likewise), and both clamp to a no-op at the band edges, which is
 * exactly the specified edge behaviour rather than a special case.
 *
 * A drop index from `decideDropIndex` is an INSERTION index into the current
 * array and converts as `to = dropIndex > from ? dropIndex - 1 : dropIndex`.
 *
 * An out-of-range `from` returns an unchanged copy: there is no entry to move.
 */
export function moveWithinBand(bandIds: readonly string[], from: number, to: number): string[] {
  const next = [...bandIds];
  if (!Number.isInteger(from) || from < 0 || from >= next.length) {
    return next;
  }

  const target = Math.min(Math.max(Math.trunc(to), 0), next.length - 1);
  if (target === from) {
    return next;
  }

  const [moved] = next.splice(from, 1);
  next.splice(target, 0, moved);
  return next;
}

/**
 * The ONLY constructor of `RoleReorderPayload`. Projects exactly the band, in
 * `draftOrder` order, highest authority first.
 *
 * Call it on the Apply click from the RECONCILED hierarchy and never store the
 * result. A payload that cannot be held cannot go stale — #2839's "Apply sends
 * a stale pendingOrder" is unrepresentable rather than fixed.
 *
 * `@all` is never named, so the server always takes the `offset = 1` path and
 * reserves position 0 for it.
 *
 * Throws when `draftOrder` is not a permutation of the band, and when the band
 * is empty. Both states are unreachable behind `evaluateReorderPrecondition`,
 * so throwing is correct: it signals a bug in the caller, not user error, and
 * an empty `role_ids` is refused by the server's binding (`min=1`) anyway.
 * The message carries counts only — role ids are not put in an error string.
 */
export function buildReorderPayload(
  hierarchy: RoleHierarchy,
  draftOrder: readonly string[]
): RoleReorderPayload {
  const bandIds = new Set(hierarchy.band.map((role) => role.id));

  if (bandIds.size === 0) {
    throw new Error('buildReorderPayload: refusing to build an empty payload from an empty band');
  }

  const seen = new Set<string>();
  for (const id of draftOrder) {
    if (!bandIds.has(id) || seen.has(id)) {
      throw new Error(
        `buildReorderPayload: draftOrder is not a permutation of the band ` +
          `(band=${bandIds.size}, draft=${draftOrder.length})`
      );
    }
    seen.add(id);
  }
  if (seen.size !== bandIds.size) {
    throw new Error(
      `buildReorderPayload: draftOrder is not a permutation of the band ` +
        `(band=${bandIds.size}, draft=${draftOrder.length})`
    );
  }

  return { role_ids: [...draftOrder], __brand: 'RoleReorderPayload' };
}
