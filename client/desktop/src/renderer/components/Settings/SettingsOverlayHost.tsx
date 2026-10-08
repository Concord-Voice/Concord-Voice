import React, { Suspense, lazy, useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  isSettingsOverlayDismissBlocked,
  useSettingsOverlayStore,
} from '../../stores/ui/settingsOverlayStore';
import { ModalPortalHostContext } from '../ui/ModalContext';
import './SettingsOverlayHost.css';
import { keyTargetsForeignModal } from '../../utils/ui/keyTargetsForeignModal';
import { focusTargetIn } from '../ui/focusTarget';

const SettingsPage = lazy(() => import('./SettingsPage'));
const ServerSettingsPage = lazy(() => import('../Servers/ServerSettingsPage'));

// Mirror SyntaxHelpModal's probe: jsdom does not fire native dialog cancel/close
// on Escape, while Electron/Chrome do. Keep the fallback out of production so
// nested settings modals own their Escape handling.
const dialogCancelsOnEscape = (() => {
  if (typeof document === 'undefined') return true;
  return globalThis.navigator !== undefined && !/jsdom/i.test(globalThis.navigator.userAgent ?? '');
})();

/**
 * SettingsOverlayHost
 *
 * Renders the active "settings" surface (app settings or server settings)
 * as a fullscreen native <dialog> portal on top of the
 * persistent chat layout. Mounted once inside AuthenticatedLayout so the
 * underlying MainView / DirectMessagesView tree is never unmounted when
 * settings open.
 *
 * Closes via:
 *  - ESC key (handled natively by <dialog>)
 *  - click on the dimmed backdrop (the dialog's ::backdrop pseudo-element
 *    surfaces clicks on the dialog element itself when the inner panel
 *    stops propagation)
 *  - explicit close() from store (e.g. back button inside the page)
 */
/**
 * Where focus goes when Settings closes and the control that opened it has left
 * the document (a step-up dialog that closed for "Set up verification", a menu
 * that unmounted): the composer, then the channel list, then the server rail.
 * The native dialog returns focus to that control when it can; otherwise it
 * lands on `<body>`, which this replaces.
 */
function chatFocusTarget(): HTMLElement | null {
  for (const selector of ['.message-input-textarea', '.channel-list', '.server-bar']) {
    const target = focusTargetIn(document.querySelector<HTMLElement>(selector));
    if (target !== null) return target;
  }
  return null;
}

const SettingsOverlayHost: React.FC = () => {
  const open = useSettingsOverlayStore((s) => s.open);
  const payload = useSettingsOverlayStore((s) => s.payload);
  const close = useSettingsOverlayStore((s) => s.close);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const [portalHost, setPortalHost] = useState<HTMLDialogElement | null>(null);
  const setDialogRef = useCallback((dialog: HTMLDialogElement | null) => {
    dialogRef.current = dialog;
    setPortalHost(dialog);
  }, []);

  // Drive the native <dialog> open state from the store
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    if (open && !dlg.open) {
      dlg.showModal();
    } else if (!open && dlg.open) {
      dlg.close();
    }
  }, [open]);

  // After every close, whatever path it took (the store, Escape, the backdrop):
  // the native dialog's own focus return has already run by now, so focus left
  // on <body> means its invoker is gone.
  const wasOpenRef = useRef(Boolean(open));
  useEffect(() => {
    const closed = wasOpenRef.current && !open;
    wasOpenRef.current = Boolean(open);
    if (!closed) return;
    const active = document.activeElement;
    if (active === null || active === document.body) chatFocusTarget()?.focus();
  }, [open]);

  // Lock body scroll while open (showModal already inerts the rest of the
  // tree, but this prevents background scroll on platforms that don't honor
  // inert)
  useEffect(() => {
    if (!open) return;
    const prev = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = prev;
    };
  }, [open]);

  useEffect(() => {
    if (!open) return;
    if (dialogCancelsOnEscape) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (keyTargetsForeignModal(e, dialogRef.current)) return;
      if (
        e.key === 'Escape' &&
        !isSettingsOverlayDismissBlocked(useSettingsOverlayStore.getState().open)
      ) {
        close();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => document.removeEventListener('keydown', handleKeyDown);
  }, [open, close]);

  // Native <dialog> fires a 'close' event on ESC; bridge it to the store.
  // Backdrop clicks land on the dialog element itself (not on the inner
  // panel), so we close on click when target === dialog. Listeners are
  // attached imperatively rather than via JSX props.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    const dismissBlocked = () =>
      isSettingsOverlayDismissBlocked(useSettingsOverlayStore.getState().open);
    const handleCancel = (e: Event) => {
      if (dismissBlocked()) e.preventDefault();
    };
    const handleClose = () => {
      if (dismissBlocked()) {
        dlg.showModal();
        return;
      }
      if (useSettingsOverlayStore.getState().open) close();
    };
    const handleClick = (e: MouseEvent) => {
      if (e.target === dlg && !dismissBlocked()) close();
    };
    dlg.addEventListener('cancel', handleCancel);
    dlg.addEventListener('close', handleClose);
    dlg.addEventListener('click', handleClick);
    return () => {
      dlg.removeEventListener('cancel', handleCancel);
      dlg.removeEventListener('close', handleClose);
      dlg.removeEventListener('click', handleClick);
    };
  }, [open, close]);

  let content: React.ReactNode = null;
  if (open === 'app') {
    content = <SettingsPage />;
  } else if (open === 'server' && payload?.serverId) {
    content = <ServerSettingsPage serverId={payload.serverId} />;
  }

  return createPortal(
    <dialog
      ref={setDialogRef}
      className="settings-overlay-host"
      aria-label="Settings"
      data-modal-portal-host="true"
    >
      {/* eslint-disable-next-line @eslint-react/no-context-provider -- Context.Provider keeps this compatible with the repository's current React context style */}
      <ModalPortalHostContext.Provider value={portalHost}>
        <div className="settings-overlay-host__panel">
          <Suspense fallback={null}>{open && portalHost ? content : null}</Suspense>
        </div>
      </ModalPortalHostContext.Provider>
    </dialog>,
    document.body
  );
};

export default SettingsOverlayHost;
