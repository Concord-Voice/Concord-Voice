import { useEffect, useRef, type MouseEvent } from 'react';
import {
  useAttestationFailureStore,
  type TerminalAttestationCode,
} from '../stores/attestationFailureStore';
import './AttestationFailedModal.css';

/**
 * Defense-in-depth URL allowlist — mirrors SafeLink.tsx's SAFE_PROTOCOLS.
 * downloadHelpUrl is server-supplied and untrusted; we validate before
 * rendering it as an href or passing to window.electron.openExternal.
 * Only http, https, and mailto are considered safe.
 */
const SAFE_PROTOCOLS = /^(https?|mailto):/i;

/**
 * Narrow a value to a Promise-like type without leaning on `as` assertions.
 * Mirrors the isPromiseLike guard in SafeLink.tsx.
 */
function isPromiseLike(value: unknown): value is Promise<unknown> {
  if (value === null || typeof value !== 'object') return false;
  if (!('catch' in value)) return false;
  return typeof value.catch === 'function';
}

// ── Presentational component ───────────────────────────────────────────────

export interface AttestationFailedModalProps {
  code: TerminalAttestationCode;
  requiredMinVersion?: string;
  downloadHelpUrl?: string;
  onDismiss: () => void;
}

/**
 * AttestationFailedModal — presentational, props-driven, no store coupling.
 *
 * Renders the "Update Required" dialog when the server rejects the client
 * with an attestation 403 (ATTESTATION_UNKNOWN_RELEASE, ATTESTATION_REVOKED,
 * CLIENT_VERSION_TOO_OLD). BSL clarity copy: users of self-hosted servers are
 * not blocked; only example.com requires official signed releases.
 *
 * SECURITY: downloadHelpUrl is server-supplied and therefore untrusted.
 * Scheme is validated against SAFE_PROTOCOLS before rendering or passing to
 * window.electron.openExternal. An unsafe or missing URL renders no link at all.
 */
export function AttestationFailedModal({
  code,
  requiredMinVersion,
  downloadHelpUrl,
  onDismiss,
}: Readonly<AttestationFailedModalProps>) {
  const isSafeUrl = downloadHelpUrl !== undefined && SAFE_PROTOCOLS.test(downloadHelpUrl);
  const dialogRef = useRef<HTMLDialogElement>(null);

  // Open the native <dialog> imperatively. showModal() gives us native modal
  // behavior: focus trap, ::backdrop dimming, Escape-to-close. Mirrors the
  // SettingsOverlayHost / DMProfileModal pattern in this codebase.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg || dlg.open) return;
    dlg.showModal();
  }, []);

  // Native <dialog> fires a 'close' event on Escape (and on dlg.close()).
  // Bridge it to onDismiss so the host component can update the store.
  // Listener attached imperatively rather than via JSX so the jsx-a11y rule
  // "non-interactive elements should not be assigned mouse or keyboard event
  // listeners" doesn't fire on the <dialog> JSX node.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    const handleClose = () => onDismiss();
    dlg.addEventListener('close', handleClose);
    return () => {
      dlg.removeEventListener('close', handleClose);
    };
  }, [onDismiss]);

  const handleDownloadClick = (e: MouseEvent<HTMLAnchorElement>): void => {
    // Type-narrowing guard, not a runtime branch: the anchor only mounts when
    // `isSafeUrl && downloadHelpUrl`, so this never fires at runtime — but it
    // narrows `string | undefined` to `string` for the openExternal call below.
    if (!downloadHelpUrl) return;
    // Route through the preload bridge so the OS browser opens the URL.
    // Guard defensively — the bridge may be absent in tests or future environments.
    const api = (
      globalThis as unknown as {
        electron?: { openExternal?: (url: string) => Promise<unknown> | void };
      }
    ).electron;
    if (api && typeof api.openExternal === 'function') {
      e.preventDefault();
      const result: Promise<unknown> | void = api.openExternal(downloadHelpUrl);
      if (isPromiseLike(result)) {
        result.catch(() => {
          /* main-process logged the failure; renderer treats as no-op */
        });
      }
    }
    // If bridge absent, default anchor activation fires; Electron's main-process
    // setWindowOpenHandler re-validates and routes to shell.openExternal.
  };

  return (
    <div className="attestation-modal-overlay">
      <dialog
        ref={dialogRef}
        aria-modal="true"
        aria-labelledby="attestation-modal-title"
        className="attestation-modal"
        data-attestation-code={code}
      >
        <h2 id="attestation-modal-title">Update Required</h2>
        <p>
          example.com requires an official Concord Voice client. Self-hosted servers may
          accept your build, but this server only accepts official signed releases.
        </p>
        {requiredMinVersion && (
          <p>
            Required minimum version: <strong>{requiredMinVersion}</strong>
          </p>
        )}
        <div className="attestation-modal__actions">
          {isSafeUrl && downloadHelpUrl && (
            <a
              href={downloadHelpUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="attestation-modal__download-link"
              onClick={handleDownloadClick}
            >
              Download Official Client
            </a>
          )}
          <button type="button" className="btn btn-secondary" onClick={onDismiss}>
            Dismiss
          </button>
        </div>
      </dialog>
    </div>
  );
}

// ── Store-connected host (default export) ──────────────────────────────────

/**
 * AttestationFailedModalHost — wires the presentational modal to the
 * attestationFailureStore. Mount this once in App.tsx alongside other
 * global overlay components (ForceUpdateOverlay, MFAChallengeModal, etc.).
 *
 * Follows the selective-subscription pattern from MFAChallengeModal:
 * each slice of store state is subscribed individually so React only
 * re-renders on relevant changes.
 */
export default function AttestationFailedModalHost() {
  const visible = useAttestationFailureStore((s) => s.visible);
  const code = useAttestationFailureStore((s) => s.code);
  const requiredMinVersion = useAttestationFailureStore((s) => s.requiredMinVersion);
  const downloadHelpUrl = useAttestationFailureStore((s) => s.downloadHelpUrl);
  const dismiss = useAttestationFailureStore((s) => s.dismiss);

  // Early return AFTER all hooks (React rules of hooks requirement)
  if (!visible || !code) return null;

  return (
    <AttestationFailedModal
      code={code}
      requiredMinVersion={requiredMinVersion}
      downloadHelpUrl={downloadHelpUrl}
      onDismiss={dismiss}
    />
  );
}
