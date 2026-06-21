import { render, screen, fireEvent } from '../../../test-utils';
import JoinServerModal from '@/renderer/components/Servers/JoinServerModal';
import { useInviteStore } from '@/renderer/stores/inviteStore';

// Mock LoadingSpinner
vi.mock('@/renderer/components/Auth/LoadingSpinner', () => ({
  default: () => <div data-testid="loading-spinner" />,
}));

describe('JoinServerModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    useInviteStore.setState({
      invites: {},
      isLoading: false,
      error: null,
    });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <JoinServerModal isOpen={false} onClose={mockOnClose} onSuccess={mockOnSuccess} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form when open', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Join a Server')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('AbCd1234')).toBeInTheDocument();
  });

  it('shows character count', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('0/8 characters')).toBeInTheDocument();
  });

  it('updates character count as user types', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234');
    fireEvent.change(input, { target: { value: 'abc' } });
    expect(screen.getByText(/3\/8 characters/)).toBeInTheDocument();
  });

  it('strips non-alphanumeric characters', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ab!@#cd12' } });
    expect(input.value).toBe('abcd12');
  });

  it('limits input to 8 characters', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'AbCd12345678' } });
    expect(input.value).toBe('AbCd1234');
  });

  it('Join Server button is disabled when code is incomplete', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    expect(screen.getByText('Join Server')).toBeDisabled();
  });

  it('calls onClose when Cancel clicked', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('shows "keep typing" hint when code is partially entered', () => {
    render(<JoinServerModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);
    const input = screen.getByPlaceholderText('AbCd1234');
    fireEvent.change(input, { target: { value: 'abc' } });
    expect(screen.getByText(/keep typing/)).toBeInTheDocument();
  });
});
