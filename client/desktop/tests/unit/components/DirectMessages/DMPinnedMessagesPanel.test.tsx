import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import DMPinnedMessagesPanel from '@/renderer/components/DirectMessages/DMPinnedMessagesPanel';
import { vi } from 'vitest';
import type { MessageWithUser } from '@/renderer/types/chat';

const mockGetPins = vi.fn();
const mockUnpinMessage = vi.fn();
vi.mock('@/renderer/services/pinService', () => ({
  getPins: (...args: unknown[]) => mockGetPins(...args),
  unpinMessage: (...args: unknown[]) => mockUnpinMessage(...args),
  getChannelPins: (...args: unknown[]) => mockGetPins(...args),
}));

const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
const mockDecryptWithKey = vi.fn();
const mockDecryptForChannel = vi.fn();
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

async function setE2EEInitialized(value: boolean) {
  const { e2eeService } = await import('@/renderer/services/e2eeService');
  Object.defineProperty(e2eeService, 'isInitialized', { value, writable: true });
}

const mockDMPins: MessageWithUser[] = [
  {
    id: 'dm-pin-1',
    channel_id: 'conv-1',
    user_id: 'user-1',
    content: 'Pinned DM one',
    username: 'alice',
    display_name: 'Alice',
    created_at: '2025-01-01T12:00:00Z',
    updated_at: '2025-01-01T12:00:00Z',
    pinned_at: '2025-01-01T13:00:00Z',
    pinned_by: 'user-1',
  },
  {
    id: 'dm-pin-2',
    channel_id: 'conv-1',
    user_id: 'user-2',
    content: 'Pinned DM two',
    username: 'bob',
    created_at: '2025-01-01T12:01:00Z',
    updated_at: '2025-01-01T12:01:00Z',
    pinned_at: '2025-01-01T13:01:00Z',
    pinned_by: 'user-1',
  },
];

describe('DMPinnedMessagesPanel', () => {
  const defaultProps = {
    conversationId: 'conv-1',
    isOpen: true,
    onClose: vi.fn(),
    onScrollToMessage: vi.fn(),
    canPin: true,
  };

  beforeEach(async () => {
    vi.clearAllMocks();
    await setE2EEInitialized(false);
    mockGetPins.mockResolvedValue(mockDMPins);
    mockUnpinMessage.mockResolvedValue({ message_id: 'dm-pin-1' });
  });

  it('returns null when not open', () => {
    const { container } = render(<DMPinnedMessagesPanel {...defaultProps} isOpen={false} />);
    expect(container.querySelector('.pinned-panel-backdrop')).not.toBeInTheDocument();
  });

  it('fetches pins with the DM conversation id', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey
      .mockResolvedValueOnce('Pinned DM one')
      .mockResolvedValueOnce('Pinned DM two');
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => {
      expect(mockGetPins).toHaveBeenCalledWith('conv-1');
    });
    await waitFor(() => {
      expect(screen.getByText('Pinned DM one')).toBeInTheDocument();
      expect(screen.getByText('Pinned DM two')).toBeInTheDocument();
    });
  });

  it('shows empty state with DM-specific copy when no pins', async () => {
    mockGetPins.mockResolvedValue([]);
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => {
      expect(screen.getByText('No pinned messages in this conversation.')).toBeInTheDocument();
    });
  });

  it('degrades gracefully when the service throws (e.g. backend 404)', async () => {
    mockGetPins.mockRejectedValue(new Error('not found'));
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => {
      expect(screen.getByText('No pinned messages in this conversation.')).toBeInTheDocument();
    });
  });

  it('calls onScrollToMessage and onClose when Jump is clicked', async () => {
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(screen.getAllByText('Jump')).toHaveLength(2));
    fireEvent.click(screen.getAllByText('Jump')[0]);
    expect(defaultProps.onScrollToMessage).toHaveBeenCalledWith('dm-pin-1');
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('calls unpinMessage and removes the pin from the list', async () => {
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(screen.getAllByText('Unpin')).toHaveLength(2));
    fireEvent.click(screen.getAllByText('Unpin')[0]);
    await waitFor(() => expect(mockUnpinMessage).toHaveBeenCalledWith('dm-pin-1'));
    await waitFor(() => expect(screen.getAllByText('Unpin')).toHaveLength(1));
  });

  it('calls onUnpin callback after successfully unpinning a message', async () => {
    const onUnpin = vi.fn();
    render(<DMPinnedMessagesPanel {...defaultProps} onUnpin={onUnpin} />);
    await waitFor(() => expect(screen.getAllByText('Unpin')).toHaveLength(2));
    fireEvent.click(screen.getAllByText('Unpin')[0]);
    await waitFor(() => expect(mockUnpinMessage).toHaveBeenCalledWith('dm-pin-1'));
    await waitFor(() => expect(onUnpin).toHaveBeenCalledTimes(1));
  });

  it('does not call onUnpin when unpin fails', async () => {
    const onUnpin = vi.fn();
    mockUnpinMessage.mockRejectedValue(new Error('nope'));
    render(<DMPinnedMessagesPanel {...defaultProps} onUnpin={onUnpin} />);
    await waitFor(() => expect(screen.getAllByText('Unpin')).toHaveLength(2));
    fireEvent.click(screen.getAllByText('Unpin')[0]);
    await waitFor(() => expect(mockUnpinMessage).toHaveBeenCalled());
    expect(onUnpin).not.toHaveBeenCalled();
  });

  it('swallows unpin errors silently', async () => {
    mockUnpinMessage.mockRejectedValue(new Error('nope'));
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(screen.getAllByText('Unpin')).toHaveLength(2));
    fireEvent.click(screen.getAllByText('Unpin')[0]);
    await waitFor(() => expect(mockUnpinMessage).toHaveBeenCalled());
    // Pins list unchanged (no exception thrown)
    expect(screen.getAllByText('Unpin')).toHaveLength(2);
  });

  it('hides Unpin buttons when canPin is false', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey
      .mockResolvedValueOnce('Pinned DM one')
      .mockResolvedValueOnce('Pinned DM two');
    render(<DMPinnedMessagesPanel {...defaultProps} canPin={false} />);
    await waitFor(() => expect(screen.getByText('Pinned DM one')).toBeInTheDocument());
    expect(screen.queryByText('Unpin')).not.toBeInTheDocument();
  });

  it('calls onClose when close button is clicked', async () => {
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    fireEvent.click(screen.getByLabelText('Close'));
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('calls onClose when backdrop dismiss is clicked', async () => {
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    const dismiss = document.querySelector('.pinned-panel-backdrop-dismiss') as HTMLElement;
    fireEvent.click(dismiss);
    expect(defaultProps.onClose).toHaveBeenCalled();
  });

  it('shows encrypted placeholder when E2EE is not initialized', async () => {
    mockGetPins.mockResolvedValue([{ ...mockDMPins[0], content: 'ciphertext' }]);
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(screen.getByText('Encrypted message')).toBeInTheDocument());
  });

  // Truncation test removed in #805: see PinnedMessagesPanel.test.tsx for
  // the same rationale — PinContent delegates long-text handling to
  // MarkdownContent + CSS rather than a fixed 200-char cap.

  describe('E2EE decryption', () => {
    const encryptedPin: MessageWithUser = {
      id: 'dm-enc-1',
      channel_id: 'conv-1',
      user_id: 'user-1',
      content: 'ciphertext-abc',
      username: 'alice',
      display_name: 'Alice',
      created_at: '2025-01-01T12:00:00Z',
      updated_at: '2025-01-01T12:00:00Z',
      pinned_at: '2025-01-01T13:00:00Z',
      pinned_by: 'user-1',
    };

    beforeEach(async () => {
      await setE2EEInitialized(true);
    });

    it('decrypts with the conversation key', async () => {
      const key = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(key);
      mockDecryptWithKey.mockResolvedValue('Hello private world');
      mockGetPins.mockResolvedValue([encryptedPin]);

      render(<DMPinnedMessagesPanel {...defaultProps} />);
      await waitFor(() => expect(screen.getByText('Hello private world')).toBeInTheDocument());
      expect(mockGetChannelKey).toHaveBeenCalledWith('conv-1');
      expect(mockDecryptWithKey).toHaveBeenCalledWith('ciphertext-abc', key);
    });

    it('falls back to decryptForChannel when channel key fetch fails', async () => {
      mockGetChannelKey.mockRejectedValue(new Error('no key'));
      mockDecryptForChannel.mockResolvedValue('Fallback plaintext');
      mockGetPins.mockResolvedValue([encryptedPin]);

      render(<DMPinnedMessagesPanel {...defaultProps} />);
      await waitFor(() => expect(screen.getByText('Fallback plaintext')).toBeInTheDocument());
      expect(mockDecryptForChannel).toHaveBeenCalledWith('conv-1', 'ciphertext-abc');
    });

    it('shows "Unable to decrypt" when decryption throws', async () => {
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockDecryptWithKey.mockRejectedValue(new Error('bad tag'));
      mockGetPins.mockResolvedValue([encryptedPin]);

      render(<DMPinnedMessagesPanel {...defaultProps} />);
      await waitFor(() => expect(screen.getByText('Unable to decrypt')).toBeInTheDocument());
    });

    it('uses versioned key lookup for historical messages', async () => {
      const versioned: MessageWithUser = {
        ...encryptedPin,
        id: 'dm-enc-v3',
        content: 'ciphertext-v3',
        key_version: 3,
      };
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      const vKey = {} as CryptoKey;
      mockGetChannelKeyByVersion.mockResolvedValue(vKey);
      mockDecryptWithKey.mockResolvedValue('Decrypted v3 message');
      mockGetPins.mockResolvedValue([versioned]);

      render(<DMPinnedMessagesPanel {...defaultProps} />);
      await waitFor(() => expect(screen.getByText('Decrypted v3 message')).toBeInTheDocument());
      expect(mockGetChannelKeyByVersion).toHaveBeenCalledWith('conv-1', 3);
      expect(mockDecryptWithKey).toHaveBeenCalledWith('ciphertext-v3', vKey);
    });
  });
});
