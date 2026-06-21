import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { mockChannel } from '../../../mocks/fixtures';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('emoji-picker-react', () => ({
  default: () => <div data-testid="emoji-picker" />,
}));

import { apiFetch } from '@/renderer/services/apiClient';
import EditChannelModal from '@/renderer/components/Channels/EditChannelModal';

const mockedApiFetch = vi.mocked(apiFetch);

describe('EditChannelModal', () => {
  const mockOnClose = vi.fn();
  const mockOnSuccess = vi.fn();

  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useChannelStore.getState().addChannel(mockChannel);
  });

  it('renders nothing when closed', () => {
    const { container } = render(
      <EditChannelModal
        isOpen={false}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    expect(container.querySelector('.modal-overlay')).not.toBeInTheDocument();
  });

  it('renders form with current channel data', () => {
    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    expect(screen.getByText('Edit Channel')).toBeInTheDocument();
    expect(screen.getByDisplayValue('general')).toBeInTheDocument();
  });

  it('disables Save when no changes made', () => {
    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    const saveBtn = screen.getByText('Save Changes');
    expect(saveBtn).toBeDisabled();
  });

  it('enables Save when name changes', () => {
    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'updated-channel' },
    });
    const saveBtn = screen.getByText('Save Changes');
    expect(saveBtn).not.toBeDisabled();
  });

  it('submits changes successfully', async () => {
    vi.useFakeTimers();

    mockedApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ channel: { ...mockChannel, name: 'updated-channel' } }),
    } as Response);

    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );

    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'updated-channel' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes'));
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });

    expect(screen.getByText('Channel updated successfully!')).toBeInTheDocument();

    vi.useRealTimers();
  });

  it('shows error on API failure', async () => {
    mockedApiFetch.mockResolvedValue({
      ok: false,
      json: async () => ({ error: 'Not allowed' }),
    } as Response);

    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );

    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'new-name' },
    });

    await act(async () => {
      fireEvent.click(screen.getByText('Save Changes'));
    });

    await waitFor(() => {
      expect(screen.getByText('Not allowed')).toBeInTheDocument();
    });
  });

  it('validates short name', () => {
    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'ab' },
    });
    fireEvent.click(screen.getByText('Save Changes'));
    expect(screen.getByText('Channel name must be at least 3 characters')).toBeInTheDocument();
  });

  it('calls onClose when Cancel is clicked', () => {
    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    fireEvent.click(screen.getByText('Cancel'));
    expect(mockOnClose).toHaveBeenCalled();
  });
});
