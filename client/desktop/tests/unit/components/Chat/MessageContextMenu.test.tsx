import { render, screen, fireEvent } from '../../../test-utils';
import { mockMessage } from '../../../mocks/fixtures';
import MessageContextMenu from '@/renderer/components/Chat/MessageContextMenu';

describe('MessageContextMenu', () => {
  const mockOnClose = vi.fn();
  const mockOnEdit = vi.fn();
  const mockOnDelete = vi.fn();
  const mockOnReaction = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  const renderMenu = (canModify = true, onReaction?: () => void) => {
    return render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={canModify}
        canModify={canModify}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        onReaction={onReaction}
      />
    );
  };

  it('renders Copy Text option', () => {
    renderMenu();
    expect(screen.getByText('Copy Text')).toBeInTheDocument();
  });

  it('renders Reply option (disabled)', () => {
    renderMenu();
    const replyBtn = screen.getByText('Reply');
    expect(replyBtn).toBeInTheDocument();
    expect(replyBtn.closest('button')).toBeDisabled();
  });

  it('renders Pin Message option (disabled)', () => {
    renderMenu();
    const pinBtn = screen.getByText('Pin Message');
    expect(pinBtn).toBeInTheDocument();
    expect(pinBtn.closest('button')).toBeDisabled();
  });

  it('shows Edit/Delete for message author', () => {
    renderMenu(true);
    expect(screen.getByText('Edit Message')).toBeInTheDocument();
    expect(screen.getByText('Delete Message')).toBeInTheDocument();
  });

  it('hides Edit/Delete for non-author', () => {
    renderMenu(false);
    expect(screen.queryByText('Edit Message')).not.toBeInTheDocument();
    expect(screen.queryByText('Delete Message')).not.toBeInTheDocument();
  });

  it('calls onEdit when Edit clicked', () => {
    renderMenu(true);
    fireEvent.click(screen.getByText('Edit Message'));
    expect(mockOnEdit).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('calls onDelete when Delete clicked', () => {
    renderMenu(true);
    fireEvent.click(screen.getByText('Delete Message'));
    expect(mockOnDelete).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('renders Add Reaction option', () => {
    renderMenu(true, mockOnReaction);
    expect(screen.getByText('Add Reaction')).toBeInTheDocument();
  });

  it('calls onReaction and closes when Add Reaction clicked', () => {
    renderMenu(true, mockOnReaction);
    fireEvent.click(screen.getByText('Add Reaction'));
    expect(mockOnReaction).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('renders Add Reaction even when canModify is false', () => {
    renderMenu(false, mockOnReaction);
    expect(screen.getByText('Add Reaction')).toBeInTheDocument();
  });

  it('Reply item is enabled when onReply provided', () => {
    const onReply = vi.fn();
    render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={true}
        canModify={true}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        onReply={onReply}
      />
    );
    const replyBtn = screen.getByText('Reply');
    expect(replyBtn.closest('button')).not.toBeDisabled();
  });

  it('Reply item calls onReply and closes menu', () => {
    const onReply = vi.fn();
    render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={true}
        canModify={true}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        onReply={onReply}
      />
    );
    fireEvent.click(screen.getByText('Reply'));
    expect(onReply).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows Pin Message when canPin is true', () => {
    const onPin = vi.fn();
    render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={true}
        canModify={true}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        onPin={onPin}
        canPin={true}
      />
    );
    const pinBtn = screen.getByText('Pin Message');
    expect(pinBtn.closest('button')).not.toBeDisabled();
  });

  it('shows Unpin Message when isPinned is true', () => {
    render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={true}
        canModify={true}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        isPinned={true}
        canPin={true}
      />
    );
    expect(screen.getByText('Unpin Message')).toBeInTheDocument();
  });

  it('Pin item is disabled when canPin is false', () => {
    render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={true}
        canModify={true}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        canPin={false}
      />
    );
    const pinBtn = screen.getByText('Pin Message');
    expect(pinBtn.closest('button')).toBeDisabled();
  });

  it('Pin item calls onPin and closes menu', () => {
    const onPin = vi.fn();
    render(
      <MessageContextMenu
        message={mockMessage}
        position={{ x: 100, y: 100 }}
        isOwnMessage={true}
        canModify={true}
        onClose={mockOnClose}
        onEdit={mockOnEdit}
        onDelete={mockOnDelete}
        onPin={onPin}
        canPin={true}
      />
    );
    fireEvent.click(screen.getByText('Pin Message'));
    expect(onPin).toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });
});
