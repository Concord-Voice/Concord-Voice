import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import RoleHierarchyRow from './RoleHierarchyRow';
import { usePermissionStore } from '../../stores/permissionStore';
import { decideDropIndex } from '../../utils/decideDropIndex';
import {
  buildReorderPayload,
  evaluateReorderPrecondition,
  moveWithinBand,
  partitionRoleHierarchy,
} from '../../utils/roleHierarchy';
import { errorMessage } from '../../utils/redactError';
import type { ReorderPrecondition, Role, RoleViewer } from '../../types/server';

/**
 * The role hierarchy rail (#2359, spec §5.5). Renders all four hierarchy groups
 * top-to-bottom, highest authority first, and makes exactly the `band` group
 * movable through three mechanisms — drag, keyboard, and a single Move up/down
 * toolbar pair acting on the selected role.
 *
 * Nothing in this rail can express an order the server would refuse: the payload
 * projects exactly the band (see `utils/roleHierarchy.ts`, which owns the
 * contract), so error handling only ever has to explain staleness.
 */

/** Fail-closed viewer for a server whose roles have not been fetched yet.
 *  Module-level so the selector returns a stable reference. */
const UNKNOWN_VIEWER: RoleViewer = { kind: 'unknown' };

/** Stable empty list for the pre-fetch read inside the Apply reconcile step. */
const EMPTY_ROLES: readonly Role[] = [];

/** Matches `.role-hierarchy { gap: 4px }` in ServerSettingsPage.css — the <ul>
 *  that actually holds the rows, NOT the `.roles-list` wrapper around it. Both
 *  are 4px today, which is exactly why naming the wrong one would go stale
 *  invisibly the moment someone changes one of them.
 *
 *  This is the gutter `decideDropIndex` clamps against so a pointer landing
 *  between two rows still resolves to an insertion point. */
const ROW_GAP_PX = 4;

const DT_ROLE = 'application/concord-role';

/** One lock string per locked group, plus the mode-level one for band rows that
 *  a read-only mode has locked. */
const LOCK_ABOVE_CEILING = 'Ranked at or above your own highest role — you cannot move it.';
const LOCK_MANAGED = 'Managed by an integration and cannot be moved.';
const LOCK_PINNED = 'The default role always stays last.';
const LOCK_MODE_READ_ONLY = 'Reordering is unavailable on this server.';

const BOUNDARY_COPY = 'Your role — everything above this line is locked';

/**
 * How the rail presents itself. Every non-interactive mode renders every group
 * locked, with no handles, no toolbar and no Apply bar.
 */
type RailMode =
  'interactive' | 'unknown-viewer' | 'band-exceeds-ceiling' | 'managed-gap' | 'nothing-to-move';

/** Three honest notices, one per read-only condition. `nothing-to-move` renders
 *  silently — nothing to reorder is not a failure state. */
const MODE_NOTICE: Record<RailMode, string | null> = {
  interactive: null,
  'unknown-viewer':
    'Reordering is unavailable on this server — your own place in the role hierarchy could not be determined. The list below is read-only.',
  'band-exceeds-ceiling':
    'There are more movable roles than your own position allows, so no order you could choose would be accepted. The list below is read-only.',
  'managed-gap':
    'This server has roles managed by an integration. They cannot be moved, and reordering the rest around them would corrupt the hierarchy, so the list below is read-only.',
  'nothing-to-move': null,
};

/**
 * Convert an INSERTION index (what `decideDropIndex` returns: the slot the row
 * lands *before*) into a TARGET index (what `moveWithinBand` takes: the index
 * the moved id occupies in the returned array).
 *
 * Removing the dragged row shifts every later slot down by one, so any
 * insertion point past its current index is one too high. The keyboard and
 * toolbar paths are plain `from ± 1` TARGET indices and must NOT be routed
 * through this.
 *
 * It lives in one named, exported helper rather than inline at the drop site
 * because this off-by-one is the single most likely defect in the whole rail.
 */
export function insertionToTargetIndex(from: number, insertionIndex: number): number {
  return insertionIndex > from ? insertionIndex - 1 : insertionIndex;
}

/**
 * Map a reorder precondition onto the rail's presentation mode.
 *
 * One correction lives here. `evaluateReorderPrecondition` reports
 * `band-exceeds-ceiling` for an EMPTY band whenever the ceiling is 0, because
 * `0 > 0 - 1` holds — reachable for a bounded viewer holding nothing above the
 * default role. The accurate answer there is `nothing-to-move`, and showing
 * "more movable roles than your position allows" for a band of zero roles would
 * be actively misleading.
 *
 * Corrected here rather than in the pure module, because that module is shared
 * contract asserted against the cross-language fixture; which *presentation* an
 * edge case earns is this component's job.
 */
function resolveRailMode(precondition: ReorderPrecondition, bandSize: number): RailMode {
  if (precondition.kind === 'band-exceeds-ceiling' && bandSize === 0) {
    return 'nothing-to-move';
  }
  if (precondition.kind === 'ok') {
    return 'interactive';
  }
  return precondition.kind;
}

/** Whether two id lists are permutations of one another. */
function isSameIdSet(a: readonly string[], b: readonly string[]): boolean {
  if (a.length !== b.length) return false;
  const seen = new Set(b);
  return a.every((id) => seen.has(id));
}

export interface RoleHierarchyListProps {
  serverId: string;
  roles: Role[];
  selectedRoleId: string | null;
  onSelectRole: (roleId: string) => void;
  onCreateRole: () => void;
}

const RoleHierarchyList: React.FC<RoleHierarchyListProps> = ({
  serverId,
  roles,
  selectedRoleId,
  onSelectRole,
  onCreateRole,
}) => {
  const viewer = usePermissionStore((s) => s.roleViewer[serverId] ?? UNKNOWN_VIEWER);
  const reorderRoles = usePermissionStore((s) => s.reorderRoles);
  const fetchRoles = usePermissionStore((s) => s.fetchRoles);

  /**
   * The in-progress order of the band, or `null` when the rail is showing the
   * server's own order. Component state on purpose (spec §5.5): it is ephemeral,
   * single-consumer, never persisted, and never survives unmount, so none of the
   * reasons `[internal]rules/frontend.md` §State Management gives for a store apply.
   */
  const [draftOrder, setDraftOrder] = useState<string[] | null>(null);
  const [draggingId, setDraggingId] = useState<string | null>(null);
  const [dropIndex, setDropIndex] = useState<number | null>(null);
  const [announcement, setAnnouncement] = useState('');
  const [alertText, setAlertText] = useState<string | null>(null);
  const [isApplying, setIsApplying] = useState(false);

  const rowElementsRef = useRef(new Map<string, HTMLLIElement>());
  const handleElementsRef = useRef(new Map<string, HTMLButtonElement>());

  const hierarchy = useMemo(() => partitionRoleHierarchy(roles, viewer), [roles, viewer]);
  const precondition = evaluateReorderPrecondition(hierarchy, viewer);
  const bandIds = useMemo(() => hierarchy.band.map((role) => role.id), [hierarchy]);

  // See resolveRailMode for the empty-band-at-ceiling-0 correction.
  const mode: RailMode = resolveRailMode(precondition, hierarchy.band.length);

  const isInteractive = mode === 'interactive';

  // Band rows in a read-only mode are locked by the MODE, not by their own
  // group, so they carry the mode-level reason — except in `nothing-to-move`,
  // where nothing is locked and there is nothing to explain. Hoisted out of the
  // row JSX so the render has no nested conditional.
  const readOnlyBandLockReason = mode === 'nothing-to-move' ? null : LOCK_MODE_READ_ONLY;

  // A draft only survives while it is still a permutation of the live band: a
  // concurrent admin's change (or this user's own refetch) can retire a role
  // mid-arrangement, and an order naming a role that no longer exists is not an
  // order this rail may render or send.
  const activeDraft = draftOrder !== null && isSameIdSet(draftOrder, bandIds) ? draftOrder : null;
  const orderedBandIds = activeDraft ?? bandIds;
  const isDirty = activeDraft !== null;

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: discards a draft the live band has invalidated so it cannot resurrect if the band later returns to the same id set; the render above already ignores it, this is cleanup, not derived state
    if (draftOrder !== null && !isSameIdSet(draftOrder, bandIds)) setDraftOrder(null);
  }, [draftOrder, bandIds]);

  const bandById = useMemo(
    () => new Map(hierarchy.band.map((role) => [role.id, role])),
    [hierarchy]
  );

  const orderedBand = useMemo(
    () =>
      orderedBandIds
        .map((id) => bandById.get(id))
        .filter((role): role is Role => role !== undefined),
    [orderedBandIds, bandById]
  );

  const registerRow = useCallback((id: string, el: HTMLLIElement | null) => {
    if (el) rowElementsRef.current.set(id, el);
    else rowElementsRef.current.delete(id);
  }, []);

  const registerHandle = useCallback((id: string, el: HTMLButtonElement | null) => {
    if (el) handleElementsRef.current.set(id, el);
    else handleElementsRef.current.delete(id);
  }, []);

  /**
   * Apply one move to the draft. `targetIndex` is always a TARGET index — the
   * index the role occupies afterwards — so a drop must convert first via
   * `insertionToTargetIndex`.
   *
   * A move that changes nothing (an `Alt+Arrow` at a band edge, a drop back onto
   * the same slot) returns without touching state, so it can never mount the
   * Apply bar on a no-op.
   */
  const commitMove = useCallback(
    (roleId: string, targetIndex: number) => {
      if (isApplying) return;
      const from = orderedBandIds.indexOf(roleId);
      if (from === -1) return;

      const next = moveWithinBand(orderedBandIds, from, targetIndex);
      const to = next.indexOf(roleId);
      if (to === from) return;

      const nameOf = (id: string) => bandById.get(id)?.name ?? 'another role';
      // Neighbour-relative, not ordinal: the rail is not numbered on screen, so
      // "position 4 of 9" states something the user cannot check. The ordinal
      // lives on the handle's own label, read on focus.
      const neighbour = to < from ? next[to + 1] : next[to - 1];
      const direction = to < from ? 'above' : 'below';

      setDraftOrder(next);
      setAlertText(null);
      setAnnouncement(`${nameOf(roleId)} moved ${direction} ${nameOf(neighbour)}`);
    },
    [bandById, isApplying, orderedBandIds]
  );

  // ── Toolbar (WCAG 2.2 SC 2.5.7 single-pointer path) ──

  const selectedBandIndex = selectedRoleId === null ? -1 : orderedBandIds.indexOf(selectedRoleId);

  const moveSelected = useCallback(
    (delta: -1 | 1) => {
      if (selectedRoleId === null || selectedBandIndex === -1) return;
      // Plain `from ± 1`: already a TARGET index, no insertion conversion.
      commitMove(selectedRoleId, selectedBandIndex + delta);
    },
    [commitMove, selectedBandIndex, selectedRoleId]
  );

  // ── Keyboard ──

  const handleHandleKeyDown = useCallback(
    (e: React.KeyboardEvent<HTMLButtonElement>, roleId: string) => {
      if (e.key !== 'ArrowUp' && e.key !== 'ArrowDown') return;
      const from = orderedBandIds.indexOf(roleId);
      if (from === -1) return;
      e.preventDefault();

      const delta = e.key === 'ArrowDown' ? 1 : -1;
      if (e.altKey) {
        // Plain `from ± 1`: already a TARGET index. `moveWithinBand` clamps, so
        // both band edges are a no-op rather than a special case here.
        commitMove(roleId, from + delta);
        return;
      }

      // Unmodified arrows rove focus. Clamped, not wrapped, to match the edges
      // of the move itself. Escape is deliberately unbound — the settings
      // overlay reserves it.
      const neighbourId = orderedBandIds[from + delta];
      if (neighbourId !== undefined) handleElementsRef.current.get(neighbourId)?.focus();
    },
    [commitMove, orderedBandIds]
  );

  // ── Drag ──

  const handleDragStart = useCallback(
    (e: React.DragEvent<HTMLButtonElement>, roleId: string) => {
      if (isApplying) {
        e.preventDefault();
        return;
      }
      e.dataTransfer.setData(DT_ROLE, roleId);
      e.dataTransfer.effectAllowed = 'move';
      setDraggingId(roleId);
    },
    [isApplying]
  );

  const handleDragEnd = useCallback(() => {
    setDraggingId(null);
    setDropIndex(null);
  }, []);

  /** Rects of the band rows in visual order, or `null` if any row is unmounted —
   *  a partial list would silently misalign every index. */
  const collectBandRects = useCallback((): DOMRect[] | null => {
    const rects: DOMRect[] = [];
    for (const id of orderedBandIds) {
      const el = rowElementsRef.current.get(id);
      if (!el) return null;
      rects.push(el.getBoundingClientRect());
    }
    return rects;
  }, [orderedBandIds]);

  const handleRowDragOver = useCallback(
    (e: React.DragEvent<HTMLLIElement>) => {
      // Not our drag: no `preventDefault()`, so the OS shows no-drop. Locked rows
      // never reach this handler at all — they are not given one.
      if (draggingId === null) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';

      const rects = collectBandRects();
      if (rects === null) return;
      setDropIndex(decideDropIndex(e.clientY, rects, ROW_GAP_PX));
    },
    [collectBandRects, draggingId]
  );

  const handleRowDrop = useCallback(
    (e: React.DragEvent<HTMLLIElement>) => {
      e.preventDefault();
      const roleId = draggingId;
      const insertionIndex = dropIndex;
      setDraggingId(null);
      setDropIndex(null);
      if (roleId === null || insertionIndex === null) return;

      const from = orderedBandIds.indexOf(roleId);
      if (from === -1) return;
      // THE conversion: insertion index -> target index. Nowhere else.
      commitMove(roleId, insertionToTargetIndex(from, insertionIndex));
    },
    [commitMove, draggingId, dropIndex, orderedBandIds]
  );

  // ── Apply / Discard ──

  const handleDiscard = useCallback(() => {
    setDraftOrder(null);
    setAlertText(null);
    setAnnouncement('Role order changes discarded.');
  }, []);

  const handleApply = useCallback(async () => {
    if (activeDraft === null || isApplying) return;
    setIsApplying(true);
    try {
      // RECONCILE BEFORE BUILDING. The payload must be the COMPLETE band as the
      // server currently sees it, and this render's `hierarchy` is only as fresh
      // as the last fetch. A band missing a role yields a payload that omits it,
      // and `applyRolePositions` renumbers only the ids it is given — leaving the
      // omitted role where it sits and committing DUPLICATE POSITIONS with HTTP
      // 200 and no error anywhere. That is the exact corruption this rail's
      // branded payload exists to make unreachable, arriving through a stale
      // premise rather than a bad construction.
      //
      // The role_created / role_deleted WS handlers narrow this window; only a
      // read immediately before the write closes it to the round-trip itself.
      const refreshed = await fetchRoles(serverId);
      if (!refreshed) {
        setAlertText(
          'Could not refresh the role list, so the new order was not applied. Your order is still here — try applying again.'
        );
        return;
      }

      const { serverRoles, roleViewer } = usePermissionStore.getState();
      const liveHierarchy = partitionRoleHierarchy(
        serverRoles[serverId] ?? EMPTY_ROLES,
        roleViewer[serverId] ?? UNKNOWN_VIEWER
      );
      const liveBandIds = liveHierarchy.band.map((role) => role.id);

      // The draft is a permutation of the band it was arranged against. If the
      // live band is a different SET, that arrangement no longer describes
      // anything the server would accept — discard it rather than send a
      // partial order built from a world that has moved.
      if (!isSameIdSet(activeDraft, liveBandIds)) {
        setDraftOrder(null);
        setAlertText(
          'The roles on this server changed while you were reordering, so your changes were discarded. The list below is up to date.'
        );
        return;
      }

      const payload = buildReorderPayload(liveHierarchy, activeDraft);
      const outcome = await reorderRoles(serverId, payload);

      if (outcome.ok) {
        setDraftOrder(null);
        if (outcome.reconciled) {
          setAlertText(null);
          setAnnouncement('Role order saved.');
          return;
        }
        // The write landed and the refetch did not. Say exactly that.
        setAlertText(
          'Role order saved, but the list could not be refreshed — what you see below may be out of date until you reopen this page.'
        );
        return;
      }

      if (outcome.kind === 'denied') {
        // A 403 under a correct band proves the world changed, so the on-screen
        // order is built on a stale role set. Revert and refetch rather than
        // invite the user to nudge a fantasy.
        setDraftOrder(null);
        setAlertText(
          `${outcome.reason} Your changes were discarded and the role list has been refreshed.`
        );
        void fetchRoles(serverId);
        return;
      }

      // Everything below preserves the draft: the arrangement is still good, only
      // the round-trip failed.
      if (outcome.kind === 'throttled') {
        setAlertText(
          'Too many role changes in a short time. Wait a moment and apply again — your order is still here.'
        );
        return;
      }
      setAlertText(
        outcome.kind === 'network'
          ? 'Could not reach the server. Your order is still here — try applying again.'
          : 'Something went wrong saving the new order. Your order is still here — try applying again.'
      );
    } catch (err) {
      // Unreachable behind the permutation guard on `activeDraft`; surfaced
      // rather than left as a silent rejection.
      console.error('Role reorder payload rejected:', errorMessage(err));
      setDraftOrder(null);
      setAlertText('Could not build the new role order. Reopen this page and try again.');
    } finally {
      setIsApplying(false);
    }
  }, [activeDraft, fetchRoles, isApplying, reorderRoles, serverId]);

  // ── Render ──

  const noticeText = roles.length === 0 ? null : MODE_NOTICE[mode];

  // Bounded viewers only, and only when something is actually above them.
  const showBoundary = viewer.kind === 'bounded' && hierarchy.aboveCeiling.length > 0;

  const renderLockedRow = (role: Role, lockReason: string | null) => (
    <RoleHierarchyRow
      key={role.id}
      role={role}
      isSelected={selectedRoleId === role.id}
      onSelect={onSelectRole}
      lockReason={lockReason}
      drag={null}
      isDragging={false}
    />
  );

  const dragGhost = <li className="role-drag-ghost" aria-hidden="true" />;

  return (
    <div className="roles-list">
      {alertText !== null && (
        <div className="role-reorder-alert" role="alert">
          {alertText}
        </div>
      )}

      {noticeText !== null && (
        <div className="role-reorder-notice" role="note">
          {noticeText}
        </div>
      )}

      {isInteractive && (
        <div className="role-reorder-toolbar">
          <button
            type="button"
            className="role-reorder-move-btn"
            onClick={() => moveSelected(-1)}
            disabled={isApplying || selectedBandIndex <= 0}
          >
            Move up
          </button>
          <button
            type="button"
            className="role-reorder-move-btn"
            onClick={() => moveSelected(1)}
            disabled={
              isApplying ||
              selectedBandIndex === -1 ||
              selectedBandIndex >= orderedBandIds.length - 1
            }
          >
            Move down
          </button>
        </div>
      )}

      {roles.length > 0 && (
        <ul className="role-hierarchy" aria-label="Roles, highest rank first">
          {/*
            Mode-aware on purpose. `isAboveCeiling` fails closed to `true` for an
            unknown viewer, so EVERY role lands in `aboveCeiling` in that mode —
            but "ranked at or above your own highest role" is a claim we cannot
            support there, because an unknown viewer is precisely one whose rank
            we could not determine. Saying it anyway would explain a lock with a
            fact we do not have.
          */}
          {hierarchy.aboveCeiling.map((role) =>
            renderLockedRow(
              role,
              mode === 'unknown-viewer' ? LOCK_MODE_READ_ONLY : LOCK_ABOVE_CEILING
            )
          )}

          {showBoundary && (
            <li className="role-reorder-boundary">
              <span className="role-reorder-boundary-chip">{BOUNDARY_COPY}</span>
            </li>
          )}

          {orderedBand.map((role, index) => (
            <React.Fragment key={role.id}>
              {dropIndex === index && dragGhost}
              {isInteractive ? (
                <RoleHierarchyRow
                  role={role}
                  isSelected={selectedRoleId === role.id}
                  onSelect={onSelectRole}
                  lockReason={null}
                  isDragging={draggingId === role.id}
                  rowRef={(el) => registerRow(role.id, el)}
                  drag={{
                    label: `Reorder ${role.name}, position ${index + 1} of ${orderedBand.length} in the movable range`,
                    handleRef: (el) => registerHandle(role.id, el),
                    onDragStart: (e) => handleDragStart(e, role.id),
                    onDragEnd: handleDragEnd,
                    onKeyDown: (e) => handleHandleKeyDown(e, role.id),
                    onDragOver: handleRowDragOver,
                    onDrop: handleRowDrop,
                  }}
                />
              ) : (
                renderLockedRow(role, readOnlyBandLockReason)
              )}
            </React.Fragment>
          ))}
          {dropIndex === orderedBand.length && dragGhost}

          {hierarchy.managed.map((role) => renderLockedRow(role, LOCK_MANAGED))}
          {hierarchy.pinned.map((role) => renderLockedRow(role, LOCK_PINNED))}
        </ul>
      )}

      <button className="create-role-btn" onClick={onCreateRole}>
        + Create Role
      </button>

      {isDirty && (
        <div className="role-reorder-apply-bar">
          <button
            type="button"
            className="role-reorder-discard-btn"
            onClick={handleDiscard}
            disabled={isApplying}
          >
            Discard
          </button>
          <button
            type="button"
            className="role-reorder-apply-btn"
            onClick={() => void handleApply()}
            disabled={isApplying}
          >
            {isApplying ? 'Applying...' : 'Apply Order'}
          </button>
        </div>
      )}

      <div className="sr-only" role="status" aria-live="polite">
        {announcement}
      </div>
    </div>
  );
};

export default RoleHierarchyList;
