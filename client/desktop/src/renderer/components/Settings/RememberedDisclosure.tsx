import React from 'react';
import { useSettingsCollapsibleStore } from '../../stores/ui/settingsCollapsibleStore';

interface RememberedDisclosureProps {
  id: string;
  summary: React.ReactNode;
  children: React.ReactNode;
  className?: string;
  summaryClassName?: string;
  name?: string;
  onCollapse?: () => void;
}

/** Shared native disclosure binding for main sections and their nested content. */
const RememberedDisclosure: React.FC<RememberedDisclosureProps> = ({
  id,
  summary,
  children,
  className,
  summaryClassName,
  name,
  onCollapse,
}) => {
  const open = useSettingsCollapsibleStore((state) => state.openSections[id] === true);
  const setSectionOpen = useSettingsCollapsibleStore((state) => state.setSectionOpen);

  const handleToggle = (event: React.SyntheticEvent<HTMLDetailsElement>) => {
    if (event.target !== event.currentTarget) return;
    const isOpen = event.currentTarget.open;
    setSectionOpen(id, isOpen);
    if (!isOpen) onCollapse?.();
  };

  return (
    <details id={id} className={className} name={name} open={open} onToggle={handleToggle}>
      <summary className={summaryClassName}>{summary}</summary>
      {children}
    </details>
  );
};

export default RememberedDisclosure;
