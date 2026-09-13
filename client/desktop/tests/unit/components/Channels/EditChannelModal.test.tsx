import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { StrictMode } from 'react';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { mockChannel, mockServer, mockUser } from '../../../mocks/fixtures';
import { deferred } from '../../../helpers/deferred';
import { Permissions } from '@/renderer/utils/policy/permissions';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('emoji-picker-react', () => ({
  default: () => <div data-testid="emoji-picker" />,
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
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
      <StrictMode>
        <EditChannelModal
          isOpen={true}
          channel={mockChannel}
          onClose={mockOnClose}
          onSuccess={mockOnSuccess}
        />
      </StrictMode>
    );
    expect(screen.getByText('Edit Channel')).toBeInTheDocument();
    expect(screen.getByDisplayValue('general')).toBeInTheDocument();
  });

  it('disables Save when no changes made', () => {
    render(
      <StrictMode>
        <EditChannelModal
          isOpen={true}
          channel={mockChannel}
          onClose={mockOnClose}
          onSuccess={mockOnSuccess}
        />
      </StrictMode>
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
      <StrictMode>
        <EditChannelModal
          isOpen={true}
          channel={mockChannel}
          onClose={mockOnClose}
          onSuccess={mockOnSuccess}
        />
      </StrictMode>
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

  it('ignores a held save after the same channel is closed and reopened', async () => {
    const oldSave = deferred<Response>();
    mockedApiFetch.mockReturnValueOnce(oldSave.promise);
    const { rerender } = render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );

    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'old-name' },
    });
    fireEvent.click(screen.getByText('Save Changes'));
    await waitFor(() => expect(mockedApiFetch).toHaveBeenCalledTimes(1));

    rerender(
      <EditChannelModal
        isOpen={false}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    rerender(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'successor-name' },
    });

    try {
      await act(async () => {
        oldSave.resolve({
          ok: true,
          json: async () => ({ channel: { ...mockChannel, name: 'old-name' } }),
        } as Response);
        await oldSave.promise;
      });
    } finally {
      oldSave.resolve({ ok: true, json: async () => ({}) } as Response);
      await oldSave.promise;
    }

    expect(screen.getByDisplayValue('successor-name')).toBeInTheDocument();
    expect(screen.queryByText('Channel updated successfully!')).not.toBeInTheDocument();
    expect(mockOnClose).not.toHaveBeenCalled();
    expect(mockOnSuccess).not.toHaveBeenCalled();
  });

  it('locks expiration controls while a channel save is held', async () => {
    const save = deferred<Response>();
    useUserStore.getState().setUser({ id: mockUser.id, username: mockUser.username });
    useChannelStore.setState({ currentServerId: 'server-1' });
    usePermissionStore.setState({
      channelPermissions: { [mockChannel.id]: Permissions.MANAGE_CHANNELS },
    });
    mockedApiFetch.mockImplementation(async (url, init) => {
      if (String(url).endsWith('/api/v1/servers/server-1/channels')) {
        return {
          ok: true,
          json: async () => ({
            channels: [
              {
                ...mockChannel,
                expiration_window_seconds: 86400,
                expiration_updated_at: '2026-09-08T05:00:00Z',
                expiration_revision: 4,
                expiration_backfill_pending: false,
              },
            ],
          }),
        } as Response;
      }
      if (init?.method === 'PATCH') return save.promise;
      return { ok: true, json: async () => ({}) } as Response;
    });
    render(
      <EditChannelModal
        isOpen={true}
        channel={mockChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'false'
      )
    );
    fireEvent.change(screen.getByDisplayValue('general'), {
      target: { value: 'held-save' },
    });
    fireEvent.click(screen.getByText('Save Changes'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: '1 hour' })).toHaveAttribute(
        'aria-disabled',
        'true'
      )
    );
    save.resolve({ ok: true, json: async () => ({ channel: mockChannel }) } as Response);
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

  it('renders the audio quality slider (not the box-select) for voice channels', () => {
    const voiceChannel = { ...mockChannel, type: 'voice' as const };
    useServerStore.getState().addServer({ ...mockServer, server_tier: 'groundspeed' });
    useChannelStore.getState().addChannel(voiceChannel);

    render(
      <EditChannelModal
        isOpen={true}
        channel={voiceChannel}
        onClose={mockOnClose}
        onSuccess={mockOnSuccess}
      />
    );

    // Slider renders with a Personal stop
    expect(screen.getAllByText('Personal').length).toBeGreaterThanOrEqual(1);
    // Old box-select "Each user uses their own quality setting" button is gone
    expect(screen.queryByRole('button', { name: /Each user uses their own quality/i })).toBeNull();
  });
});
