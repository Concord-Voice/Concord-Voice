import { useCallback, useEffect, useEffectEvent, useLayoutEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import './AppLayout.css';

export interface AttributedPopoverProps {
  id: string;
  anchor: HTMLElement | null;
  label: string;
  open: boolean;
  placement?: 'left' | 'right';
  onClose: () => void;
  children: React.ReactNode;
}

const VIEWPORT_GUTTER = 8;
const ANCHOR_GAP = 12;
const FALLBACK_WIDTH = 280;

export const AttributedPopover: React.FC<AttributedPopoverProps> = ({
  id,
  anchor,
  label,
  open,
  placement = 'left',
  onClose,
  children,
}) => {
  const surfaceRef = useRef<HTMLDivElement>(null);
  const arrowRef = useRef<HTMLSpanElement>(null);
  const anchorRef = useRef(anchor);
  const close = useEffectEvent(onClose);
  const attributed = open && anchor !== null;

  const position = useCallback(() => {
    const surface = surfaceRef.current;
    const arrow = arrowRef.current;
    if (!open || !anchor || !surface || !arrow) return;
    anchorRef.current = anchor;

    const anchorRect = anchor.getBoundingClientRect();
    const surfaceRect = surface.getBoundingClientRect();
    const width = surfaceRect.width || Math.min(FALLBACK_WIDTH, innerWidth - VIEWPORT_GUTTER * 2);
    const height = surfaceRect.height;
    const preferredLeft =
      placement === 'right' ? anchorRect.right + ANCHOR_GAP : anchorRect.left - width - ANCHOR_GAP;
    const left = Math.max(
      VIEWPORT_GUTTER,
      Math.min(preferredLeft, innerWidth - width - VIEWPORT_GUTTER)
    );
    const top = Math.max(
      VIEWPORT_GUTTER,
      Math.min(anchorRect.top, innerHeight - height - VIEWPORT_GUTTER)
    );
    const arrowTop = Math.max(
      16,
      Math.min(anchorRect.top + anchorRect.height / 2 - top, Math.max(16, height - 16))
    );

    surface.style.left = `${left}px`;
    surface.style.top = `${top}px`;
    surface.style.visibility = 'visible';
    arrow.style.top = `${arrowTop}px`;
  }, [anchor, open, placement]);

  useLayoutEffect(position, [position]);

  useEffect(() => {
    const surface = surfaceRef.current;
    if (!attributed || !surface) return;

    const restoreFocus = () => {
      const currentAnchor = anchorRef.current;
      if (currentAnchor?.isConnected) currentAnchor.focus({ preventScroll: true });
    };
    const dismiss = () => {
      close();
      restoreFocus();
    };
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      event.stopPropagation();
      dismiss();
    };
    const handleOutsidePointer = (event: PointerEvent) => {
      const currentAnchor = anchorRef.current;
      if (
        event.target instanceof Node &&
        (surface.contains(event.target) || currentAnchor?.contains(event.target))
      ) {
        return;
      }
      dismiss();
    };

    document.addEventListener('keydown', handleKeyDown);
    document.addEventListener('pointerdown', handleOutsidePointer);
    return () => {
      document.removeEventListener('keydown', handleKeyDown);
      document.removeEventListener('pointerdown', handleOutsidePointer);
      restoreFocus();
    };
  }, [attributed]);

  useEffect(() => {
    if (!attributed) return;
    window.addEventListener('resize', position);
    window.addEventListener('scroll', position, true);
    return () => {
      window.removeEventListener('resize', position);
      window.removeEventListener('scroll', position, true);
    };
  }, [attributed, position]);

  if (!open || !anchor) return null;

  return createPortal(
    <section
      id={id}
      ref={surfaceRef}
      className="attributed-popover"
      aria-label={label}
      data-placement={placement}
      data-dock-focus-owner={anchor.id || undefined}
      data-reduce-motion={String(document.documentElement.dataset.reduceAnimations === 'true')}
      style={{ visibility: 'hidden', maxHeight: 'calc(100vh - 16px)' }}
    >
      <span
        ref={arrowRef}
        className={`attributed-popover__arrow attributed-popover__arrow--${placement}`}
        aria-hidden="true"
      />
      <div className="attributed-popover__label">{label}</div>
      <div className="attributed-popover__body">{children}</div>
    </section>,
    document.body
  );
};
