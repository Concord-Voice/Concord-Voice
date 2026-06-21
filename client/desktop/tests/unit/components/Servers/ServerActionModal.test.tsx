import { render, screen, fireEvent } from '../../../test-utils';
import ServerActionModal from '@/renderer/components/Servers/ServerActionModal';

describe('ServerActionModal', () => {
  const mockOnClose = vi.fn();
  const mockOnCreateServer = vi.fn();
  const mockOnJoinServer = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <ServerActionModal
        isOpen={false}
        onClose={mockOnClose}
        onCreateServer={mockOnCreateServer}
        onJoinServer={mockOnJoinServer}
      />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders create and join options when open', () => {
    render(
      <ServerActionModal
        isOpen={true}
        onClose={mockOnClose}
        onCreateServer={mockOnCreateServer}
        onJoinServer={mockOnJoinServer}
      />
    );
    expect(screen.getByText('Create a Server')).toBeInTheDocument();
    expect(screen.getByText('Join a Server')).toBeInTheDocument();
  });

  it('calls onCreateServer and onClose when Create clicked', () => {
    render(
      <ServerActionModal
        isOpen={true}
        onClose={mockOnClose}
        onCreateServer={mockOnCreateServer}
        onJoinServer={mockOnJoinServer}
      />
    );
    fireEvent.click(screen.getByText('Create a Server'));
    expect(mockOnClose).toHaveBeenCalled();
    expect(mockOnCreateServer).toHaveBeenCalled();
  });

  it('calls onJoinServer and onClose when Join clicked', () => {
    render(
      <ServerActionModal
        isOpen={true}
        onClose={mockOnClose}
        onCreateServer={mockOnCreateServer}
        onJoinServer={mockOnJoinServer}
      />
    );
    fireEvent.click(screen.getByText('Join a Server'));
    expect(mockOnClose).toHaveBeenCalled();
    expect(mockOnJoinServer).toHaveBeenCalled();
  });

  it('shows descriptions', () => {
    render(
      <ServerActionModal
        isOpen={true}
        onClose={mockOnClose}
        onCreateServer={mockOnCreateServer}
        onJoinServer={mockOnJoinServer}
      />
    );
    expect(screen.getByText('Start a new community from scratch')).toBeInTheDocument();
    expect(screen.getByText(/Enter an invite code/)).toBeInTheDocument();
  });
});
