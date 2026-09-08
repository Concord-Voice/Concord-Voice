import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import DMPinnedMessagesPanel from '@/renderer/components/DirectMessages/DMPinnedMessagesPanel';
import { vi } from 'vitest';
import type { MessageWithUser } from '@/renderer/types/chat';
import { resetAllStores } from '../../../helpers/store-helpers';

const mockGetPins = vi.fn();
const mockUnpinMessage = vi.fn();
vi.mock('@/renderer/services/messaging/pinService', () => ({
  getPins: (...args: unknown[]) => mockGetPins(...args),
  unpinMessage: (...args: unknown[]) => mockUnpinMessage(...args),
  getChannelPins: (...args: unknown[]) => mockGetPins(...args),
}));

const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
const mockDecryptWithKey = vi.fn();
const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();
const mockOperationGuard = { assertCurrent: vi.fn() };

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    createChannelOperationGuard: vi.fn(() => mockOperationGuard),
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

async function setE2EEInitialized(value: boolean) {
  const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
  Object.defineProperty(e2eeService, 'isInitialized', { value, writable: true });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
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
    resetAllStores();
    vi.clearAllMocks();
    mockGetPins.mockReset();
    mockUnpinMessage.mockReset();
    mockGetChannelKey.mockReset();
    mockGetChannelKeyByVersion.mockReset();
    mockDecryptWithKey.mockReset();
    mockDecryptForChannel.mockReset();
    mockDecryptForChannelWithVersion.mockReset();
    mockOperationGuard.assertCurrent.mockReset();
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

  it.each([
    ['matching conversation', 'conv-1'],
    ['server-wide purge', null],
  ] as const)('invalidates stale pin loads for a %s purge', async (_label, scopeId) => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockResolvedValue('fresh DM pin');
    const oldLoad = deferred<MessageWithUser[]>();
    const freshLoad = deferred<MessageWithUser[]>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(freshLoad.promise);
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(1));

    act(() => {
      globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId } }));
    });
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    expect(screen.getByText('Loading...')).toBeInTheDocument();

    await act(async () => oldLoad.resolve([{ ...mockDMPins[0], content: 'stale DM pin' }]));
    expect(screen.queryByText('stale DM pin')).not.toBeInTheDocument();
    expect(screen.getByText('Loading...')).toBeInTheDocument();

    await act(async () => freshLoad.resolve([]));
    await waitFor(() =>
      expect(screen.getByText('No pinned messages in this conversation.')).toBeInTheDocument()
    );
  });

  it('ignores an unrelated explicit purge scope', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockResolvedValue('loaded DM pin');
    const oldLoad = deferred<MessageWithUser[]>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(oldLoad.promise);
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(1));
    for (const detail of [{ scopeId: 'other-conversation' }, { scopeId: undefined }, {}]) {
      act(() => globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail })));
      expect(mockGetPins).toHaveBeenCalledTimes(1);
    }
    await act(async () => oldLoad.resolve(mockDMPins.slice(0, 1)));
    await waitFor(() => expect(screen.getByText('loaded DM pin')).toBeInTheDocument());
  });

  it('clears loaded pins immediately on purge', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    const freshLoad = deferred<MessageWithUser[]>();
    mockGetPins.mockReset();
    mockGetPins
      .mockReturnValueOnce(Promise.resolve(mockDMPins.slice(0, 1)))
      .mockReturnValueOnce(freshLoad.promise);
    mockDecryptWithKey.mockResolvedValue('old DM pin');
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(screen.getByText('old DM pin')).toBeInTheDocument());
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    expect(screen.queryByText('old DM pin')).not.toBeInTheDocument();
    expect(screen.getByText('Loading...')).toBeInTheDocument();
    await act(async () => freshLoad.resolve([]));
  });

  it('ignores a decrypt completion that follows a matching purge', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    const oldDecrypt = deferred<string>();
    const freshLoad = deferred<MessageWithUser[]>();
    mockGetPins.mockReset();
    mockGetPins
      .mockReturnValueOnce(Promise.resolve(mockDMPins.slice(0, 1)))
      .mockReturnValueOnce(freshLoad.promise);
    mockDecryptWithKey.mockReturnValueOnce(oldDecrypt.promise).mockResolvedValue('fresh DM pin');
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    await waitFor(() => expect(mockDecryptWithKey).toHaveBeenCalledTimes(1));

    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    await act(async () => {
      oldDecrypt.resolve('stale DM pin');
      await Promise.resolve();
    });
    expect(screen.queryByText('stale DM pin')).not.toBeInTheDocument();
    expect(screen.getByText('Loading...')).toBeInTheDocument();

    await act(async () => freshLoad.resolve(mockDMPins.slice(0, 1)));
    await waitFor(() => expect(screen.getByText('fresh DM pin')).toBeInTheDocument());
  });

  it('keeps fresh pins when a stale request rejects afterward', async () => {
    await setE2EEInitialized(true);
    mockGetChannelKey.mockResolvedValue({} as CryptoKey);
    mockDecryptWithKey.mockResolvedValue('Pinned DM one');
    const oldLoad = deferred<MessageWithUser[]>();
    const freshLoad = deferred<MessageWithUser[]>();
    mockGetPins.mockReset();
    mockGetPins.mockReturnValueOnce(oldLoad.promise).mockReturnValueOnce(freshLoad.promise);
    render(<DMPinnedMessagesPanel {...defaultProps} />);
    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await waitFor(() => expect(mockGetPins).toHaveBeenCalledTimes(2));
    await act(async () => freshLoad.resolve(mockDMPins.slice(0, 1)));
    await waitFor(() => expect(screen.getByText('Pinned DM one')).toBeInTheDocument());
    await act(async () => oldLoad.reject(new Error('stale')));
    expect(screen.getByText('Pinned DM one')).toBeInTheDocument();
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

  it('does not call onUnpin when a matching purge precedes unpin completion', async () => {
    const onUnpin = vi.fn();
    const pendingUnpin = deferred<unknown>();
    mockUnpinMessage.mockReturnValue(pendingUnpin.promise);
    render(<DMPinnedMessagesPanel {...defaultProps} onUnpin={onUnpin} />);
    await waitFor(() => expect(screen.getAllByText('Unpin')).toHaveLength(2));
    fireEvent.click(screen.getAllByText('Unpin')[0]);
    await waitFor(() => expect(mockUnpinMessage).toHaveBeenCalledWith('dm-pin-1'));

    act(() =>
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: 'conv-1' } })
      )
    );
    await act(async () => pendingUnpin.resolve({}));
    expect(onUnpin).not.toHaveBeenCalled();
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
      expect(mockDecryptWithKey).toHaveBeenCalledWith('ciphertext-abc', key, mockOperationGuard);
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
      expect(mockDecryptWithKey).toHaveBeenCalledWith('ciphertext-v3', vKey, mockOperationGuard);
    });
  });
});
