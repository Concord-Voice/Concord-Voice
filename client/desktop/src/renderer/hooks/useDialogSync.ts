import { useEffect } from 'react';

/**
 * Syncs an imperative native <dialog> element with a React-controlled `open`
 * prop. `.showModal()`/`.close()` move focus into / back out of the dialog
 * for assistive technology. jsdom and some older renderers don't implement
 * showModal — fall back to setting the `open` attribute (non-modal, but keeps
 * the dialog visible and testable).
 *
 * Deliberately NOT `ui/Modal`: consumers of this hook opt out of the
 * portal/focus-trap/modal-stack system by design (native-dialog posture) and
 * need only open-state sync. Shared by SubscriptionResetModal and
 * SyntaxHelpModal.
 */
export function useDialogSync(
  dialogRef: React.RefObject<HTMLDialogElement | null>,
  open: boolean
): void {
  useEffect(() => {
    const el = dialogRef.current;
    if (!el) return;
    if (open && !el.open) {
      if (typeof el.showModal === 'function') {
        try {
          el.showModal();
        } catch {
          el.setAttribute('open', '');
        }
      } else {
        el.setAttribute('open', '');
      }
    } else if (!open && el.open) {
      el.close();
    }
  }, [dialogRef, open]);
}
