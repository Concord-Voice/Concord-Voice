import React from 'react';
import { GripVertical, Lock } from 'lucide-react';
import type { Role } from '../../types/server';

/**
 * Everything a movable band row needs to participate in a drag or a keyboard
 * move. `null` on a row means the row is not movable, and — deliberately — that
 * no `dragover` handler is attached to it at all: a locked row must never call
 * `preventDefault()`, so the OS shows a no-drop cursor over it (spec §5.5).
 */
export interface RoleRowDragProps {
  /** Static handle label carrying "position N of M in the movable range". */
  label: string;
  onDragStart: (e: React.DragEvent<HTMLButtonElement>) => void;
  onDragEnd: () => void;
  onKeyDown: (e: React.KeyboardEvent<HTMLButtonElement>) => void;
  onDragOver: (e: React.DragEvent<HTMLLIElement>) => void;
  onDrop: (e: React.DragEvent<HTMLLIElement>) => void;
  handleRef: (el: HTMLButtonElement | null) => void;
}

export interface RoleHierarchyRowProps {
  role: Role;
  isSelected: boolean;
  onSelect: (roleId: string) => void;
  /**
   * Why this row cannot be moved. Rendered as a visually-hidden span referenced
   * by `aria-describedby` ON the select button — never on the glyph, which is
   * not focusable and would therefore never be announced.
   *
   * `null` renders an inert 24x24 spacer instead of the lock glyph: a row that
   * is simply the only movable role is not "locked", and claiming otherwise
   * would be a lie in the one presentation that has nothing to explain.
   */
  lockReason: string | null;
  /** Present only for a movable band row; `null` renders the lock slot. */
  drag: RoleRowDragProps | null;
  isDragging: boolean;
  rowRef?: (el: HTMLLIElement | null) => void;
}

/**
 * One row of the role hierarchy rail (#2359). Presentational only — props in,
 * DOM out, no store access and no drag state of its own.
 *
 * The `<li>` wrapper carries no handler, no `tabIndex` and no `role`; it exists
 * to put the select button and the handle/lock slot side by side. That split is
 * mandatory rather than cosmetic: the row used to be a `<button>`, and a nested
 * `<button>` handle inside it is invalid HTML and fails WCAG SC 4.1.2.
 */
const RoleHierarchyRow: React.FC<RoleHierarchyRowProps> = ({
  role,
  isSelected,
  onSelect,
  lockReason,
  drag,
  isDragging,
  rowRef,
}) => {
  const lockId = `role-lock-${role.id}`;

  return (
    <li
      ref={rowRef}
      className={`role-item${isDragging ? ' role-item--dragging' : ''}`}
      onDragOver={drag?.onDragOver}
      onDrop={drag?.onDrop}
    >
      <button
        type="button"
        className={`role-select-btn${isSelected ? ' selected' : ''}`}
        aria-current={isSelected ? 'true' : undefined}
        aria-describedby={lockReason ? lockId : undefined}
        onClick={() => onSelect(role.id)}
      >
        <span className="role-color-dot" style={{ backgroundColor: role.color || '#99aab5' }} />
        <span className="role-item-name" style={role.color ? { color: role.color } : undefined}>
          {role.name}
        </span>
      </button>

      {drag ? (
        <button
          type="button"
          ref={drag.handleRef}
          className="role-drag-handle"
          aria-label={drag.label}
          draggable
          onDragStart={drag.onDragStart}
          onDragEnd={drag.onDragEnd}
          onKeyDown={drag.onKeyDown}
        >
          <GripVertical size={12} aria-hidden="true" />
        </button>
      ) : (
        <span className="role-lock-slot">
          {lockReason !== null && (
            <>
              <Lock size={12} aria-hidden="true" />
              <span id={lockId} className="sr-only">
                {lockReason}
              </span>
            </>
          )}
        </span>
      )}
    </li>
  );
};

export default RoleHierarchyRow;
