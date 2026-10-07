import React from 'react';

interface CollapsibleSectionProps {
  id: string;
  title: string;
  children: React.ReactNode;
  defaultOpen?: boolean;
  onCollapse?: () => void;
}

const CollapsibleSection: React.FC<CollapsibleSectionProps> = ({
  id,
  title,
  children,
  defaultOpen = false,
  onCollapse,
}) => {
  return (
    <details
      className="settings-section settings-collapsible"
      id={id}
      open={defaultOpen}
      onToggle={(event) => {
        if (event.target === event.currentTarget && !event.currentTarget.open) onCollapse?.();
      }}
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
