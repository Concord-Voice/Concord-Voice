import { render, screen, userEvent } from '../../../test-utils';
import { fireEvent } from '@testing-library/react';
import Modal from '@/renderer/components/ui/Modal';
import { ModalPortalHostContext } from '@/renderer/components/ui/ModalContext';
import { useRef, useState } from 'react';
import { vi } from 'vitest';

describe('Modal', () => {
  const onClose = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders when open', () => {
    render(
      <Modal isOpen={true} onClose={onClose} title="Test Modal">
        <p>Modal content</p>
      </Modal>
    );
    expect(screen.getByText('Test Modal')).toBeInTheDocument();
    expect(screen.getByText('Modal content')).toBeInTheDocument();
  });

  it('does not render when closed', () => {
    render(
      <Modal isOpen={false} onClose={onClose} title="Test Modal">
        <p>Modal content</p>
      </Modal>
    );
    expect(screen.queryByText('Test Modal')).not.toBeInTheDocument();
  });

  it('calls onClose when close button is clicked', async () => {
    const user = userEvent.setup();
    render(
      <Modal isOpen={true} onClose={onClose} title="Test Modal">
        <p>Modal content</p>
      </Modal>
    );
    await user.click(screen.getByLabelText('Close'));
    expect(onClose).toHaveBeenCalled();
  });

  it('calls onClose on Escape key', async () => {
    const user = userEvent.setup();
    render(
      <Modal isOpen={true} onClose={onClose} title="Test Modal">
        <p>Modal content</p>
      </Modal>
    );
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalled();
  });

  it('calls onClose when overlay is clicked', async () => {
    const user = userEvent.setup();
    render(
      <Modal isOpen={true} onClose={onClose} title="Test Modal">
        <p>Modal content</p>
      </Modal>
    );
    const overlay = document.querySelector('.modal-overlay')!;
    await user.click(overlay);
    expect(onClose).toHaveBeenCalled();
  });

  it('applies width class', () => {
    render(
      <Modal isOpen={true} onClose={onClose} title="Test Modal" width="small">
        <p>Small modal</p>
      </Modal>
    );
    const container = document.querySelector('.modal-container');
    expect(container?.classList.contains('modal-small')).toBe(true);
  });

  // #158 — the feedback modal uses the new 660px xlarge variant. Lock the
  // class name as a contract test so a future rename of the size token can't
  // silently change the visual width.
  it('applies xlarge width class for the feedback modal size', () => {
    render(
      <Modal isOpen={true} onClose={onClose} title="Feedback" width="xlarge">
        <p>Feedback modal</p>
      </Modal>
    );
    const container = document.querySelector('.modal-container');
    expect(container?.classList.contains('modal-xlarge')).toBe(true);
  });

  describe('dialog semantics (a11y — #2087)', () => {
    it('exposes role=dialog, aria-modal, and an accessible name from the title', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Semantics">
          <p>Body</p>
        </Modal>
      );
      // getByRole('dialog', { name }) asserts role + aria-labelledby → title text in one shot.
      const dialog = screen.getByRole('dialog', { name: 'Semantics' });
      expect(dialog).toHaveAttribute('aria-modal', 'true');
      expect(dialog).toHaveClass('modal-container');
    });
  });

  describe('focus management (a11y — #2087)', () => {
    it('moves initial focus to an explicitly supplied disclosure heading', () => {
      function Harness() {
        const disclosureHeadingRef = useRef<HTMLHeadingElement>(null);
        return (
          <Modal
            isOpen={true}
            onClose={onClose}
            title="Review Activity History terms"
            initialFocusRef={disclosureHeadingRef}
          >
            <h4 ref={disclosureHeadingRef} tabIndex={-1}>
              Activity History disclosure
            </h4>
            <button>Continue</button>
          </Modal>
        );
      }

      render(<Harness />);

      expect(screen.getByRole('heading', { name: 'Activity History disclosure' })).toHaveFocus();
    });

    it('moves focus into the dialog on open', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Focus Me">
          <button>Inside</button>
        </Modal>
      );
      // The container itself is the focus target (tabIndex=-1) so the SR announces the dialog + label.
      expect(screen.getByRole('dialog', { name: 'Focus Me' })).toHaveFocus();
    });

    it('returns focus to the invoking element when the modal closes (invoker inside inert #root)', async () => {
      // Regression (#2087): the invoker lives inside #root, which is inert while
      // the modal is open. Focus restoration on close must happen AFTER #root's
      // inert is cleared — otherwise .focus() on the (still-inert) invoker is a
      // no-op and focus is stranded on <body>. Rendering into #root is what makes
      // this test actually exercise the inert path.
      const root = document.createElement('div');
      root.id = 'root';
      document.body.appendChild(root);
      const user = userEvent.setup();
      function Harness() {
        const [open, setOpen] = useState(false);
        return (
          <>
            <button onClick={() => setOpen(true)}>Open</button>
            <Modal isOpen={open} onClose={() => setOpen(false)} title="Roundtrip">
              <p>Body</p>
            </Modal>
          </>
        );
      }
      render(<Harness />, { container: root });
      const openBtn = screen.getByRole('button', { name: 'Open' });
      await user.click(openBtn); // focuses openBtn, then opens the modal
      expect(screen.getByRole('dialog', { name: 'Roundtrip' })).toHaveFocus();

      await user.keyboard('{Escape}'); // closes via the existing ESC path
      expect(openBtn).toHaveFocus();
      root.remove();
    });

    it('captures the invoker before #root becomes inert so focus returns on close', async () => {
      // jsdom does not implement inert's focus-blur, so emulate it: when #root
      // gains `inert` while it contains the focused element, blur that element to
      // <body>, exactly as a real browser does. This exercises the open-time
      // ordering guarantee that the plain #root test above cannot: the invoker
      // must be captured BEFORE register() makes #root inert. Capturing after
      // would record <body> and strand focus there on close. WCAG 2.1 SC 2.4.3.
      const user = userEvent.setup();
      const root = document.createElement('div');
      root.id = 'root';
      const setAttr = root.setAttribute.bind(root);
      root.setAttribute = (name: string, value: string) => {
        setAttr(name, value);
        const active = document.activeElement;
        if (name === 'inert' && active && active !== document.body && root.contains(active)) {
          (active as HTMLElement).blur();
        }
      };
      document.body.appendChild(root);
      try {
        function Harness() {
          const [open, setOpen] = useState(false);
          return (
            <>
              <button onClick={() => setOpen(true)}>Open</button>
              <Modal isOpen={open} onClose={() => setOpen(false)} title="Roundtrip">
                <p>Body</p>
              </Modal>
            </>
          );
        }
        render(<Harness />, { container: root });
        const openBtn = screen.getByRole('button', { name: 'Open' });

        // Opening makes #root inert, which (via the emulation) blurs openBtn to
        // <body>. The fix captured openBtn first, so restoration still works.
        await user.click(openBtn);
        expect(screen.getByRole('dialog', { name: 'Roundtrip' })).toHaveFocus();

        await user.keyboard('{Escape}');
        expect(openBtn).toHaveFocus(); // restored to the real invoker, not <body>
      } finally {
        root.remove();
      }
    });
  });

  describe('focus trap (a11y — #2087)', () => {
    it('Tab on the last focusable wraps to the first', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Trap" dismissable={false}>
          <button>First</button>
          <button>Last</button>
        </Modal>
      );
      const first = screen.getByRole('button', { name: 'First' });
      const last = screen.getByRole('button', { name: 'Last' });
      last.focus();
      fireEvent.keyDown(last, { key: 'Tab' });
      expect(first).toHaveFocus();
    });

    it('Shift+Tab on the first focusable wraps to the last', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Trap" dismissable={false}>
          <button>First</button>
          <button>Last</button>
        </Modal>
      );
      const first = screen.getByRole('button', { name: 'First' });
      const last = screen.getByRole('button', { name: 'Last' });
      first.focus();
      fireEvent.keyDown(first, { key: 'Tab', shiftKey: true });
      expect(last).toHaveFocus();
    });

    it('only the topmost modal traps Tab (nested)', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Outer" dismissable={false}>
          <button>OuterBtn</button>
          <Modal isOpen={true} onClose={onClose} title="Inner" dismissable={false}>
            <button>InnerFirst</button>
            <button>InnerLast</button>
          </Modal>
        </Modal>
      );
      const innerFirst = screen.getByRole('button', { name: 'InnerFirst' });
      const innerLast = screen.getByRole('button', { name: 'InnerLast' });
      innerLast.focus();
      fireEvent.keyDown(innerLast, { key: 'Tab' });
      // Inner is topmost → focus wraps within the inner ring, never escapes to OuterBtn.
      expect(innerFirst).toHaveFocus();
    });

    it('keeps focus on the container when there are no focusable children', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Empty" dismissable={false}>
          <p>No focusable children</p>
        </Modal>
      );
      const dialog = screen.getByRole('dialog', { name: 'Empty' });
      // Container is focused on open; Tab with an empty focusable ring keeps it there.
      fireEvent.keyDown(dialog, { key: 'Tab' });
      expect(dialog).toHaveFocus();
    });
  });

  describe('dismissable prop', () => {
    it.each([
      { dismissable: true, expectedCloseCalls: 1 },
      { dismissable: false, expectedCloseCalls: 0 },
    ])(
      'consumes Escape above a native Settings host when dismissable=$dismissable',
      ({ dismissable, expectedCloseCalls }) => {
        const settingsHost = document.createElement('dialog');
        settingsHost.setAttribute('open', '');
        settingsHost.dataset.modalPortalHost = 'true';
        document.body.appendChild(settingsHost);
        const outerEscape = vi.fn();
        document.addEventListener('keydown', outerEscape);

        try {
          render(
            <Modal isOpen={true} onClose={onClose} title="Settings child" dismissable={dismissable}>
              <p>Body</p>
            </Modal>
          );
          const event = new KeyboardEvent('keydown', {
            key: 'Escape',
            bubbles: true,
            cancelable: true,
          });

          document.dispatchEvent(event);

          expect(event.defaultPrevented).toBe(true);
          expect(outerEscape).not.toHaveBeenCalled();
          expect(onClose).toHaveBeenCalledTimes(expectedCloseCalls);
        } finally {
          document.removeEventListener('keydown', outerEscape);
          settingsHost.remove();
        }
      }
    );

    it('dismissable={false} suppresses the close button', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Test Modal" dismissable={false}>
          <p>Modal content</p>
        </Modal>
      );
      expect(screen.queryByLabelText('Close')).not.toBeInTheDocument();
    });

    it('dismissable={false} blocks Escape key from calling onClose', async () => {
      const user = userEvent.setup();
      render(
        <Modal isOpen={true} onClose={onClose} title="Test Modal" dismissable={false}>
          <p>Modal content</p>
        </Modal>
      );
      await user.keyboard('{Escape}');
      expect(onClose).not.toHaveBeenCalled();
    });

    it('dismissable={false} blocks overlay click from calling onClose', async () => {
      const user = userEvent.setup();
      render(
        <Modal isOpen={true} onClose={onClose} title="Test Modal" dismissable={false}>
          <p>Modal content</p>
        </Modal>
      );
      const overlay = document.querySelector('.modal-overlay')!;
      await user.click(overlay);
      expect(onClose).not.toHaveBeenCalled();
    });

    it('default (no dismissable prop) still renders the close button', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Test Modal">
          <p>Modal content</p>
        </Modal>
      );
      expect(screen.getByLabelText('Close')).toBeInTheDocument();
    });

    it('default (no dismissable prop) still calls onClose on Escape', async () => {
      const user = userEvent.setup();
      render(
        <Modal isOpen={true} onClose={onClose} title="Test Modal">
          <p>Modal content</p>
        </Modal>
      );
      await user.keyboard('{Escape}');
      expect(onClose).toHaveBeenCalled();
    });

    it('default (no dismissable prop) still calls onClose on overlay click', async () => {
      const user = userEvent.setup();
      render(
        <Modal isOpen={true} onClose={onClose} title="Test Modal">
          <p>Modal content</p>
        </Modal>
      );
      const overlay = document.querySelector('.modal-overlay')!;
      await user.click(overlay);
      expect(onClose).toHaveBeenCalled();
    });
  });

  describe('nested modals', () => {
    const onCloseOuter = vi.fn();
    const onCloseInner = vi.fn();

    beforeEach(() => {
      onCloseOuter.mockClear();
      onCloseInner.mockClear();
    });

    function NestedModals({ innerOpen = true }: { innerOpen?: boolean }) {
      return (
        <Modal isOpen={true} onClose={onCloseOuter} title="Outer Modal">
          <p>Outer content</p>
          <Modal isOpen={innerOpen} onClose={onCloseInner} title="Inner Modal">
            <p>Inner content</p>
          </Modal>
        </Modal>
      );
    }

    it('Escape closes only the topmost (inner) modal', async () => {
      const user = userEvent.setup();
      render(<NestedModals />);

      await user.keyboard('{Escape}');

      expect(onCloseInner).toHaveBeenCalledTimes(1);
      expect(onCloseOuter).not.toHaveBeenCalled();
    });

    it('overlay click closes only the topmost modal', async () => {
      const user = userEvent.setup();
      render(<NestedModals />);

      // Identify the inner overlay by its dialog's accessible name — modals now
      // portal to document.body, so DOM index is not a reliable "topmost" proxy.
      const innerOverlay = screen
        .getByRole('dialog', { name: 'Inner Modal' })
        .closest('.modal-overlay')!;
      await user.click(innerOverlay);

      expect(onCloseInner).toHaveBeenCalledTimes(1);
      expect(onCloseOuter).not.toHaveBeenCalled();
    });

    it('after inner closes, Escape closes the outer modal', async () => {
      const user = userEvent.setup();
      const { rerender } = render(<NestedModals />);

      // Close inner modal
      rerender(<NestedModals innerOpen={false} />);

      await user.keyboard('{Escape}');

      expect(onCloseOuter).toHaveBeenCalledTimes(1);
      expect(onCloseInner).not.toHaveBeenCalled();
    });

    it('after inner closes, overlay click closes the outer modal', async () => {
      const user = userEvent.setup();
      const { rerender } = render(<NestedModals />);

      // Close inner modal
      rerender(<NestedModals innerOpen={false} />);

      const overlay = document.querySelector('.modal-overlay')!;
      await user.click(overlay);

      expect(onCloseOuter).toHaveBeenCalledTimes(1);
      expect(onCloseInner).not.toHaveBeenCalled();
    });
  });

  describe('background inert (a11y — #2087)', () => {
    let root: HTMLElement;

    beforeEach(() => {
      // The inert coordinator targets the app mount point (#root); create one.
      root = document.createElement('div');
      root.id = 'root';
      document.body.appendChild(root);
    });

    afterEach(() => {
      root.remove();
    });

    it('makes #root inert while a modal is open and restores it on close', () => {
      const { unmount } = render(
        <Modal isOpen={true} onClose={onClose} title="Inert">
          <p>Body</p>
        </Modal>
      );
      expect(root).toHaveAttribute('inert');
      unmount();
      expect(root).not.toHaveAttribute('inert');
    });

    it('does not make the topmost modal overlay itself inert', () => {
      render(
        <Modal isOpen={true} onClose={onClose} title="Inert">
          <p>Body</p>
        </Modal>
      );
      const overlay = document.querySelector('.modal-overlay')!;
      expect(overlay).not.toHaveAttribute('inert');
    });

    it('inerts the parent overlay while a nested child is open, then restores it on child close', () => {
      const onCloseOuter = vi.fn();
      const onCloseInner = vi.fn();
      function Nested({ innerOpen = true }: { innerOpen?: boolean }) {
        return (
          <Modal isOpen={true} onClose={onCloseOuter} title="Outer">
            <p>Outer</p>
            <Modal isOpen={innerOpen} onClose={onCloseInner} title="Inner">
              <p>Inner</p>
            </Modal>
          </Modal>
        );
      }
      const { rerender } = render(<Nested />);
      // Identify overlays by accessible name — portal DOM order is not meaningful.
      const outerOverlay = () =>
        screen.getByRole('dialog', { name: 'Outer' }).closest('.modal-overlay');
      const innerOverlay = screen.getByRole('dialog', { name: 'Inner' }).closest('.modal-overlay');
      expect(outerOverlay()).toHaveAttribute('inert'); // parent inert — child is topmost
      expect(innerOverlay).not.toHaveAttribute('inert'); // child reachable

      rerender(<Nested innerOpen={false} />);
      expect(document.querySelectorAll('.modal-overlay').length).toBe(1);
      expect(outerOverlay()).not.toHaveAttribute('inert'); // parent now topmost — reachable
    });

    it('stays inside an open native Settings dialog and inerts its background panel', () => {
      const settingsHost = document.createElement('dialog');
      settingsHost.className = 'settings-overlay-host';
      settingsHost.setAttribute('open', '');
      settingsHost.dataset.modalPortalHost = 'true';
      const settingsPanel = document.createElement('div');
      settingsPanel.className = 'settings-overlay-host__panel';
      settingsHost.appendChild(settingsPanel);
      document.body.appendChild(settingsHost);

      try {
        const { unmount } = render(
          <ModalPortalHostContext.Provider value={settingsHost}>
            <Modal isOpen={true} onClose={onClose} title="Settings child">
              <button>Continue</button>
            </Modal>
          </ModalPortalHostContext.Provider>
        );
        const overlay = screen
          .getByRole('dialog', { name: 'Settings child' })
          .closest('.modal-overlay');

        expect(overlay?.parentElement).toBe(settingsHost);
        expect(settingsPanel).toHaveAttribute('inert');
        expect(overlay).not.toHaveAttribute('inert');

        unmount();
        expect(settingsPanel).not.toHaveAttribute('inert');
      } finally {
        settingsHost.remove();
      }
    });
  });
});
