import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import PinnedMessagesPanel from '@/renderer/components/Chat/PinnedMessagesPanel';
import { vi } from 'vitest';
import type { MessageWithUser } from '@/renderer/types/chat';

// Mock pin service
const mockGetChannelPins = vi.fn();
const mockUnpinMessage = vi.fn();
vi.mock('@/renderer/services/pinService', () => ({
  getChannelPins: (...args: unknown[]) => mockGetChannelPins(...args),
  unpinMessage: (...args: unknown[]) => mockUnpinMessage(...args),
}));

// Mock e2ee service
const mockDecryptWithKey = vi.fn();
const mockDecryptForChannel = vi.fn();
const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

const mockPins: MessageWithUser[] = [
  {
    id: 'pin-1',
    channel_id: 'channel-1',
    user_id: 'user-1',
    content: 'Pinned message one',
    username: 'testuser',
    display_name: 'Test User',
    created_at: '2025-01-01T12:00:00Z',
    updated_at: '2025-01-01T12:00:00Z',
    pinned_at: '2025-01-01T13:00:00Z',
    pinned_by: 'user-1',
  },
  {
    id: 'pin-2',
    channel_id: 'channel-1',
    user_id: 'user-2',
    content: 'Pinned message two',
    username: 'testuser2',
    created_at: '2025-01-01T12:01:00Z',
    updated_at: '2025-01-01T12:01:00Z',
    pinned_at: '2025-01-01T13:01:00Z',
    pinned_by: 'user-1',
  },
];

async function enableE2EE() {
  const { e2eeService } = await import('@/renderer/services/e2eeService');
  Object.defineProperty(e2eeService, 'isInitialized', { value: true, writable: true });
}

describe('PinnedMessagesPanel', () => {
  const defaultProps = {
    channelId: 'channel-1',
    isOpen: true,
    onClose: vi.fn(),
    onScrollToMessage: vi.fn(),
    canPin: true,
  };

  beforeEach(async () => {
    vi.clearAllMocks();
    // Reset isInitialized to false before each test (enableE2EE persists via defineProperty)
    const { e2eeService } = await import('@/renderer/services/e2eeService');
    Object.defineProperty(e2eeService, 'isInitialized', { value: false, writable: true });
    mockGetChannelPins.mockResolvedValue(mockPins);
    mockUnpinMessage.mockResolvedValue({ message_id: 'pin-1' });
  });

  it('returns null when not open', () => {
    const { container } = render(<PinnedMessagesPanel {...defaultProps} isOpen={false} />);
    expect(container.querySelector('.pinned-panel-backdrop')).not.toBeInTheDocument();
  });

  it('shows loading state then renders pinned messages', async () => {
    await enableE2EE();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey
      .mockResolvedValueOnce('Pinned message one')
      .mockResolvedValueOnce('Pinned message two');

    render(<PinnedMessagesPanel {...defaultProps} />);

    await waitFor(() => {
      expect(screen.getByText('Pinned message one')).toBeInTheDocument();
      expect(screen.getByText('Pinned message two')).toBeInTheDocument();
    });

    expect(mockGetChannelPins).toHaveBeenCalledWith('channel-1');
  });

  it('shows empty state when no pins', async () => {
    mockGetChannelPins.mockResolvedValue([]);

    render(<PinnedMessagesPanel {...defaultProps} />);

    await waitFor(() => {
      expect(screen.getByText('No pinned messages')).toBeInTheDocument();
    });
  });

  it('calls onScrollToMessage and onClose when Jump is clicked', async () => {
    render(<PinnedMessagesPanel {...defaultProps} />);

    await waitFor(() => {
      expect(screen.getAllByText('Jump')).toHaveLength(2);
    });

    fireEvent.click(screen.getAllByText('Jump')[0]);
    expect(defaultProps.onScrollToMessage).toHaveBeenCalledWith('pin-1');
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('calls unpinMessage and removes from list when Unpin is clicked', async () => {
    render(<PinnedMessagesPanel {...defaultProps} />);

    await waitFor(() => {
      expect(screen.getAllByText('Unpin')).toHaveLength(2);
    });

    fireEvent.click(screen.getAllByText('Unpin')[0]);

    await waitFor(() => {
      expect(mockUnpinMessage).toHaveBeenCalledWith('pin-1');
    });
  });

  it('hides Unpin buttons when canPin is false', async () => {
    await enableE2EE();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey
      .mockResolvedValueOnce('Pinned message one')
      .mockResolvedValueOnce('Pinned message two');

    render(<PinnedMessagesPanel {...defaultProps} canPin={false} />);

    await waitFor(() => {
      expect(screen.getByText('Pinned message one')).toBeInTheDocument();
    });

    expect(screen.queryByText('Unpin')).not.toBeInTheDocument();
  });

  it('calls onClose when backdrop dismiss button is clicked', async () => {
    render(<PinnedMessagesPanel {...defaultProps} />);

    const dismissBtn = document.querySelector('.pinned-panel-backdrop-dismiss') as HTMLElement;
    expect(dismissBtn).toBeInTheDocument();
    fireEvent.click(dismissBtn);
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('calls onClose when close button is clicked', async () => {
    render(<PinnedMessagesPanel {...defaultProps} />);

    fireEvent.click(screen.getByLabelText('Close'));
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('shows encrypted message placeholder for encrypted pins', async () => {
    mockGetChannelPins.mockResolvedValue([{ ...mockPins[0], content: 'ciphertext' }]);

    render(<PinnedMessagesPanel {...defaultProps} />);

    await waitFor(() => {
      expect(screen.getByText('Encrypted message')).toBeInTheDocument();
    });
  });

  // Truncation test removed in #805: the new PinContent component delegates
  // long-text handling to MarkdownContent (which respects markdown boundaries)
  // and the panel's CSS height clamp, rather than a fixed character cap that
  // would corrupt markdown formatting at the cut point.

  describe('E2EE decryption', () => {
    const encryptedPin: MessageWithUser = {
      id: 'pin-enc-1',
      channel_id: 'channel-1',
      user_id: 'user-1',
      content: 'ciphertext-abc',
      username: 'testuser',
      display_name: 'Test User',
      created_at: '2025-01-01T12:00:00Z',
      updated_at: '2025-01-01T12:00:00Z',
      pinned_at: '2025-01-01T13:00:00Z',
      pinned_by: 'user-1',
    };

    it('decrypts encrypted pinned messages when E2EE is initialized', async () => {
      await enableE2EE();
      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      mockDecryptWithKey.mockResolvedValue('Decrypted secret message');
      mockGetChannelPins.mockResolvedValue([encryptedPin]);

      render(<PinnedMessagesPanel {...defaultProps} />);

      await waitFor(() => {
        expect(screen.getByText('Decrypted secret message')).toBeInTheDocument();
      });

      expect(mockGetChannelKey).toHaveBeenCalledWith('channel-1');
      expect(mockDecryptWithKey).toHaveBeenCalledWith('ciphertext-abc', mockKey);
    });

    it('shows "Unable to decrypt" when decryption fails', async () => {
      await enableE2EE();
      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      mockDecryptWithKey.mockRejectedValue(new Error('Decryption failed'));
      mockGetChannelPins.mockResolvedValue([encryptedPin]);

      render(<PinnedMessagesPanel {...defaultProps} />);

      await waitFor(() => {
        expect(screen.getByText('Unable to decrypt')).toBeInTheDocument();
      });
    });

    it('handles versioned keys for historical messages', async () => {
      await enableE2EE();
      const versionedPin: MessageWithUser = {
        ...encryptedPin,
        id: 'pin-enc-v3',
        content: 'ciphertext-v3',
        key_version: 3,
      };
      const mockVersionedKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockGetChannelKeyByVersion.mockResolvedValue(mockVersionedKey);
      mockDecryptWithKey.mockResolvedValue('Decrypted versioned message');
      mockGetChannelPins.mockResolvedValue([versionedPin]);

      render(<PinnedMessagesPanel {...defaultProps} />);

      await waitFor(() => {
        expect(screen.getByText('Decrypted versioned message')).toBeInTheDocument();
      });

      expect(mockGetChannelKeyByVersion).toHaveBeenCalledWith('channel-1', 3);
      expect(mockDecryptWithKey).toHaveBeenCalledWith('ciphertext-v3', mockVersionedKey);
    });

    it('falls back to decryptForChannel when channelKey fetch fails', async () => {
      await enableE2EE();
      mockGetChannelKey.mockRejectedValue(new Error('Key not available'));
      mockDecryptForChannel.mockResolvedValue('Fallback decrypted message');
      mockGetChannelPins.mockResolvedValue([encryptedPin]);

      render(<PinnedMessagesPanel {...defaultProps} />);

      await waitFor(() => {
        expect(screen.getByText('Fallback decrypted message')).toBeInTheDocument();
      });

      expect(mockDecryptForChannel).toHaveBeenCalledWith('channel-1', 'ciphertext-abc');
    });
  });

  it('hides Unpin button when canPin is false', async () => {
    await enableE2EE();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey
      .mockResolvedValueOnce('Pinned message one')
      .mockResolvedValueOnce('Pinned message two');

    render(<PinnedMessagesPanel {...defaultProps} canPin={false} />);

    await waitFor(() => {
      expect(screen.getByText('Pinned message one')).toBeInTheDocument();
    });

    expect(screen.queryByText('Unpin')).not.toBeInTheDocument();
    // Jump buttons should still be present
    expect(screen.getAllByText('Jump')).toHaveLength(2);
  });

  it('shows Unpin button when canPin is true', async () => {
    await enableE2EE();
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey
      .mockResolvedValueOnce('Pinned message one')
      .mockResolvedValueOnce('Pinned message two');

    render(<PinnedMessagesPanel {...defaultProps} canPin={true} />);

    await waitFor(() => {
      expect(screen.getByText('Pinned message one')).toBeInTheDocument();
    });

    expect(screen.getAllByText('Unpin')).toHaveLength(2);
  });
});
