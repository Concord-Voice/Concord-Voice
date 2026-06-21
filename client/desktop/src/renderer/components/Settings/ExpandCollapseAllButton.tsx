import React, { type RefObject } from 'react';
import { useExpandCollapseAll } from '../../hooks/useExpandCollapseAll';

interface ExpandCollapseAllButtonProps {
  /**
   * Ref to the container that holds the `<details.settings-collapsible>`
   * sections. The button reads aggregate state from and writes bulk actions
   * to every matching section under this container.
   */
  containerRef: RefObject<HTMLElement | null>;
}

/**
 * Top-right header control on the active settings panel (closes #297).
 *
 * Renders nothing when the active panel has no collapsible sections (e.g.,
 * a future flat-layout tab) so the chrome stays clean. Label flips between
 * "Expand all" and "Collapse all" based on aggregate state: only shows
 * "Collapse all" when every section is currently expanded — mixed and
 * fully-collapsed states both show "Expand all" per the #297 default.
 *
 * The chevron mirrors the label direction (down for expand, up for
 * collapse), matching the chevron rotation on individual section headers
 * for visual consistency.
 */
const ExpandCollapseAllButton: React.FC<ExpandCollapseAllButtonProps> = ({ containerRef }) => {
  const { allExpanded, hasSections, toggle } = useExpandCollapseAll(containerRef);

  if (!hasSections) return null;

  const label = allExpanded ? 'Collapse all' : 'Expand all';

  return (
    <button
      type="button"
      className="settings-expand-collapse-all-btn"
      onClick={toggle}
      aria-label={label}
    >
      <span>{label}</span>
      <svg
        className="settings-expand-collapse-all-chevron"
        width="14"
        height="14"
        viewBox="0 0 16 16"
        fill="none"
        aria-hidden="true"
        data-expanded={allExpanded ? 'true' : 'false'}
      >
        <path
          d="M4 6l4 4 4-4"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
    </button>
  );
};

export default ExpandCollapseAllButton;
