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

/** Minimum gap kept between a menu (or submenu) and the viewport edge. */
const VIEWPORT_MARGIN = 8;

/** Gap between a submenu and its trigger — `.ctx-submenu`'s side margin in the CSS. */
const SUBMENU_GAP = 6;

/**
 * Clamp one axis of a box into `[VIEWPORT_MARGIN, viewport − size − VIEWPORT_MARGIN]`.
 * A box larger than the space available pins to the margin, and its overflow is
 * made scrollable by `fitToViewport` — it never escapes past the top or left
 * edge, where nothing could ever scroll it back into reach.
 */
function clampToViewport(start: number, size: number, viewport: number): number {
  return Math.max(VIEWPORT_MARGIN, Math.min(start, viewport - size - VIEWPORT_MARGIN));
}

/** Cap a box taller than the viewport to the viewport minus margins and let it
 *  scroll. Applied only when it is actually needed — see the note on
 *  `.ctx-menu` overflow in ContextMenu.css for why this is not unconditional. */
function fitToViewport(el: HTMLElement, height: number, viewportHeight: number): void {
  const available = viewportHeight - 2 * VIEWPORT_MARGIN;
  if (height <= available) return;
  el.style.maxHeight = `${available}px`;
  el.style.overflowY = 'auto';
}

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

  // Viewport overflow adjustment: flip to the other side of the cursor when the
  // menu would overflow, then CLAMP. Flipping alone put a menu taller than the
  // space above the cursor at a negative `top` — measured at y=−55 for a click
  // at y=383 under 2× UI scale — where its first items were unreachable.
  //
  // Sized from `offsetWidth`/`offsetHeight`, the layout box, not
  // `getBoundingClientRect()`: the `ctxMenuIn` entrance animation starts at
  // `scale(0.92)`, so a rect read on mount under-measures the menu by 8 %.
  useEffect(() => {
    const el = menuRef.current;
    if (!el) return;
    const width = el.offsetWidth;
    const height = el.offsetHeight;
    const viewportHeight = globalThis.innerHeight;
    const viewportWidth = globalThis.innerWidth;

    const flippedTop = position.y + height > viewportHeight ? position.y - height : position.y;
    const flippedLeft = position.x + width > viewportWidth ? position.x - width : position.x;
    const top = clampToViewport(flippedTop, height, viewportHeight);
    const left = clampToViewport(flippedLeft, width, viewportWidth);

    // Written only when it differs from the rendered position, so a menu that
    // already fits is left exactly where React put it.
    if (top !== position.y) el.style.top = `${top}px`;
    if (left !== position.x) el.style.left = `${left}px`;
    fitToViewport(el, height, viewportHeight);
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
  /**
   * Inert, but still FOCUSABLE — `aria-disabled` rather than the native
   * attribute, with the same greyed styling and the same activation guard.
   *
   * Use this, not `disabled`, when the item's LABEL is the only place a fact is
   * stated ("Alpha — already a member"): a natively disabled button leaves the
   * focus order and takes its label with it, so the explanation never reaches a
   * keyboard or screen-reader user — which defeats greying an item rather than
   * omitting it. Same swap, same reason, as the ScreenSharePicker toggle
   * (#3198 PR 2).
   *
   * Keep `disabled` for a TRANSIENT busy state ("Updating…"), where there is no
   * label to read and letting someone tab onto a control that does nothing is
   * worse than removing it from the order.
   */
  ariaDisabled?: boolean;
  onClick: () => void;
  /** Show a chevron indicating a submenu */
  hasSubMenu?: boolean;
}

const Item: React.FC<ItemProps> = ({
  icon,
  label,
  danger,
  disabled,
  ariaDisabled,
  onClick,
  hasSubMenu,
}) => (
  <button
    className={`ctx-menu-item ${danger ? 'ctx-menu-item-danger' : ''} ${disabled || ariaDisabled ? 'ctx-menu-item-disabled' : ''}`}
    // Both forms block activation here, which is what covers pointer AND
    // keyboard for the aria form: Enter and Space on a focused button dispatch
    // a click, so the one guard is the whole story.
    onClick={disabled || ariaDisabled ? undefined : onClick}
    disabled={disabled}
    aria-disabled={ariaDisabled || undefined}
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
    // Measured from the layout box, not the submenu's own rect: `ctxSubMenuIn`
    // starts at `translateX(-8px) scale(0.96)`, so a rect read on mount is shifted
    // and under-measures the size — the root menu's defect, documented above.
    // Position comes from the positioned parent (not animated once the root menu
    // has settled) plus the layout offset.
    const width = el.offsetWidth;
    const height = el.offsetHeight;
    const parentRect = (el.offsetParent as HTMLElement | null)?.getBoundingClientRect();
    const rect = {
      top: (parentRect?.top ?? 0) + el.offsetTop,
      left: (parentRect?.left ?? 0) + el.offsetLeft,
      height,
    };
    const vw = globalThis.innerWidth;
    const vh = globalThis.innerHeight;

    // Flip to the left side if overflowing right — then CLAMP, as the root menu
    // does. Flipped, the submenu's right edge sits SUBMENU_GAP left of its
    // trigger, so one wider than the space there (a wide root menu pinned to the
    // right of an 800px layout) put its leading items at a negative `left`.
    if (rect.left + width > vw) {
      setFlipped(true);
      const parentLeft = parentRect?.left ?? 0;
      const flippedLeft = parentLeft - SUBMENU_GAP - width;
      const left = clampToViewport(flippedLeft, width, vw);
      if (left !== flippedLeft) {
        // Pin the measured width: moving `left` changes the space this
        // shrink-to-fit box is laid out in, and with it the width just measured.
        el.style.width = `${width}px`;
        el.style.left = `${left - parentLeft}px`;
        el.style.right = 'auto';
      }
    }

    // Nudge up if overflowing bottom — by no more than keeps the top edge on
    // screen. The unbounded nudge had the root menu's defect: a submenu taller
    // than the space above its trigger was pushed past the top of the viewport.
    // `top` is relative to the trigger's wrapper, so the viewport shift is
    // applied to the current offset rather than written as an absolute.
    if (rect.top + height > vh) {
      const shift = clampToViewport(rect.top, rect.height, vh) - rect.top;
      el.style.top = `${el.offsetTop + shift}px`;
      // A submenu has no nested flyout of its own, so scrolling it clips nothing.
      fitToViewport(el, rect.height, vh);
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
