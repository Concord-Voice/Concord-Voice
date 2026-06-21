import { render, screen, fireEvent } from '../../../test-utils';
import MessageInputContextMenu from '@/renderer/components/Chat/MessageInputContextMenu';

describe('MessageInputContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnPaste = vi.fn();
  const defaultProps = {
    position: { x: 100, y: 200 },
    onClose: mockOnClose,
    onPaste: mockOnPaste,
  };

  beforeEach(() => {
    vi.clearAllMocks();
    // Restore default readText return value after vi.clearAllMocks() clears call history
    vi.mocked(navigator.clipboard.readText).mockResolvedValue('pasted text');
  });

  it('renders Paste menu item', () => {
    render(<MessageInputContextMenu {...defaultProps} />);
    expect(screen.getByText('Paste')).toBeInTheDocument();
  });

  it('renders Upload File menu item (disabled)', () => {
    render(<MessageInputContextMenu {...defaultProps} />);
    expect(screen.getByText('Upload File')).toBeInTheDocument();
  });

  it('renders Insert Emoji menu item (disabled)', () => {
    render(<MessageInputContextMenu {...defaultProps} />);
    expect(screen.getByText('Insert Emoji')).toBeInTheDocument();
  });

  it('calls onPaste with clipboard text on Paste click', async () => {
    render(<MessageInputContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Paste'));
    await vi.waitFor(() => {
      expect(mockOnPaste).toHaveBeenCalledWith('pasted text');
    });
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onClose even if clipboard read fails', async () => {
    (navigator.clipboard.readText as ReturnType<typeof vi.fn>).mockRejectedValueOnce(
      new Error('denied')
    );
    render(<MessageInputContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Paste'));
    await vi.waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(mockOnPaste).not.toHaveBeenCalled();
  });

  it('does not call onPaste when clipboard is empty', async () => {
    (navigator.clipboard.readText as ReturnType<typeof vi.fn>).mockResolvedValueOnce('');
    render(<MessageInputContextMenu {...defaultProps} />);
    fireEvent.click(screen.getByText('Paste'));
    await vi.waitFor(() => {
      expect(mockOnClose).toHaveBeenCalled();
    });
    expect(mockOnPaste).not.toHaveBeenCalled();
  });
});
