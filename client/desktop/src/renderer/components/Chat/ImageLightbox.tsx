import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Download, RotateCcw, X, ZoomIn, ZoomOut } from 'lucide-react';
import './ImageLightbox.css';

export interface ImageLightboxProps {
  /**
   * The already-decrypted blob/object URL for the attachment. Reused as-is —
   * the lightbox never re-fetches or re-decrypts, and it does NOT revoke this
   * URL on close because the inline `<img>` (and `blobUrlCache`) still own it.
   */
  src: string;
  alt: string;
  onClose: () => void;
  /**
   * Invoked when the user picks Save. Omit to hide the control. The caller owns
   * the native Save-As flow (it holds the attachment id / mime to name the file).
   */
  onSave?: () => void;
}

const MIN_SCALE = 1;
const MAX_SCALE = 5;
const SCALE_STEP = 0.5;
const clampScale = (s: number): number => Math.min(MAX_SCALE, Math.max(MIN_SCALE, s));

/**
 * Full-screen image viewer for message attachments (#1729). Click an inline
 * image to open; scroll / the toolbar buttons / double-click zoom; drag to pan
 * when zoomed. Escape, the toolbar ✕, or a backdrop click dismiss it.
 *
 * Accessibility: rendered as a native `<dialog open>` (semantic dialog, no
 * `role`). Interactive affordances live on real interactive elements — the
 * backdrop dismiss is a `<button>`, the toolbar holds `<button>`s, and the
 * `<dialog>` itself carries no click/keyboard JSX handlers (wheel-zoom + the
 * Tab focus-trap + Escape are imperative ref/document listeners). Portaled to
 * document.body so the fixed overlay always covers the viewport. `open` (not
 * `showModal()`) keeps it jsdom-testable; modality is enforced by the focus trap.
 */
const ImageLightbox: React.FC<ImageLightboxProps> = ({ src, alt, onClose, onSave }) => {
  const overlayRef = useRef<HTMLDialogElement>(null);
  const closeBtnRef = useRef<HTMLButtonElement>(null);
  const [scale, setScale] = useState(MIN_SCALE);
  const [offset, setOffset] = useState({ x: 0, y: 0 });
  const dragRef = useRef<{ x: number; y: number; ox: number; oy: number } | null>(null);
  const isReset = scale === MIN_SCALE && offset.x === 0 && offset.y === 0;

  const reset = useCallback(() => {
    setScale(MIN_SCALE);
    setOffset({ x: 0, y: 0 });
  }, []);

  const zoomBy = useCallback((delta: number) => {
    setScale((s) => {
      const next = clampScale(s + delta);
      if (next === MIN_SCALE) setOffset({ x: 0, y: 0 }); // recenter when fully zoomed out
      return next;
    });
  }, []);

  // Escape closes (document-level so it fires regardless of which control has
  // focus). stopImmediatePropagation keeps an unrelated global Esc handler from
  // also firing on the same keypress.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopImmediatePropagation();
        onClose();
      }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  // Focus trap — keep Tab within the overlay. A document listener (rather than a
  // JSX handler on the non-interactive <dialog>) so the trap composes with the
  // native dialog semantics. The tabindex=-1 backdrop is excluded from the ring.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key !== 'Tab') return;
      const focusables = overlayRef.current?.querySelectorAll<HTMLElement>(
        'button:not([disabled]):not([tabindex="-1"])'
      );
      if (!focusables || focusables.length === 0) return;
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    return () => document.removeEventListener('keydown', onKeyDown);
  }, []);

  // Wheel-zoom as a ref listener (not a JSX onWheel on the <dialog>) so the
  // dialog stays free of interaction handlers. Passive: zoom only mutates state.
  useEffect(() => {
    const el = overlayRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => zoomBy(e.deltaY < 0 ? SCALE_STEP : -SCALE_STEP);
    el.addEventListener('wheel', onWheel, { passive: true });
    return () => el.removeEventListener('wheel', onWheel);
  }, [zoomBy]);

  // Focus the close button on open; restore focus to the trigger on close (a11y).
  useEffect(() => {
    const previouslyFocused = document.activeElement as HTMLElement | null;
    closeBtnRef.current?.focus();
    return () => previouslyFocused?.focus?.();
  }, []);

  const onDoubleClick = () => {
    if (scale > MIN_SCALE) reset();
    else setScale(2);
  };

  // Pan when zoomed in.
  const onPointerDown = (e: React.PointerEvent) => {
    if (scale <= MIN_SCALE) return;
    dragRef.current = { x: e.clientX, y: e.clientY, ox: offset.x, oy: offset.y };
    (e.currentTarget as HTMLElement).setPointerCapture?.(e.pointerId);
  };
  const onPointerMove = (e: React.PointerEvent) => {
    const d = dragRef.current;
    if (!d) return;
    setOffset({ x: d.ox + (e.clientX - d.x), y: d.oy + (e.clientY - d.y) });
  };
  const onPointerUp = () => {
    dragRef.current = null;
  };

  return createPortal(
    <dialog ref={overlayRef} open className="image-lightbox-overlay" aria-label="Image viewer">
      {/* Full-bleed backdrop dismiss — a real <button> (interactive element), so
          "click outside to close" carries no a11y smell; Escape is the keyboard
          equivalent. tabindex=-1 keeps it out of the Tab ring (the toolbar ✕ is
          the focusable close); it sits behind the toolbar/image via z-index. */}
      <button
        type="button"
        className="image-lightbox-backdrop"
        aria-label="Close image viewer"
        tabIndex={-1}
        onClick={onClose}
      />
      <div className="image-lightbox-toolbar">
        <button
          type="button"
          className="image-lightbox-btn"
          onClick={() => zoomBy(-SCALE_STEP)}
          disabled={scale <= MIN_SCALE}
          aria-label="Zoom out"
        >
          <ZoomOut size={18} />
        </button>
        <span className="image-lightbox-zoom" aria-live="polite">
          {Math.round(scale * 100)}%
        </span>
        <button
          type="button"
          className="image-lightbox-btn"
          onClick={() => zoomBy(SCALE_STEP)}
          disabled={scale >= MAX_SCALE}
          aria-label="Zoom in"
        >
          <ZoomIn size={18} />
        </button>
        <button
          type="button"
          className="image-lightbox-btn"
          onClick={reset}
          disabled={isReset}
          aria-label="Reset zoom"
        >
          <RotateCcw size={18} />
        </button>
        {onSave && (
          <button
            type="button"
            className="image-lightbox-btn"
            onClick={onSave}
            aria-label="Save image"
          >
            <Download size={18} />
          </button>
        )}
        <button
          type="button"
          ref={closeBtnRef}
          className="image-lightbox-btn image-lightbox-close"
          onClick={onClose}
          aria-label="Close"
        >
          <X size={18} />
        </button>
      </div>
      <img
        src={src}
        alt={alt}
        className="image-lightbox-image"
        style={{
          transform: `translate(${offset.x}px, ${offset.y}px) scale(${scale})`,
          cursor: scale > MIN_SCALE ? 'grab' : 'default',
        }}
        onDoubleClick={onDoubleClick}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        draggable={false}
      />
    </dialog>,
    document.body
  );
};

export default ImageLightbox;
