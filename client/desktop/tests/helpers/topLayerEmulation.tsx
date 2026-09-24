import React, { useCallback, useEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { beforeEach, afterEach, vi } from 'vitest';
import { ModalPortalHostContext } from '@/renderer/components/ui/ModalContext';

// ── jsdom gap emulation ──────────────────────────────────────────────────
// jsdom implements neither the `inert` attribute's focus-blocking behavior
// nor the browser's top-layer / showModal() modality. Both are load-bearing
// for the "global overlay reachable over Settings" regression family: Settings
// opens a native <dialog> with showModal(), and ModalContext's syncInert sets
// `inert` on #root and on non-topmost modal overlays. Without emulating both
// rules here, every focus/Tab/Escape assertion in a consuming test would
// trivially pass regardless of whether the production bug is present.
//
// Extracted from tests/unit/components/Auth/mfaChallengeBehindModal.test.tsx
// (see [internal]rules/frontend.md § "A global overlay must be reachable over
// the Settings dialog") so every overlay of this kind can reuse one emulation.
let dialogStack: HTMLDialogElement[];
let originalShowModal: typeof HTMLDialogElement.prototype.showModal;
let originalFocus: typeof HTMLElement.prototype.focus;

/** The topmost open, connected dialog in the emulated top layer. */
export function topDialog(): HTMLDialogElement | undefined {
  for (let i = dialogStack.length - 1; i >= 0; i--) {
    const d = dialogStack[i];
    if (d.isConnected && d.open) return d;
  }
  return undefined;
}

// (c) MEASURED 2026-09-23 on Electron 44.4.3 / Chromium 152.0.7977.130: a
// showModal()-opened <dialog> escapes an `inert` ANCESTOR of the dialog — only
// an `inert` attribute found between the focus target and its nearest MODAL
// <dialog> (or on that dialog itself) blocks focus. Only a modal dialog
// escapes: an open non-modal one (show(), or the open attribute) stays inert,
// per the HTML spec. `:modal` matches a showModal() dialog (tests/setup.ts).
function nearestModalDialog(el: HTMLElement): Element | null {
  let dialog = el.closest('dialog');
  while (dialog && !dialog.matches(':modal'))
    dialog = dialog.parentElement?.closest('dialog') ?? null;
  return dialog;
}

function nearestInertBoundaryBlocks(el: HTMLElement): boolean {
  const dialog = nearestModalDialog(el);
  if (!dialog) {
    // No enclosing modal dialog — an inert attribute anywhere up the chain
    // (including on #root) blocks focus, same as a real inert ancestor.
    return !!el.closest('[inert]');
  }
  // Only walk from the element up to (and including) its nearest modal
  // dialog. `inert` above that boundary (e.g. on #root) does not apply —
  // that is exactly what lets a modal dialog escape an inert ancestor.
  let node: HTMLElement | null = el;
  while (node) {
    if (node.hasAttribute('inert')) return true;
    if (node === dialog) return false;
    node = node.parentElement;
  }
  return false;
}

const FOCUSABLE = [
  'a[href]',
  'button:not([disabled])',
  'input:not([disabled])',
  'select:not([disabled])',
  'textarea:not([disabled])',
  '[tabindex]:not([tabindex="-1"])',
].join(', ');

function isFocusBlocked(el: HTMLElement): boolean {
  // (a)/(c) inert-boundary rule, dialog-aware.
  if (nearestInertBoundaryBlocks(el)) return true;
  // (a2) a closed <dialog> is not rendered, so nothing inside it can take
  // focus (React's autoFocus fires before the effect that opens the dialog).
  if (el.closest('dialog:not([open])')) return true;
  // (b) while any showModal()-opened <dialog> is open and connected, every
  // element NOT inside the most-recently-shown such dialog is blocked.
  const top = topDialog();
  if (top && !top.contains(el)) return true;
  return false;
}

/**
 * Installs the showModal()/focus jsdom-gap emulation via beforeEach/afterEach.
 * Call once at the top level of a test file (module scope), mirroring how the
 * founding MFA regression test registered these hooks directly.
 */
export function installTopLayerEmulation(): void {
  beforeEach(() => {
    dialogStack = [];
    originalShowModal = HTMLDialogElement.prototype.showModal;
    originalFocus = HTMLElement.prototype.focus;

    // A showModal() adds the dialog at the TOP of the top layer, including one
    // that was in it and closed since (HTML "add an element to the top layer").
    // On a dialog that is already modal it does nothing.
    vi.spyOn(HTMLDialogElement.prototype, 'showModal').mockImplementation(function (
      this: HTMLDialogElement
    ) {
      if (this.open && this.matches(':modal')) return;
      originalShowModal.call(this);
      const i = dialogStack.indexOf(this);
      if (i !== -1) dialogStack.splice(i, 1);
      dialogStack.push(this);
      // The dialog focusing steps: focus the first focusable control.
      this.querySelector<HTMLElement>(FOCUSABLE)?.focus();
    });
    // Chromium QUEUES the close event (HTML "close the dialog"); the setup.ts
    // polyfill fires it synchronously, which no browser does. A dialog that is
    // already closed fires nothing.
    vi.spyOn(HTMLDialogElement.prototype, 'close').mockImplementation(function (
      this: HTMLDialogElement
    ) {
      if (!this.open) return;
      this.removeAttribute('open');
      queueMicrotask(() => this.dispatchEvent(new Event('close')));
    });

    vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (
      this: HTMLElement,
      ...args: Parameters<typeof HTMLElement.prototype.focus>
    ) {
      if (isFocusBlocked(this)) return;
      originalFocus.apply(this, args);
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });
}

// ── #root harness ────────────────────────────────────────────────────────
// ModalContext's syncInert targets document.getElementById('root'), which
// test-utils' default RTL container does not provide.
/**
 * Installs a fresh `<div id="root">` in `document.body` for each test via
 * beforeEach/afterEach, and returns a getter for the current element. Call
 * once at the top level of a test file, alongside `installTopLayerEmulation`.
 */
export function installRootHarness(): () => HTMLDivElement {
  let root: HTMLDivElement;

  beforeEach(() => {
    root = document.createElement('div');
    root.id = 'root';
    document.body.appendChild(root);
  });

  afterEach(() => {
    root.remove();
  });

  return () => root;
}

// ── SettingsStandIn for SettingsOverlayHost ─────────────────────────────
// A minimal native-<dialog> host that mirrors SettingsOverlayHost's shape
// (callback ref -> useState -> ModalPortalHostContext), WITHOUT its
// jsdom-only Escape fallback (SettingsOverlayHost only installs a manual
// Escape listener when `dialogCancelsOnEscape` is false, which would muddy
// the Escape assertion this test cares about — the real
// showModal()-opened <dialog> already owns Escape in a real browser).
export function SettingsStandIn({ children }: { children: React.ReactNode }) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);
  const [portalHost, setPortalHost] = useState<HTMLDialogElement | null>(null);
  const setDialogRef = useCallback((dialog: HTMLDialogElement | null) => {
    dialogRef.current = dialog;
    setPortalHost(dialog);
  }, []);

  useEffect(() => {
    const dlg = dialogRef.current;
    if (dlg && !dlg.open) dlg.showModal();
  }, [portalHost]);

  return createPortal(
    <dialog ref={setDialogRef} aria-label="Settings">
      {/* eslint-disable-next-line @eslint-react/no-context-provider -- test stand-in mirrors SettingsOverlayHost's own provider usage */}
      <ModalPortalHostContext.Provider value={portalHost}>
        {portalHost ? children : null}
      </ModalPortalHostContext.Provider>
    </dialog>,
    document.body
  );
}

/**
 * A raw showModal() <dialog> rendered as an ordinary (non-portaled) child, so
 * it can be nested directly inside an `inert` ancestor such as #root. Used to
 * prove fact (c): a modal dialog escapes an inert ancestor. Not exported as
 * part of the MFA regression shape — new to this helper.
 */
export function DialogInsideRoot({
  label,
  buttonLabel,
  leadingButtonLabel,
}: {
  label: string;
  buttonLabel: string;
  /** A control before the target, so the focusing steps land somewhere else. */
  leadingButtonLabel?: string;
}) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);

  useEffect(() => {
    const dlg = dialogRef.current;
    if (dlg && !dlg.open) dlg.showModal();
  }, []);

  return (
    <dialog ref={dialogRef} aria-label={label}>
      {leadingButtonLabel && <button type="button">{leadingButtonLabel}</button>}
      <button type="button">{buttonLabel}</button>
    </dialog>
  );
}
