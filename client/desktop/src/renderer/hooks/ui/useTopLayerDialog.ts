import { useEffect, useId, useRef } from 'react';
import { useModalStack } from '../../components/ui/ModalContext';
import { GLOBAL_OVERLAY_ATTRIBUTE } from '../../services/system/keyboardShortcutService';

// ModalContext treats the highest depth as topmost, so an entry here outranks
// every ui/Modal, including one opened later. Among these entries nothing reads
// the order: each has `el: null`, and only a ui/Modal asks isTopmost(). (It is
// not the top-layer order anyway, since the MFA challenge retakes the top.)
export const TOP_LAYER_DEPTH = Number.MAX_SAFE_INTEGER;

function cancelEscape(e: KeyboardEvent) {
  if (e.key === 'Escape') e.preventDefault();
}

/**
 * Opens a global overlay as a modal <dialog> that stays reachable over the
 * Settings dialog and any open ui/Modal ([internal]rules/frontend.md § "A global
 * overlay must be reachable over the Settings dialog").
 *
 * - showModal() puts the dialog in the browser top layer, above Settings. It
 *   also escapes an inert ancestor (measured, Chromium 152), so the dialog may
 *   render inside #root.
 * - The topmost ModalContext entry makes every ui/Modal non-topmost, so their
 *   document-level Escape and Tab handlers stand down. `el` is null: the top
 *   layer ignores z-index, so syncInert has nothing to write.
 * - Focus returns to the element that had it once the dialog unmounts.
 * - The dialog is marked as a global overlay, so no keyboard shortcut runs
 *   while it is open (none may show Settings above it).
 * - A mandatory overlay (the caller sets `closedby="none"`) cancels Escape
 *   keydowns and re-shows itself if anything closes it. closedby="none"
 *   disables only its own close watcher: a dialog shown without user
 *   activation shares a close-watcher group with the dialog below it, and
 *   Escape then closes THAT one (measured, Chromium 152).
 *
 * The caller renders the <dialog> with the returned ref only while `isOpen`,
 * and a dismissible caller owns its `cancel`/`close` handling.
 */
export function useTopLayerDialog(isOpen: boolean) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const invokerRef = useRef<HTMLElement | null>(null);
  const stackId = useId();
  const { register, unregister } = useModalStack();

  useEffect(() => {
    const dlg = dialogRef.current;
    if (!isOpen || !dlg) return;
    // Capture the focused element before register() inerts the background.
    // Guarded by !dlg.open so StrictMode's effect replay keeps the original.
    if (!dlg.open) {
      invokerRef.current = document.activeElement as HTMLElement | null;
      dlg.showModal();
    }
    dlg.setAttribute(GLOBAL_OVERLAY_ATTRIBUTE, '');
    register(stackId, TOP_LAYER_DEPTH, null);
    const mandatory = dlg.getAttribute('closedby') === 'none';
    const reshow = () => {
      if (dlg.isConnected && !dlg.open) dlg.showModal();
    };
    if (mandatory) {
      dlg.addEventListener('keydown', cancelEscape);
      dlg.addEventListener('close', reshow);
    }
    return () => {
      dlg.removeEventListener('keydown', cancelEscape);
      dlg.removeEventListener('close', reshow);
      unregister(stackId);
      // unregister() has lifted the inert background, so focus returns to the
      // element the user was on. In StrictMode's replay the dialog is still
      // open and modal, so the invoker is inert behind it and this does nothing.
      invokerRef.current?.focus?.();
    };
  }, [isOpen, stackId, register, unregister]);

  return { dialogRef, titleId: `${stackId}-title`, descriptionId: `${stackId}-description` };
}
