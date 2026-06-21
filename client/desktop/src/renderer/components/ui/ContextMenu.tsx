import React, { useEffect, useRef, useState, useCallback } from 'react';
import './ContextMenu.css';

/* ------------------------------------------------------------------ */
/*  Main ContextMenu wrapper                                          */
/* ------------------------------------------------------------------ */

interface ContextMenuProps {
  position: { x: number; y: number };
  onClose: () => void;
  children: React.ReactNode;
}

const CLOSE_DURATION = 150; // ms — matches CSS animation

const ContextMenuRoot: React.FC<ContextMenuProps> = ({ position, onClose, children }) => {
  const menuRef = useRef<HTMLDivElement>(null);
  const [closing, setClosing] = useState(false);
  const closingRef = useRef(false);

  const animateClose = useCallback(() => {
    if (closingRef.current) return;
    closingRef.current = true;
    setClosing(true);
    setTimeout(() => {
      onClose();
    }, CLOSE_DURATION);
  }, [onClose]);

  // Click-outside & Escape to close
  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (e.target instanceof Node && menuRef.current && !menuRef.current.contains(e.target)) {
        animateClose();
      }
    };

    const handleEscape = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        animateClose();
      }
    };

    document.addEventListener('mousedown', handleClickOutside);
    document.addEventListener('keydown', handleEscape);
    return () => {
      document.removeEventListener('mousedown', handleClickOutside);
      document.removeEventListener('keydown', handleEscape);
    };
  }, [animateClose]);

  // Viewport overflow adjustment
  useEffect(() => {
    if (menuRef.current) {
      const rect = menuRef.current.getBoundingClientRect();
      const viewportHeight = globalThis.innerHeight;
      const viewportWidth = globalThis.innerWidth;

      if (rect.bottom > viewportHeight) {
        menuRef.current.style.top = `${position.y - rect.height}px`;
      }
      if (rect.right > viewportWidth) {
        menuRef.current.style.left = `${position.x - rect.width}px`;
      }
    }
  }, [position]);

  return (
    <div className={`ctx-menu-overlay ${closing ? 'ctx-menu-overlay-closing' : ''}`}>
      <div
        ref={menuRef}
        className={`ctx-menu ${closing ? 'ctx-menu-closing' : ''}`}
        style={{ top: position.y, left: position.x }}
      >
        {children}
      </div>
    </div>
  );
};

/* ------------------------------------------------------------------ */
/*  Sub-components                                                    */
/* ------------------------------------------------------------------ */

interface HeaderProps {
  children: React.ReactNode;
}

const Header: React.FC<HeaderProps> = ({ children }) => (
  <div className="ctx-menu-header">{children}</div>
);

const Separator: React.FC = () => <div className="ctx-menu-separator" />;

interface ItemProps {
  icon?: React.ReactNode;
  label: string;
  danger?: boolean;
  disabled?: boolean;
  onClick: () => void;
  /** Show a chevron indicating a submenu */
  hasSubMenu?: boolean;
}

const Item: React.FC<ItemProps> = ({ icon, label, danger, disabled, onClick, hasSubMenu }) => (
  <button
    className={`ctx-menu-item ${danger ? 'ctx-menu-item-danger' : ''} ${disabled ? 'ctx-menu-item-disabled' : ''}`}
    onClick={disabled ? undefined : onClick}
    disabled={disabled}
  >
    {icon != null && <span className="ctx-menu-item-icon">{icon}</span>}
    <span style={{ flex: 1 }}>{label}</span>
    {hasSubMenu && (
      <span className="ctx-menu-item-chevron">
        <svg width="12" height="12" viewBox="0 0 12 12" fill="none">
          <path
            d="M4.5 2.5L8 6L4.5 9.5"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      </span>
    )}
  </button>
);

/* ------------------------------------------------------------------ */
/*  SubMenu – flyout panel that appears beside the trigger item       */
/* ------------------------------------------------------------------ */

interface SubMenuProps {
  children: React.ReactNode;
  closing?: boolean;
}

const SubMenu: React.FC<SubMenuProps> = ({ children, closing }) => {
  const subRef = useRef<HTMLDivElement>(null);
  const [flipped, setFlipped] = useState(false);

  useEffect(() => {
    if (!subRef.current) return;
    const el = subRef.current;
    const rect = el.getBoundingClientRect();
    const vw = globalThis.innerWidth;
    const vh = globalThis.innerHeight;

    // Flip to the left side if overflowing right
    if (rect.right > vw) {
      setFlipped(true);
    }

    // Nudge up if overflowing bottom
    if (rect.bottom > vh) {
      const overflow = rect.bottom - vh + 8;
      el.style.top = `${-overflow}px`;
    }
  }, []);

  const classes = [
    'ctx-submenu',
    flipped ? 'ctx-submenu-flip' : '',
    closing ? 'ctx-submenu-closing' : '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <div ref={subRef} className={classes}>
      {children}
    </div>
  );
};

/* ------------------------------------------------------------------ */
/*  Compose & export                                                  */
/* ------------------------------------------------------------------ */

const ContextMenu = Object.assign(ContextMenuRoot, {
  Header,
  Separator,
  Item,
  SubMenu,
});

export default ContextMenu;
