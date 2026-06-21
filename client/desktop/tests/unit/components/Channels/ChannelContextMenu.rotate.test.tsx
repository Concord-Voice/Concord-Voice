import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { mockEncryptedChannel } from '../../../mocks/fixtures';
import { usePermissionStore } from '@/renderer/stores/permissionStore';
import {
  ADMIN_PERMISSIONS,
  MANAGE_CHANNELS,
  MANAGE_CRYPTO_ROTATION,
  BASE_PERMISSIONS,
} from '@/renderer/utils/permissions';
import ChannelContextMenu from '@/renderer/components/Channels/ChannelContextMenu';

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

import { apiFetch } from '@/renderer/services/apiClient';

const mockApiFetch = apiFetch as ReturnType<typeof vi.fn>;

const SERVER_ID = 'server-1';
const PERMS_WITH_ROTATE = ADMIN_PERMISSIONS | MANAGE_CHANNELS | MANAGE_CRYPTO_ROTATION;

describe('ChannelContextMenu — Rotate Encryption Key', () => {
  const mockOnClose = vi.fn();
  const mockOnEditChannel = vi.fn();
  const mockOnDeleteChannel = vi.fn();

  beforeEach(() => {
    vi.clearAllMocks();
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: PERMS_WITH_ROTATE },
    });
  });

  const renderMenu = (channel = mockEncryptedChannel) => {
    return render(
      <ChannelContextMenu
        channel={channel}
        position={{ x: 100, y: 100 }}
        serverId={SERVER_ID}
        onClose={mockOnClose}
        onEditChannel={mockOnEditChannel}
        onDeleteChannel={mockOnDeleteChannel}
      />
    );
  };

  it('shows Rotate Encryption Key for encrypted channel with MANAGE_CRYPTO_ROTATION', () => {
    renderMenu();
    expect(screen.getByText('Rotate Encryption Key')).toBeInTheDocument();
  });

  it('hides Rotate Encryption Key without MANAGE_CRYPTO_ROTATION permission', () => {
    usePermissionStore.setState({
      serverPermissions: { [SERVER_ID]: BASE_PERMISSIONS },
    });
    renderMenu();
    expect(screen.queryByText('Rotate Encryption Key')).not.toBeInTheDocument();
  });

  it('calls API and shows success on rotate', async () => {
    mockApiFetch.mockResolvedValue({ ok: true, status: 200 });
    renderMenu();

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        `/api/v1/channels/${mockEncryptedChannel.id}/rotate-key`,
        { method: 'POST' }
      );
    });

    await waitFor(() => {
      expect(screen.getByText('Key Rotated!')).toBeInTheDocument();
    });
  });

  it('shows rate limit message on 429', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      status: 429,
      json: async () => ({ retry_after: 3600 }),
    });
    renderMenu();

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('Try again in 1h')).toBeInTheDocument();
    });
  });

  it('shows error message on non-429 failure', async () => {
    mockApiFetch.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: 'Internal error' }),
    });
    renderMenu();

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('Internal error')).toBeInTheDocument();
    });
  });

  it('shows fallback error message on network failure', async () => {
    mockApiFetch.mockRejectedValue(new Error('Network error'));
    renderMenu();

    fireEvent.click(screen.getByText('Rotate Encryption Key'));

    await waitFor(() => {
      expect(screen.getByText('Network error')).toBeInTheDocument();
    });
  });
});
