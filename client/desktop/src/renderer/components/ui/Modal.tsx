import React, { useContext, useEffect, useId, useRef } from 'react';
import { ModalDepthContext, useModalStack } from './ModalContext';
import './Modal.css';

export interface ModalProps {
  isOpen: boolean;
  onClose: () => void;
  title: string;
  children: React.ReactNode;
  width?: 'small' | 'medium' | 'large' | 'xlarge';
  dismissable?: boolean;
}

const Modal: React.FC<ModalProps> = ({
  isOpen,
  onClose,
  title,
  children,
  width = 'medium',
  dismissable = true,
}) => {
  const overlayRef = useRef<HTMLDivElement>(null);
  const modalId = useId();
  // eslint-disable-next-line @eslint-react/no-use-context -- useContext is the appropriate API here; use() would change conditional-hook semantics for this depth read
  const depth = useContext(ModalDepthContext);
  const { register, unregister, isTopmost } = useModalStack();

  useEffect(() => {
    if (!isOpen) return;

    register(modalId, depth);
    return () => unregister(modalId);
  }, [isOpen, modalId, depth, register, unregister]);

  useEffect(() => {
    if (!isOpen) return;

    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && dismissable && isTopmost(modalId)) {
        e.stopImmediatePropagation();
        onClose();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [isOpen, onClose, modalId, isTopmost, dismissable]);

  const handleOverlayClick = (e: React.MouseEvent) => {
    if (e.target === overlayRef.current && dismissable && isTopmost(modalId)) {
      e.stopPropagation();
      onClose();
    }
  };

  if (!isOpen) return null;

  const content = (
    <div className="modal-overlay" ref={overlayRef} onClick={handleOverlayClick}>
      <div className={`modal-container modal-${width}`}>
        <div className="modal-header">
          <h3 className="modal-title">{title}</h3>
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
      </div>
    </div>
  );
  // eslint-disable-next-line @eslint-react/no-context-provider -- Context.Provider pattern required for depth nesting; React 19 Context-as-JSX refactor deferred
  return <ModalDepthContext.Provider value={depth + 1}>{content}</ModalDepthContext.Provider>;
};

export default Modal;
