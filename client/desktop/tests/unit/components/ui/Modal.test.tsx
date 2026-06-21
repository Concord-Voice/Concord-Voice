import { render, screen, userEvent } from '../../../test-utils';
import Modal from '@/renderer/components/ui/Modal';
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

  describe('dismissable prop', () => {
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

      // The last .modal-overlay in the DOM is the inner modal's overlay
      const overlays = document.querySelectorAll('.modal-overlay');
      const innerOverlay = overlays[overlays.length - 1];
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
});
