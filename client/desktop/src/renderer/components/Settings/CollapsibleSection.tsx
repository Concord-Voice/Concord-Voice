import React from 'react';
import { useSettingsCollapsibleStore } from '../../stores/ui/settingsCollapsibleStore';

interface CollapsibleSectionProps {
  id: string;
  title: string;
  children: React.ReactNode;
  /** Called after this section closes, by click, Collapse all or a script write (#3635). */
  onCollapse?: () => void;
}

/**
 * A Settings section with a native `<details>`/`<summary>` disclosure. Collapsed by default,
 * with no per-site override (#2365).
 *
 * Open state is remembered for the renderer session in `useSettingsCollapsibleStore`, keyed
 * by `id`, through a two-way binding: `open` renders from the store and `onToggle` writes the
 * DOM truth back. The native `toggle` event fires for script-driven `el.open = …` writes as
 * well as clicks, so Expand/Collapse all, the sub-nav scroll, a focus request and the update
 * indicator reach the store with no store call of their own. React rewrites the `open`
 * attribute only when the subscribed value changes, so it never stomps a DOM change the store
 * has not recorded yet.
 *
 * `onToggle` bubbles in React, so a nested `<details>` (the font-area accordion, the About
 * legal text) invokes this handler too; the same-target guard skips it, and the handler reads
 * `currentTarget.open` — never `target.open`, which would be the inner element's state, and
 * never `newState`, which jsdom does not supply. `onCollapse` (#3635) fires after the store
 * write, so a callback that throws cannot leave the memory stale.
 */
const CollapsibleSection: React.FC<CollapsibleSectionProps> = ({
  id,
  title,
  children,
  onCollapse,
}) => {
  const open = useSettingsCollapsibleStore((s) => s.openSections[id] === true);
  const setSectionOpen = useSettingsCollapsibleStore((s) => s.setSectionOpen);

  const handleToggle = (event: React.SyntheticEvent<HTMLDetailsElement>) => {
    if (event.target !== event.currentTarget) return;
    const isOpen = event.currentTarget.open;
    setSectionOpen(id, isOpen);
    if (!isOpen) onCollapse?.();
  };

  return (
    <details
      className="settings-section settings-collapsible"
      id={id}
      open={open}
      onToggle={handleToggle}
    >
      <summary className="settings-collapsible-header">
        <h2 className="settings-section-title">{title}</h2>
        <svg
          className="settings-collapsible-chevron"
          width="16"
          height="16"
          viewBox="0 0 16 16"
          fill="none"
        >
          <path
            d="M4 6l4 4 4-4"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </summary>
      <div className="settings-collapsible-body">{children}</div>
    </details>
  );
};

export default CollapsibleSection;
