import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { mockServer } from '../../../mocks/fixtures';
import type { ChannelGroup } from '@/renderer/types/chat';

// Mock apiFetch
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';
import EditCategoryModal from '@/renderer/components/Channels/EditCategoryModal';

const mockedApiFetch = vi.mocked(apiFetch);

const mockGroup: ChannelGroup = {
  id: 'group-1',
  server_id: 'server-1',
  name: 'Voice Channels',
  position: 0,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
};

describe('EditCategoryModal', () => {
  const mockOnClose = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useServerStore.getState().addServer(mockServer);
    useServerStore.getState().setActiveServer(mockServer.id);
    useChannelStore.setState({
      channelGroups: [mockGroup],
    });
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <EditCategoryModal isOpen={false} group={mockGroup} onClose={mockOnClose} />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form when open with current name populated', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    expect(screen.getByLabelText(/Category Name/)).toBeInTheDocument();
    const input = screen.getByDisplayValue('Voice Channels');
    expect(input).toBeInTheDocument();
  });

  it('renders Save Changes button', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    expect(
      screen.getByText('Save Changes', { selector: 'button[type="submit"]' })
    ).toBeInTheDocument();
  });

  it('shows character count', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    // "Voice Channels" is 14 characters
    expect(screen.getByText('14/100 characters')).toBeInTheDocument();
  });

  it('shows validation error for empty name', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: '' },
    });
    fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Category name is required')).toBeInTheDocument();
  });

  it('shows validation error for whitespace-only name', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: '   ' },
    });
    fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Category name is required')).toBeInTheDocument();
  });

  it('shows validation error for name exceeding max length', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    const longName = 'a'.repeat(101);
    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: longName },
    });
    fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    expect(screen.getByText('Category name must be at most 100 characters')).toBeInTheDocument();
  });

  it('closes without API call when name is unchanged', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    expect(mockedApiFetch).not.toHaveBeenCalled();
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('submits valid form successfully', async () => {
    vi.useFakeTimers();

    mockedApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ channel_group: { ...mockGroup, name: 'Renamed Category' } }),
    } as Response);

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'Renamed Category' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Category updated!')).toBeInTheDocument();
    expect(useChannelStore.getState().channelGroups[0].name).toBe('Renamed Category');

    vi.useRealTimers();
  });

  it('shows error on API failure', async () => {
    mockedApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Name already taken' }),
    } as Response);

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'New Name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Name already taken')).toBeInTheDocument();
    });
  });

  it('shows generic error on network failure', async () => {
    mockedApiFetch.mockRejectedValue(new Error('Connection refused'));

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'New Name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Connection refused')).toBeInTheDocument();
    });
  });

  it('shows generic fallback error on non-Error throw', async () => {
    mockedApiFetch.mockRejectedValue('unexpected');

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'New Name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    await waitFor(() => {
      expect(screen.getByText('Failed to update category')).toBeInTheDocument();
    });
  });

  it('calls onClose when Cancel is clicked', () => {
    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });

  it('does not submit when no active server', async () => {
    useServerStore.getState().clearServers();

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'New Name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    expect(mockedApiFetch).not.toHaveBeenCalled();
  });

  it('disables submit button when no active server', () => {
    useServerStore.getState().clearServers();

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    const submitBtn = screen.getByText('Save Changes', { selector: 'button[type="submit"]' });
    expect(submitBtn).toBeDisabled();
  });

  it('disables inputs while submitting', async () => {
    // Never resolve the API call to keep the submitting state
    mockedApiFetch.mockReturnValue(new Promise(() => {}));

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'New Name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    expect(screen.getByDisplayValue('New Name')).toBeDisabled();
    expect(screen.getByText('Cancel')).toBeDisabled();
  });

  it('does not close while submitting when Cancel is clicked', async () => {
    // Never resolve to keep submitting state
    mockedApiFetch.mockReturnValue(new Promise(() => {}));

    render(<EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />);

    fireEvent.change(screen.getByDisplayValue('Voice Channels'), {
      target: { value: 'New Name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes', { selector: 'button[type="submit"]' }));
    });

    // Cancel is disabled during submit
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).not.toHaveBeenCalled();
  });

  it('updates input when group prop changes', () => {
    const { rerender } = render(
      <EditCategoryModal isOpen={true} group={mockGroup} onClose={mockOnClose} />
    );

    expect(screen.getByDisplayValue('Voice Channels')).toBeInTheDocument();

    const updatedGroup = { ...mockGroup, name: 'Text Channels' };
    rerender(<EditCategoryModal isOpen={true} group={updatedGroup} onClose={mockOnClose} />);

    expect(screen.getByDisplayValue('Text Channels')).toBeInTheDocument();
  });
});
