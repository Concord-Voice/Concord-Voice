import { render, screen, fireEvent } from '../../../test-utils';
import MessageActions from '@/renderer/components/Chat/MessageActions';
import { vi } from 'vitest';

describe('MessageActions', () => {
  const defaultProps = {
    messageId: 'msg-1',
    canModify: true,
    isEditing: false,
    shiftHeld: false,
    onEdit: vi.fn(),
    onDelete: vi.fn(),
    onRequestDelete: vi.fn(),
  };

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('does not render when canModify is false', () => {
    const { container } = render(<MessageActions {...defaultProps} canModify={false} />);
    expect(container.querySelector('.message-options')).not.toBeInTheDocument();
  });

  it('does not render when isEditing is true', () => {
    const { container } = render(<MessageActions {...defaultProps} isEditing={true} />);
    expect(container.querySelector('.message-options')).not.toBeInTheDocument();
  });

  it('renders options trigger button when canModify and not editing', () => {
    render(<MessageActions {...defaultProps} />);
    expect(screen.getByLabelText('Message options')).toBeInTheDocument();
  });

  it('opens options menu on trigger click', () => {
    const { container } = render(<MessageActions {...defaultProps} />);
    const options = container.querySelector('.message-options');

    const trigger = screen.getByLabelText('Message options');
    expect(options).not.toHaveClass('message-options--open');

    fireEvent.click(trigger);

    expect(options).toHaveClass('message-options--open');
    expect(screen.getByText('Edit')).toBeInTheDocument();
    expect(screen.getByText('Delete')).toBeInTheDocument();
  });

  it('calls onEdit and closes menu when Edit is clicked', () => {
    render(<MessageActions {...defaultProps} />);

    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Edit'));

    expect(defaultProps.onEdit).toHaveBeenCalledTimes(1);
    // Menu should be closed after clicking Edit
    expect(screen.queryByText('Edit')).not.toBeInTheDocument();
  });

  it('calls onRequestDelete when Delete is clicked without shift', () => {
    render(<MessageActions {...defaultProps} />);

    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Delete'));

    expect(defaultProps.onRequestDelete).toHaveBeenCalledTimes(1);
    expect(defaultProps.onDelete).not.toHaveBeenCalled();
  });

  it('calls onDelete directly when Delete is shift-clicked', () => {
    render(<MessageActions {...defaultProps} />);

    fireEvent.click(screen.getByLabelText('Message options'));
    fireEvent.click(screen.getByText('Delete'), { shiftKey: true });

    expect(defaultProps.onDelete).toHaveBeenCalledWith('msg-1');
    expect(defaultProps.onRequestDelete).not.toHaveBeenCalled();
  });

  it('shows quick-delete button when shiftHeld is true', () => {
    render(<MessageActions {...defaultProps} shiftHeld={true} />);
    expect(screen.getByLabelText('Delete message')).toBeInTheDocument();
  });

  it('does not show quick-delete button when shiftHeld is false', () => {
    render(<MessageActions {...defaultProps} shiftHeld={false} />);
    expect(screen.queryByLabelText('Delete message')).not.toBeInTheDocument();
  });

  it('quick-delete calls onDelete directly', () => {
    render(<MessageActions {...defaultProps} shiftHeld={true} />);

    fireEvent.click(screen.getByLabelText('Delete message'));
    expect(defaultProps.onDelete).toHaveBeenCalledWith('msg-1');
  });

  it('closes options menu on outside click', () => {
    const { container } = render(
      <div>
        <MessageActions {...defaultProps} />
        <button data-testid="outside">Outside</button>
      </div>
    );
    const options = container.querySelector('.message-options');

    // Open menu
    fireEvent.click(screen.getByLabelText('Message options'));
    expect(screen.getByText('Edit')).toBeInTheDocument();
    expect(options).toHaveClass('message-options--open');

    // Click outside
    fireEvent.mouseDown(screen.getByTestId('outside'));
    expect(screen.queryByText('Edit')).not.toBeInTheDocument();
    expect(options).not.toHaveClass('message-options--open');
  });

  it('does not show Edit when onEdit is undefined', () => {
    render(<MessageActions {...defaultProps} onEdit={undefined} />);

    fireEvent.click(screen.getByLabelText('Message options'));
    expect(screen.queryByText('Edit')).not.toBeInTheDocument();
    expect(screen.getByText('Delete')).toBeInTheDocument();
  });

  it('does not show Delete or quick-delete when onDelete is undefined', () => {
    render(<MessageActions {...defaultProps} onDelete={undefined} shiftHeld={true} />);

    fireEvent.click(screen.getByLabelText('Message options'));
    expect(screen.queryByText('Delete')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Delete message')).not.toBeInTheDocument();
  });
});
