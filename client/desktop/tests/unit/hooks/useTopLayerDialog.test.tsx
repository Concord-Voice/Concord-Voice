import React from 'react';
import { render, screen, fireEvent } from '../../test-utils';
// test-utils' provider wrapper suppresses the StrictMode effect replay, so the
// replay case renders bare and supplies ModalProvider itself.
import { render as bareRender } from '@testing-library/react';
import { vi } from 'vitest';
import Modal from '@/renderer/components/ui/Modal';
import { ModalProvider } from '@/renderer/components/ui/ModalContext';
import { useTopLayerDialog } from '@/renderer/hooks/ui/useTopLayerDialog';
import { keyboardShortcutService } from '@/renderer/services/system/keyboardShortcutService';
import { installTopLayerEmulation, installRootHarness } from '../../helpers/topLayerEmulation';
import { resetAllStores } from '../../helpers/store-helpers';

// The emulation makes focus honour `inert` and the top layer, so these cases
// observe the hook's ORDERING (unregister lifts #root's inert before focus is
// restored), which plain jsdom would let pass either way.
installTopLayerEmulation();
const getRoot = installRootHarness();

function Overlay({ open }: Readonly<{ open: boolean }>) {
  const { dialogRef, titleId } = useTopLayerDialog(open);
  if (!open) return null;
  return (
    <dialog ref={dialogRef} aria-labelledby={titleId}>
      <h2 id={titleId}>Overlay</h2>
      <button type="button">Inside</button>
    </dialog>
  );
}

function MandatoryOverlay() {
  const { dialogRef, titleId } = useTopLayerDialog(true);
  return (
    <dialog ref={dialogRef} aria-labelledby={titleId} closedby="none">
      <h2 id={titleId}>Mandatory</h2>
      <button type="button">Inside</button>
    </dialog>
  );
}

function Scene({ open, onModalClose }: Readonly<{ open: boolean; onModalClose: () => void }>) {
  return (
    <>
      <button type="button">Invoker</button>
      <Modal isOpen onClose={onModalClose} title="Underneath">
        <button type="button">Modal button</button>
      </Modal>
      <Overlay open={open} />
    </>
  );
}

describe('useTopLayerDialog', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('returns focus to the element that had it once the overlay unmounts', () => {
    const { rerender } = render(<button type="button">Invoker</button>, {
      container: getRoot(),
    });
    const invoker = screen.getByRole('button', { name: 'Invoker' });
    invoker.focus();
    expect(document.activeElement, 'precondition: the invoker holds focus').toBe(invoker);

    rerender(
      <>
        <button type="button">Invoker</button>
        <Overlay open />
      </>
    );
    screen.getByRole('button', { name: 'Inside' }).focus();
    expect(document.activeElement, 'focus moved into the open overlay').not.toBe(invoker);

    rerender(
      <>
        <button type="button">Invoker</button>
        <Overlay open={false} />
      </>
    );
    expect(document.activeElement, 'closing the overlay must return focus to its invoker').toBe(
      invoker
    );
  });

  it('keeps the overlay topmost across a StrictMode effect replay, calling showModal once', () => {
    const showModal = vi.spyOn(HTMLDialogElement.prototype, 'showModal');
    const onModalClose = vi.fn();
    bareRender(
      <React.StrictMode>
        <ModalProvider>
          <Scene open onModalClose={onModalClose} />
        </ModalProvider>
      </React.StrictMode>,
      { container: getRoot() }
    );

    const overlayCalls = showModal.mock.contexts.filter(
      (dlg) => (dlg as HTMLDialogElement).getAttribute('aria-labelledby') !== null
    );
    expect(overlayCalls.length, 'the replayed effect must not call showModal twice').toBe(1);

    fireEvent.keyDown(screen.getByRole('button', { name: 'Inside' }), { key: 'Escape' });
    expect(
      onModalClose,
      'after the replay the overlay is still registered topmost, so the ui/Modal ignores Escape'
    ).not.toHaveBeenCalled();
  });

  // A shortcut run from inside an open overlay either shows a dialog over it
  // (Ctrl/Cmd+, opens Settings, which buries the overlay in the top layer) or
  // cancels the Escape keydown the overlay's own dialog needs. Real service,
  // real default shortcuts; the handler is the outermost effect a shortcut has.
  it.each([
    ['open-settings', ',', { ctrlKey: true }],
    ['close-modal', 'Escape', {}],
  ] as const)(
    'runs no %s shortcut while the overlay is open, and runs it once the overlay closes',
    (id, key, modifiers) => {
      const handler = vi.fn();
      keyboardShortcutService.init();
      keyboardShortcutService.registerHandler(id, handler);
      const press = () => {
        const event = new KeyboardEvent('keydown', {
          key,
          ...modifiers,
          bubbles: true,
          cancelable: true,
        });
        document.dispatchEvent(event);
        return event;
      };
      try {
        const { rerender } = render(<Overlay open />, { container: getRoot() });
        const event = press();
        expect(handler, `${id} must not run behind an open overlay`).not.toHaveBeenCalled();
        expect(event.defaultPrevented, 'the keydown must reach the dialog uncancelled').toBe(false);

        // Control: the same shortcut runs once the overlay is gone.
        rerender(<Overlay open={false} />);
        press();
        expect(handler).toHaveBeenCalledOnce();
      } finally {
        keyboardShortcutService.destroy();
      }
    }
  );

  // A user in a call must still be able to mute while an update or
  // reconnection overlay is up: mute and deafen open no dialog and never use
  // Escape, so the gate above exempts them.
  it.each([
    ['toggle-mute', 'm'],
    ['toggle-deafen', 'd'],
  ] as const)('still runs %s while the overlay is open', (id, key) => {
    const handler = vi.fn();
    keyboardShortcutService.init();
    keyboardShortcutService.registerHandler(id, handler);
    try {
      render(<Overlay open />, { container: getRoot() });
      document.dispatchEvent(
        new KeyboardEvent('keydown', {
          key,
          ctrlKey: true,
          shiftKey: true,
          bubbles: true,
          cancelable: true,
        })
      );
      expect(handler).toHaveBeenCalledOnce();
    } finally {
      keyboardShortcutService.destroy();
    }
  });

  // A dialog shown without user activation joins the close-watcher group of the
  // one below it, and closedby="none" disables only its own watcher, so Chromium
  // passes Escape through and closes the dialog BELOW (measured, see
  // scripts/dialog-top-layer-probe). Cancelling the keydown stops the request.
  it.each([
    ['a mandatory (closedby="none") overlay cancels', true],
    ['a dismissible overlay leaves', false],
  ] as const)('%s the Escape keydown inside it', (_label, mandatory) => {
    render(mandatory ? <MandatoryOverlay /> : <Overlay open />, { container: getRoot() });
    const notCancelled = fireEvent.keyDown(screen.getByRole('button', { name: 'Inside' }), {
      key: 'Escape',
    });
    expect(notCancelled).toBe(!mandatory);
  });

  // Rule 4: a closed dialog whose state still says open is the deadlock again.
  // Nothing may close a mandatory overlay, so if something does, it re-shows.
  it.each([
    ['re-shows a mandatory overlay', true],
    ['leaves a dismissible overlay closed', false],
  ] as const)('%s that something closed', async (_label, mandatory) => {
    render(mandatory ? <MandatoryOverlay /> : <Overlay open />, { container: getRoot() });
    const dialog = screen.getByRole('button', { name: 'Inside' }).closest('dialog');
    dialog?.close();
    await Promise.resolve(); // Chromium queues the close event.
    expect(dialog?.open).toBe(mandatory);
  });

  // TOP_LAYER_DEPTH: a ui/Modal opened AFTER the overlay (a deep link, say) sits
  // below the top layer, so it must not become topmost and take Escape.
  it('keeps a ui/Modal opened after the overlay from taking its Escape', () => {
    const onModalClose = vi.fn();
    const { rerender } = render(<Overlay open />, { container: getRoot() });
    rerender(
      <>
        <Overlay open />
        <Modal isOpen onClose={onModalClose} title="Opened later">
          <button type="button">Later button</button>
        </Modal>
      </>
    );
    fireEvent.keyDown(screen.getByRole('button', { name: 'Inside' }), { key: 'Escape' });
    expect(onModalClose).not.toHaveBeenCalled();
  });

  // Positive control for the Escape assertion above: without it, a ui/Modal
  // whose Escape handler never fires would make "not called" pass vacuously.
  it('positive control: with no overlay mounted, Escape reaches the ui/Modal', () => {
    const onModalClose = vi.fn();
    render(<Scene open={false} onModalClose={onModalClose} />, { container: getRoot() });

    fireEvent.keyDown(screen.getByRole('button', { name: 'Modal button' }), { key: 'Escape' });
    expect(onModalClose).toHaveBeenCalledTimes(1);
  });
});
