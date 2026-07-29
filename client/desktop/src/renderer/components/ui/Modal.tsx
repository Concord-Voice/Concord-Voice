import React, { useContext, useEffect, useId, useRef } from 'react';
import { createPortal } from 'react-dom';
import { ModalDepthContext, ModalPortalHostContext, useModalStack } from './ModalContext';
import './Modal.css';

export interface ModalProps {
  isOpen: boolean;
  onClose: () => void;
  title: string;
  children: React.ReactNode;
  width?: 'small' | 'medium' | 'large' | 'xlarge';
  dismissable?: boolean;
  initialFocusRef?: React.RefObject<HTMLElement | null>;
}

// Focusable descendants of a modal container, in DOM order, for the Tab trap.
// ponytail: no visibility filter — jsdom reports offsetParent === null for
// everything (no layout engine), so an offsetParent check would drop every
// element under test; a hidden focusable inside a modal overlay is not a real
// case. Disabled/aria-hidden are excluded because those genuinely skip tab order.
function getFocusable(container: HTMLElement): HTMLElement[] {
  const selector = 'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])';
  return Array.from(container.querySelectorAll<HTMLElement>(selector)).filter(
    (el) => !el.hasAttribute('disabled') && el.getAttribute('aria-hidden') !== 'true'
  );
}

const Modal: React.FC<ModalProps> = ({
  isOpen,
  onClose,
  title,
  children,
  width = 'medium',
  dismissable = true,
  initialFocusRef,
}) => {
  const overlayRef = useRef<HTMLDivElement>(null);
  const containerRef = useRef<HTMLDialogElement>(null);
  const previousFocusRef = useRef<HTMLElement | null>(null);
  const modalId = useId();
  const titleId = useId();
  // eslint-disable-next-line @eslint-react/no-use-context -- useContext is the appropriate API here; use() would change conditional-hook semantics for this depth read
  const depth = useContext(ModalDepthContext);
  // eslint-disable-next-line @eslint-react/no-use-context -- useContext is the appropriate API for this optional portal-host read
  const portalHost = useContext(ModalPortalHostContext);
  const { register, unregister, isTopmost } = useModalStack();

  useEffect(() => {
    if (!isOpen) return;

    // Capture the invoking element BEFORE register() runs syncInert(). Making
    // #root inert blurs any focused descendant to <body> per the HTML inert spec,
    // and the invoker lives inside #root, so capturing after register() would
    // record <body> and strand focus there on close. jsdom does not implement
    // inert's blur, so this ordering only matters in a real browser. This is the
    // capture half of focus return; the restore half is the next effect's
    // cleanup. WCAG 2.1 SC 2.4.3 (#2087).
    previousFocusRef.current = document.activeElement as HTMLElement | null;
    if (previousFocusRef.current?.id) {
      overlayRef.current?.setAttribute('data-dock-focus-owner', previousFocusRef.current.id);
    }
    // Pass the overlay element so ModalContext can make it inert when this modal
    // is not the topmost of a nested stack (#2087). overlayRef is set by the time
    // this post-commit effect runs (the portal content is already mounted).
    register(modalId, depth, overlayRef.current);
    return () => unregister(modalId);
  }, [isOpen, modalId, depth, register, unregister]);

  useEffect(() => {
    if (!isOpen) return;

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key !== 'Escape' || !isTopmost(modalId)) return;
      // A child Modal may live inside a native showModal() host. Consume Escape
      // in capture phase so neither that host's listener nor the browser's
      // default cancel action can close the parent Settings surface. A
      // non-dismissible child must consume it too; dismissable controls only
      // whether this modal invokes onClose.
      e.preventDefault();
      e.stopImmediatePropagation();
      if (dismissable) onClose();
    };
    document.addEventListener('keydown', handleKeyDown, true);
    return () => document.removeEventListener('keydown', handleKeyDown, true);
  }, [isOpen, onClose, modalId, isTopmost, dismissable]);

  // Move focus into the dialog on open, and restore it to the invoking element
  // on close. Gated on isTopmost so a parent modal does not steal focus from a
  // just-opened child (React runs child effects before parent effects on mount,
  // so the parent's effect sees the child already registered at a higher depth).
  // WCAG 2.1 — 2.4.3 (Focus Order).
  useEffect(() => {
    if (!isOpen || !isTopmost(modalId)) return;
    // The invoker was captured in the register effect above (before #root became
    // inert). Here we only move focus into the dialog; the cleanup restores it.
    (initialFocusRef?.current ?? containerRef.current)?.focus();
    return () => {
      previousFocusRef.current?.focus?.();
    };
  }, [isOpen, isTopmost, modalId, initialFocusRef]);

  // Contain Tab / Shift-Tab within the topmost modal. A document-level listener
  // (mirroring the Escape handler above) is used rather than an onKeyDown on the
  // container so the trap still fires when focus has escaped to the body — the
  // whole point of a focus trap — and so the non-interactive container <dialog>
  // carries no event handler. Only the topmost modal acts (isTopmost), so nested
  // modals don't fight.
  useEffect(() => {
    if (!isOpen) return;

    const handleTab = (e: KeyboardEvent) => {
      if (e.key !== 'Tab' || !isTopmost(modalId)) return;
      const container = containerRef.current;
      if (!container) return;
      const focusables = getFocusable(container);
      if (focusables.length === 0) {
        e.preventDefault();
        container.focus();
        return;
      }
      const first = focusables[0];
      const last = focusables.at(-1) ?? first;
      const active = document.activeElement;
      if (e.shiftKey) {
        if (active === first || active === container || !container.contains(active)) {
          e.preventDefault();
          last.focus();
        }
      } else if (active === last || active === container || !container.contains(active)) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', handleTab);
    return () => document.removeEventListener('keydown', handleTab);
  }, [isOpen, modalId, isTopmost]);

  const handleOverlayClick = (e: React.MouseEvent) => {
    if (e.target === overlayRef.current && dismissable && isTopmost(modalId)) {
      e.stopPropagation();
      onClose();
    }
  };

  if (!isOpen) return null;

  const content = (
    <div className="modal-overlay" ref={overlayRef} onClick={handleOverlayClick}>
      {/* Native <dialog> (declarative `open`, not showModal) gives the implicit
          dialog role + AT semantics while staying in normal flow inside the
          overlay — so ModalContext stacking, the overlay, and the hand-rolled
          focus trap are unchanged. aria-modal asserts modality (declarative open
          is non-modal natively; our overlay + trap enforce it). */}
      <dialog
        className={`modal-container modal-${width}`}
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        ref={containerRef}
        open
      >
        <div className="modal-header">
          <h3 className="modal-title" id={titleId}>
            {title}
          </h3>
          {dismissable && (
            <button className="modal-close" onClick={onClose} aria-label="Close">
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                <path
                  d="M4 4l8 8M12 4l-8 8"
                  stroke="currentColor"
                  strokeWidth="2"
                  strokeLinecap="round"
                />
              </svg>
            </button>
          )}
        </div>
        <div className="modal-body">{children}</div>
      </dialog>
    </div>
  );
  // Portal outside #root — normally to document.body, or to a designated native
  // dialog host that itself lives beside #root (#2087). This lets ModalContext
  // make #root `inert` without inert-ing the modal itself. Context still flows
  // through the portal (React tree is preserved), so nested modals keep depth+1.
  // A native dialog opened with showModal() lives in the browser top layer.
  // Portaling a child modal to document.body would leave it underneath that
  // dialog regardless of z-index, so designated hosts keep their child modal
  // inside the same top-layer subtree (SettingsOverlayHost is one such host).
  return createPortal(
    // eslint-disable-next-line @eslint-react/no-context-provider -- Context.Provider pattern required for depth nesting; React 19 Context-as-JSX refactor deferred
    <ModalDepthContext.Provider value={depth + 1}>{content}</ModalDepthContext.Provider>,
    portalHost ?? document.body
  );
};

export default Modal;
