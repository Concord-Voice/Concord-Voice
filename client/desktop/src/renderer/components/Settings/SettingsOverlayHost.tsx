import React, { Suspense, lazy, useEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { useSettingsOverlayStore } from '../../stores/settingsOverlayStore';
import './SettingsOverlayHost.css';

const SettingsPage = lazy(() => import('./SettingsPage'));
const ProfilePage = lazy(() => import('../Profile/ProfilePage'));
const ServerSettingsPage = lazy(() => import('../Servers/ServerSettingsPage'));

/**
 * SettingsOverlayHost
 *
 * Renders the active "settings" surface (app settings, profile editor, or
 * server settings) as a fullscreen native <dialog> portal on top of the
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
const SettingsOverlayHost: React.FC = () => {
  const open = useSettingsOverlayStore((s) => s.open);
  const payload = useSettingsOverlayStore((s) => s.payload);
  const close = useSettingsOverlayStore((s) => s.close);
  const dialogRef = useRef<HTMLDialogElement>(null);

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

  // Native <dialog> fires a 'close' event on ESC; bridge it to the store.
  // Backdrop clicks land on the dialog element itself (not on the inner
  // panel), so we close on click when target === dialog. Listeners are
  // attached imperatively rather than via JSX props.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    const handleClose = () => {
      if (open) close();
    };
    const handleClick = (e: MouseEvent) => {
      if (e.target === dlg) close();
    };
    dlg.addEventListener('close', handleClose);
    dlg.addEventListener('click', handleClick);
    return () => {
      dlg.removeEventListener('close', handleClose);
      dlg.removeEventListener('click', handleClick);
    };
  }, [open, close]);

  let content: React.ReactNode = null;
  if (open === 'app') {
    content = <SettingsPage />;
  } else if (open === 'profile') {
    content = <ProfilePage />;
  } else if (open === 'server' && payload?.serverId) {
    content = <ServerSettingsPage serverId={payload.serverId} />;
  }

  return createPortal(
    <dialog ref={dialogRef} className="settings-overlay-host" aria-label="Settings">
      <div className="settings-overlay-host__panel">
        <Suspense fallback={null}>{open ? content : null}</Suspense>
      </div>
    </dialog>,
    document.body
  );
};

export default SettingsOverlayHost;
