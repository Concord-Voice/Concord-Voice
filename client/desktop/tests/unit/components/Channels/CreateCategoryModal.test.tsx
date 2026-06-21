import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { mockServer } from '../../../mocks/fixtures';

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';
import CreateCategoryModal from '@/renderer/components/Channels/CreateCategoryModal';

const mockedApiFetch = vi.mocked(apiFetch);

describe('CreateCategoryModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
  });

  it('renders nothing when closed', () => {
    const { container } = render(<CreateCategoryModal isOpen={false} onClose={mockOnClose} />);
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form when open', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByLabelText(/Category Name/)).toBeInTheDocument();
    expect(screen.getByPlaceholderText('General')).toBeInTheDocument();
    expect(
      screen.getByText('Create Category', { selector: 'button[type="submit"]' })
    ).toBeInTheDocument();
  });

  it('renders Cancel button', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('Cancel')).toBeInTheDocument();
  });

  it('shows character count', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    expect(screen.getByText('0/100 characters')).toBeInTheDocument();
    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Test' },
    });
    expect(screen.getByText('4/100 characters')).toBeInTheDocument();
  });

  it('shows validation error for empty name', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Category name is required')).toBeInTheDocument();
  });

  it('shows validation error for whitespace-only name', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: '   ' },
    });
    fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Category name is required')).toBeInTheDocument();
  });

  it('shows validation error for name exceeding max length', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    const longName = 'a'.repeat(101);
    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: longName },
    });
    fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Category name must be at most 100 characters')).toBeInTheDocument();
  });

  it('submits valid form successfully', async () => {
    vi.useFakeTimers();

    const newGroup = {
      id: 'group-new',
      server_id: 'server-1',
      name: 'New Category',
      position: 0,
      created_at: '2025-01-01T00:00:00Z',
      updated_at: '2025-01-01T00:00:00Z',
    };

    mockedApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ channel_group: newGroup }),
    } as Response);

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} onSuccess={mockOnSuccess} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'New Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Category created!')).toBeInTheDocument();
    expect(useChannelStore.getState().channelGroups).toHaveLength(1);
    expect(mockOnSuccess).toHaveBeenCalled();

    vi.useRealTimers();
  });

  it('handles API response with "group" key', async () => {
    vi.useFakeTimers();

    const newGroup = {
      id: 'group-alt',
      server_id: 'server-1',
      name: 'Alt Category',
      position: 0,
      created_at: '2025-01-01T00:00:00Z',
      updated_at: '2025-01-01T00:00:00Z',
    };

    mockedApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ group: newGroup }),
    } as Response);

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Alt Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Category created!')).toBeInTheDocument();

    vi.useRealTimers();
  });

  it('shows error on API failure', async () => {
    mockedApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Name already taken' }),
    } as Response);

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Existing Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Name already taken')).toBeInTheDocument();
    });
  });

  it('shows generic error on network failure', async () => {
    mockedApiFetch.mockRejectedValue(new Error('Network error'));

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Test Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });
  });

  it('shows generic fallback error on non-Error throw', async () => {
    mockedApiFetch.mockRejectedValue('unexpected');

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Test Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Failed to create category')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('does not submit when no active server', async () => {
    useServerStore.getState().clearServers();

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Test Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    expect(mockedApiFetch).not.toHaveBeenCalled();
  });

  it('disables submit button when no active server', () => {
    useServerStore.getState().clearServers();

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    const submitBtn = screen.getByText('Create Category', { selector: 'button[type="submit"]' });
    expect(submitBtn).toBeDisabled();
  });

  it('disables inputs while submitting', async () => {
    // Never resolve the API call to keep the submitting state
    mockedApiFetch.mockReturnValue(new Promise(() => {}));

    render(<CreateCategoryModal isOpen={true} onClose={mockOnClose} />);

    fireEvent.change(screen.getByPlaceholderText('General'), {
      target: { value: 'Test Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Create Category', { selector: 'button[type="submit"]' }));
    });

    expect(screen.getByPlaceholderText('General')).toBeDisabled();
    expect(screen.getByText('Cancel')).toBeDisabled();
  });
});
