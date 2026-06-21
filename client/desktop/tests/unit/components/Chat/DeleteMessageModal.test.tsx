import { render, screen } from '../../../test-utils';
import DeleteMessageModal from '@/renderer/components/Chat/DeleteMessageModal';
import { vi } from 'vitest';

describe('DeleteMessageModal', () => {
  const onClose = vi.fn();
  const onConfirm = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders when open', () => {
    render(<DeleteMessageModal isOpen={true} onClose={onClose} onConfirm={onConfirm} />);
    expect(screen.getByText(/delete message/i)).toBeInTheDocument();
  });

  it('does not render when closed', () => {
    render(<DeleteMessageModal isOpen={false} onClose={onClose} onConfirm={onConfirm} />);
    expect(screen.queryByText(/delete message/i)).not.toBeInTheDocument();
  });

  it('calls onClose when cancel is clicked', async () => {
    const { userEvent } = await import('../../../test-utils');
    const user = userEvent.setup();
    render(<DeleteMessageModal isOpen={true} onClose={onClose} onConfirm={onConfirm} />);
    await user.click(screen.getByText(/cancel/i));
    expect(onClose).toHaveBeenCalled();
  });

  it('calls onConfirm when delete is clicked', async () => {
    const { userEvent } = await import('../../../test-utils');
    const user = userEvent.setup();
    render(<DeleteMessageModal isOpen={true} onClose={onClose} onConfirm={onConfirm} />);
    await user.click(screen.getByText(/^delete$/i));
    expect(onConfirm).toHaveBeenCalled();
  });
});
