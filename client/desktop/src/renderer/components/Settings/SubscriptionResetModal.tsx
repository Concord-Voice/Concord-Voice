import React, { useEffect, useRef } from 'react';
import { openSubscriptionPage } from '../../utils/openSubscriptionPage';
import { useDialogSync } from '../../hooks/useDialogSync';
import './SubscriptionResetModal.css';

// Mirror SyntaxHelpModal's environment probe: jsdom implements <dialog> but does
// NOT fire native `cancel`/`close` on Escape via .showModal(); Electron/Chrome
// do. We install a fallback Escape handler ONLY under jsdom to avoid
// double-firing onClose in production.
const dialogCancelsOnEscape = (() => {
  if (typeof document === 'undefined') return true;
  return globalThis.navigator !== undefined && !/jsdom/i.test(globalThis.navigator.userAgent ?? '');
})();

export interface SubscriptionResetModalProps {
  open: boolean;
  /** Dismiss + persist the acknowledgement ("Got it"). */
  onAcknowledge: () => void;
}

/**
 * One-time launch-reset explainer (#1301 Decision 4 / dossier §I.3). Shown once,
 * ever, to a free user whose premium settings were clamped to free defaults.
 * The ONLY modal in #1301 — every other lock affordance is non-modal (anti-nag
 * invariant).
 *
 * Native `<dialog>` `.showModal()` provides the focus trap (a11y O2) AND returns
 * focus to the element that was focused before opening, on close. Both actions
 * dismiss; the secondary action also routes to the Subscription page.
 */
const SubscriptionResetModal: React.FC<SubscriptionResetModalProps> = ({ open, onAcknowledge }) => {
  const dialogRef = useRef<HTMLDialogElement>(null);

  // jsdom-only Escape fallback (Escape = "Got it" / acknowledge).
  useEffect(() => {
    if (!open) return;
    if (dialogCancelsOnEscape) return;
    const handler = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onAcknowledge();
    };
    globalThis.addEventListener('keydown', handler);
    return () => {
      globalThis.removeEventListener('keydown', handler);
    };
  }, [open, onAcknowledge]);

  // Sync imperative <dialog> open state with the React-controlled prop.
  useDialogSync(dialogRef, open);

  if (!open) return null;

  const handleViewSubscriptions = (): void => {
    onAcknowledge();
    openSubscriptionPage();
  };

  return (
    <dialog
      ref={dialogRef}
      className="subscription-reset-modal-backdrop"
      aria-modal="true"
      aria-labelledby="subscription-reset-modal-title"
      aria-describedby="subscription-reset-modal-body"
      onClose={onAcknowledge}
    >
      <div className="subscription-reset-modal">
        <h2 id="subscription-reset-modal-title">Some settings were reset</h2>
        <p id="subscription-reset-modal-body">
          Your audio/video quality was reset to free defaults.
        </p>
        <div className="subscription-reset-modal__actions">
          <button
            type="button"
            className="subscription-reset-modal__secondary"
            onClick={handleViewSubscriptions}
          >
            View planned subscriptions
          </button>
          <button
            type="button"
            className="subscription-reset-modal__primary"
            onClick={onAcknowledge}
          >
            Got it
          </button>
        </div>
      </div>
    </dialog>
  );
};

export default SubscriptionResetModal;
